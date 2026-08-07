package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clienttesting "k8s.io/client-go/testing"

	"github.com/gmalfray/vcluster-manager/internal/config"
	"github.com/gmalfray/vcluster-manager/internal/kubernetes"
	"github.com/gmalfray/vcluster-manager/internal/models"
)

func adminActor() models.Actor { return models.Actor{Username: "alice", IsAdmin: true} }
func plainActor() models.Actor { return models.Actor{Username: "bob", IsAdmin: false} }

// --- validBackupName ---

func TestValidBackupName(t *testing.T) {
	tests := []struct {
		name   string
		backup string
		want   bool
	}{
		{"manual backup name", "manual-demo-1717000000000", true},
		{"schedule-generated name", "daily-20230101020000", true},
		{"single char", "a", true},
		{"with dots", "backup.v1", true},
		{"empty rejected", "", false},
		{"path traversal", "../etc/passwd", false},
		{"embedded slash", "foo/bar", false},
		{"embedded newline", "foo\nbar", false},
		{"starts with dash", "-backup", false},
		{"uppercase rejected", "Manual-Demo", false},
		{"with spaces", "backup name", false},
		{"quote injection", `backup" \n evil: true`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := validBackupName(tt.backup); got != tt.want {
				t.Errorf("validBackupName(%q) = %v, want %v", tt.backup, got, tt.want)
			}
		})
	}
}

// --- IsTerminalRestorePhase ---

func TestIsTerminalRestorePhase(t *testing.T) {
	tests := []struct {
		phase string
		want  bool
	}{
		{"Completed", true},
		{"Failed", true},
		{"PartiallyFailed", true},
		{"New", false},
		{"InProgress", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := IsTerminalRestorePhase(tt.phase); got != tt.want {
			t.Errorf("IsTerminalRestorePhase(%q) = %v, want %v", tt.phase, got, tt.want)
		}
	}
}

// --- shared test fixtures ---

// newVeleroTestService builds a Service around a StatusClient, registered
// under "preprod" so envOrDefault's fallback resolves to it.
func newVeleroTestService(k8s *kubernetes.StatusClient) *Service {
	var mu sync.RWMutex
	clients := map[string]*kubernetes.StatusClient{}
	if k8s != nil {
		clients["preprod"] = k8s
	}
	return New(Deps{
		Cfg:          &config.Config{VeleroNamespace: "velero-system"},
		K8sClients:   clients,
		K8sClientsMu: &mu,
	})
}

func newBackupObj(name, namespace, phase string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "velero.io/v1",
		"kind":       "Backup",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": namespace,
		},
		"status": map[string]interface{}{
			"phase": phase,
		},
	}}
}

func newPVCObj(name, namespace string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": namespace,
		},
	}}
}

func newStatefulSetObj(name, namespace string, replicas int64) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "StatefulSet",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": namespace,
		},
		"spec": map[string]interface{}{
			"replicas": replicas,
		},
	}}
}

func newHelmReleaseObj(name, namespace string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "helm.toolkit.fluxcd.io/v2",
		"kind":       "HelmRelease",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": namespace,
		},
		"spec": map[string]interface{}{},
	}}
}

func newKustomizationObj(name, namespace string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kustomize.toolkit.fluxcd.io/v1",
		"kind":       "Kustomization",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": namespace,
		},
		"spec": map[string]interface{}{},
	}}
}

func newRestoreObj(name, namespace, phase string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "velero.io/v1",
		"kind":       "Restore",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": namespace,
		},
		"status": map[string]interface{}{
			"phase": phase,
		},
	}}
}

// --- CreateVeleroRestore: RBAC and input validation ---

func TestCreateVeleroRestore_ForbiddenForNonAdmin(t *testing.T) {
	s := newVeleroTestService(nil)
	_, err := s.CreateVeleroRestore(context.Background(), plainActor(), "demo", "preprod", "manual-demo-1", "")
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected ErrForbidden for non-admin, got %v", err)
	}
}

func TestCreateVeleroRestore_RequiresBackupName(t *testing.T) {
	s := newVeleroTestService(nil)
	_, err := s.CreateVeleroRestore(context.Background(), adminActor(), "demo", "preprod", "", "")
	if !errors.Is(err, ErrBackupNameRequired) {
		t.Fatalf("expected ErrBackupNameRequired for an empty backup name, got %v", err)
	}
}

func TestCreateVeleroRestore_RejectsInvalidBackupName(t *testing.T) {
	s := newVeleroTestService(nil)
	_, err := s.CreateVeleroRestore(context.Background(), adminActor(), "demo", "preprod", "../etc/passwd", "")
	if !errors.Is(err, ErrInvalidBackupName) {
		t.Fatalf("expected ErrInvalidBackupName, got %v", err)
	}
}

func TestCreateVeleroRestore_RejectsInvalidTargetName(t *testing.T) {
	s := newVeleroTestService(nil)
	_, err := s.CreateVeleroRestore(context.Background(), adminActor(), "demo", "preprod", "manual-demo-1", "../etc/passwd")
	if !errors.Is(err, ErrInvalidName) {
		t.Fatalf("expected ErrInvalidName, got %v", err)
	}
}

func TestCreateVeleroRestore_AllowsEmptyTargetName(t *testing.T) {
	// Empty target means "in-place restore of the same vcluster" — not a name
	// to validate, so it must fall through to the next guard (no k8s client
	// configured here), not ErrInvalidName.
	s := newVeleroTestService(nil)
	_, err := s.CreateVeleroRestore(context.Background(), adminActor(), "demo", "preprod", "manual-demo-1", "")
	if !errors.Is(err, ErrK8sUnavailable) {
		t.Fatalf("expected ErrK8sUnavailable (empty target should not trip ErrInvalidName), got %v", err)
	}
}

func TestCreateVeleroRestore_K8sUnavailable(t *testing.T) {
	s := newVeleroTestService(nil)
	_, err := s.CreateVeleroRestore(context.Background(), adminActor(), "demo", "preprod", "manual-demo-1", "")
	if !errors.Is(err, ErrK8sUnavailable) {
		t.Fatalf("expected ErrK8sUnavailable, got %v", err)
	}
}

// --- CreateVeleroRestore: the data-safety guard ---
//
// An in-place restore deletes the vcluster's PVC so Velero can recreate it
// from the backup. That must never happen unless the backup's phase is
// Completed — these tests seed the PVC and StatefulSet up front and check
// they're untouched when the guard rejects the restore.

func TestCreateVeleroRestore_BlocksInPlaceRestoreWhenBackupNotCompleted(t *testing.T) {
	k8s := kubernetes.NewTestStatusClient(
		newBackupObj("manual-demo-1", "velero-system", "Failed"),
		newPVCObj("data-vcluster-demo-0", "vcluster-demo"),
		newStatefulSetObj("vcluster-demo", "vcluster-demo", 1),
	)
	s := newVeleroTestService(k8s)

	_, err := s.CreateVeleroRestore(context.Background(), adminActor(), "demo", "preprod", "manual-demo-1", "")

	var notRestorable *ErrBackupNotRestorable
	if !errors.As(err, &notRestorable) {
		t.Fatalf("expected *ErrBackupNotRestorable, got %v", err)
	}
	if notRestorable.Phase != "Failed" {
		t.Errorf("expected reported phase %q, got %q", "Failed", notRestorable.Phase)
	}

	// The guard must have run before anything destructive: deleting the PVC
	// now must still succeed, proving CreateVeleroRestore never touched it.
	if err := k8s.DeleteVClusterPVC(context.Background(), "demo"); err != nil {
		t.Fatalf("expected the PVC to still exist untouched by the blocked restore: %v", err)
	}
}

func TestCreateVeleroRestore_UnknownBackupBlocksTheRestore(t *testing.T) {
	// No Backup object seeded at all: GetVeleroBackupPhase fails before any
	// destructive step runs.
	k8s := kubernetes.NewTestStatusClient(
		newPVCObj("data-vcluster-demo-0", "vcluster-demo"),
		newStatefulSetObj("vcluster-demo", "vcluster-demo", 1),
	)
	s := newVeleroTestService(k8s)

	_, err := s.CreateVeleroRestore(context.Background(), adminActor(), "demo", "preprod", "manual-demo-missing", "")

	if !errors.Is(err, ErrBackupLookupFailed) {
		t.Fatalf("expected ErrBackupLookupFailed, got %v", err)
	}

	if err := k8s.DeleteVClusterPVC(context.Background(), "demo"); err != nil {
		t.Fatalf("expected the PVC to still exist untouched by the blocked restore: %v", err)
	}
}

// --- TriggerVeleroBackup / DeleteVeleroBackup: RBAC ---

func TestTriggerVeleroBackup_ForbiddenForNonAdmin(t *testing.T) {
	s := newVeleroTestService(nil)
	_, err := s.TriggerVeleroBackup(context.Background(), plainActor(), "demo", "preprod")
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected ErrForbidden for non-admin, got %v", err)
	}
}

func TestDeleteVeleroBackup_ForbiddenForNonAdmin(t *testing.T) {
	s := newVeleroTestService(nil)
	_, err := s.DeleteVeleroBackup(context.Background(), plainActor(), "demo", "manual-demo-1", "preprod")
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected ErrForbidden for non-admin, got %v", err)
	}
}

func TestDeleteVeleroBackup_RejectsInvalidBackupNameBeforeK8sLookup(t *testing.T) {
	// No client configured: if the name check ran after the k8s lookup this
	// would fail with ErrK8sUnavailable instead.
	s := newVeleroTestService(nil)
	_, err := s.DeleteVeleroBackup(context.Background(), adminActor(), "demo", "../etc/passwd", "preprod")
	if !errors.Is(err, ErrInvalidBackupName) {
		t.Fatalf("expected ErrInvalidBackupName, got %v", err)
	}
}

// --- GetVeleroBackups / GetVeleroBackupContent: read-only availability ---

func TestGetVeleroBackups_K8sUnavailable(t *testing.T) {
	s := newVeleroTestService(nil)
	_, err := s.GetVeleroBackups(context.Background(), "demo", "preprod")
	if !errors.Is(err, ErrK8sUnavailable) {
		t.Fatalf("expected ErrK8sUnavailable, got %v", err)
	}
}

func TestGetVeleroBackupContent_RejectsInvalidBackupName(t *testing.T) {
	s := newVeleroTestService(nil)
	_, err := s.GetVeleroBackupContent(context.Background(), adminActor(), "demo", "../etc/passwd", "preprod")
	if !errors.Is(err, ErrInvalidBackupName) {
		t.Fatalf("expected ErrInvalidBackupName, got %v", err)
	}
}

// --- GetVeleroBackupContent: RBAC -------------------------------------------
//
// The backup content is the tenant's raw resource dump, secrets included —
// unlike GetVeleroBackups (metadata only), a plain reader must not see it.

func TestGetVeleroBackupContent_ForbiddenForNonAdmin(t *testing.T) {
	s := newVeleroTestService(nil)
	_, err := s.GetVeleroBackupContent(context.Background(), plainActor(), "demo", "manual-demo-1", "preprod")
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected ErrForbidden for non-admin, got %v", err)
	}
}

func TestGetVeleroBackupContent_ForbiddenCheckedBeforeBackupName(t *testing.T) {
	// The admin check must run first: a non-admin probing an invalid name
	// should still get ErrForbidden, not a hint about name validation.
	s := newVeleroTestService(nil)
	_, err := s.GetVeleroBackupContent(context.Background(), plainActor(), "demo", "../etc/passwd", "preprod")
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected ErrForbidden to be checked before the backup name, got %v", err)
	}
}

// --- CreateVeleroRestore: stage failures abort cleanly ----------------------
//
// Before the RBAC/topology fixes, a failed suspend/scale/delete stage was
// only logged — CreateVeleroRestore pressed on to create the Restore object
// regardless, and the UI ended up claiming success. These tests check the
// opposite: a failed stage stops the sequence right there and comes back as
// an error the adapter can show.

func TestCreateVeleroRestore_AbortsWhenSuspendFluxFails(t *testing.T) {
	// No HelmRelease/Kustomization seeded: SetFluxSuspend fails on the very
	// first patch, before anything destructive runs.
	k8s := kubernetes.NewTestStatusClient(
		newBackupObj("manual-demo-1", "velero-system", "Completed"),
	)
	s := newVeleroTestService(k8s)

	_, err := s.CreateVeleroRestore(context.Background(), adminActor(), "demo", "preprod", "manual-demo-1", "")

	if !errors.Is(err, ErrRestoreStageFailed) {
		t.Fatalf("expected ErrRestoreStageFailed, got %v", err)
	}
	if !strings.Contains(err.Error(), "suspension de Flux") {
		t.Errorf("expected the error to name the failed stage, got %q", err.Error())
	}
}

func TestCreateVeleroRestore_AbortsWhenScaleDownFails(t *testing.T) {
	// Flux suspend succeeds, but there's no control-plane workload to scale.
	k8s := kubernetes.NewTestStatusClient(
		newBackupObj("manual-demo-1", "velero-system", "Completed"),
		newHelmReleaseObj("vcluster-demo", "vcluster-demo"),
		newKustomizationObj("tenant-demo", "flux-system"),
	)
	s := newVeleroTestService(k8s)

	_, err := s.CreateVeleroRestore(context.Background(), adminActor(), "demo", "preprod", "manual-demo-1", "")

	if !errors.Is(err, ErrRestoreStageFailed) {
		t.Fatalf("expected ErrRestoreStageFailed, got %v", err)
	}
	if !strings.Contains(err.Error(), "échelle") {
		t.Errorf("expected the error to name the failed stage, got %q", err.Error())
	}
}

// Une suppression de PVC qui échoue POUR UNE AUTRE RAISON que « absent » laisse
// le volume intact : il faut abandonner et reprendre Flux.
func TestCreateVeleroRestore_AbortsWhenPVCDeleteFails(t *testing.T) {
	refus := func(action clienttesting.Action) (bool, runtime.Object, error) {
		if action.GetVerb() == "delete" && action.GetResource().Resource == "persistentvolumeclaims" {
			return true, nil, apierrors.NewForbidden(
				schema.GroupResource{Resource: "persistentvolumeclaims"}, "data-vcluster-demo-0",
				fmt.Errorf("refusé par un webhook"))
		}
		return false, nil, nil
	}
	k8s := kubernetes.NewTestStatusClientWithReactor(refus,
		newBackupObj("manual-demo-1", "velero-system", "Completed"),
		newHelmReleaseObj("vcluster-demo", "vcluster-demo"),
		newKustomizationObj("tenant-demo", "flux-system"),
		newStatefulSetObj("vcluster-demo", "vcluster-demo", 1),
		newPVCObj("data-vcluster-demo-0", "vcluster-demo"),
	)
	s := newVeleroTestService(k8s)

	_, err := s.CreateVeleroRestore(context.Background(), adminActor(), "demo", "preprod", "manual-demo-1", "")

	if !errors.Is(err, ErrRestoreStageFailed) {
		t.Fatalf("expected ErrRestoreStageFailed, got %v", err)
	}
	if !strings.Contains(err.Error(), "suppression du volume") {
		t.Errorf("expected the error to name the failed stage, got %q", err.Error())
	}
}

// Le volume DÉJÀ absent n'est pas un échec : c'est le rejeu après un
// ErrRestoreStageFailedVolumeGone, que le message d'erreur recommande. Le
// traiter comme un échec faisait reprendre Flux sur un vcluster sans volume,
// donc recréer un PVC vide — précisément ce que la sentinelle interdit.
func TestCreateVeleroRestore_AlreadyDeletedVolumeIsNotAFailure(t *testing.T) {
	var mu sync.Mutex
	helmReleasePatches := 0
	compteur := func(action clienttesting.Action) (bool, runtime.Object, error) {
		if action.GetVerb() == "patch" && action.GetResource().Resource == "helmreleases" {
			mu.Lock()
			helmReleasePatches++
			mu.Unlock()
		}
		return false, nil, nil
	}
	// Aucun PVC semé : la suppression renverra NotFound.
	k8s := kubernetes.NewTestStatusClientWithReactor(compteur,
		newBackupObj("manual-demo-1", "velero-system", "Completed"),
		newHelmReleaseObj("vcluster-demo", "vcluster-demo"),
		newKustomizationObj("tenant-demo", "flux-system"),
		newStatefulSetObj("vcluster-demo", "vcluster-demo", 1),
	)
	s := newVeleroTestService(k8s)

	view, err := s.CreateVeleroRestoreUnwatched(context.Background(), adminActor(), "demo", "preprod", "manual-demo-1", "")
	if err != nil {
		t.Fatalf("le rejeu a échoué alors que le volume était déjà parti : %v", err)
	}
	if view.RestoreName == "" {
		t.Fatal("aucun restore créé")
	}
	mu.Lock()
	defer mu.Unlock()
	// Un seul patch = SetFluxSuspend(true). Un second voudrait dire que Flux a
	// été repris, donc qu'un PVC vide va être recréé.
	if helmReleasePatches != 1 {
		t.Fatalf("%d patches sur le HelmRelease : Flux a été repris alors que le volume est parti", helmReleasePatches)
	}
}

func TestCreateVeleroRestore_SucceedsWhenAllStagesSucceed(t *testing.T) {
	k8s := kubernetes.NewTestStatusClient(
		newBackupObj("manual-demo-1", "velero-system", "Completed"),
		newHelmReleaseObj("vcluster-demo", "vcluster-demo"),
		newKustomizationObj("tenant-demo", "flux-system"),
		newStatefulSetObj("vcluster-demo", "vcluster-demo", 1),
		newPVCObj("data-vcluster-demo-0", "vcluster-demo"),
	)
	s := newVeleroTestService(k8s)

	view, err := s.CreateVeleroRestore(context.Background(), adminActor(), "demo", "preprod", "manual-demo-1", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if view.RestoreName == "" {
		t.Error("expected a non-empty restore name")
	}
	if !view.InPlace {
		t.Error("expected InPlace to be true for a same-name restore")
	}
}

// --- CreateVeleroRestore: whether to resume Flux on abort depends on whether
// the PVC was already deleted ------------------------------------------------
//
// Resuming Flux after a stage fails is only safe while the vcluster's volume
// is still intact: once the PVC is gone, resuming lets the StatefulSet
// recreate an empty one and masks the failure. These count the patches made
// to the HelmRelease to tell whether SetFluxSuspend(false) actually ran a
// second time (the abort), on top of the initial SetFluxSuspend(true).

// failCreateReactor makes the fake dynamic client fail every create of the
// given resource (e.g. "restores"), leaving everything else untouched.
func failCreateReactor(resource string) clienttesting.ReactionFunc {
	return func(action clienttesting.Action) (bool, runtime.Object, error) {
		if action.GetVerb() == "create" && action.GetResource().Resource == resource {
			return true, nil, fmt.Errorf("simulated failure creating %s", resource)
		}
		return false, nil, nil
	}
}

func TestCreateVeleroRestore_AbortResumesFluxOnlyBeforeThePVCIsGone(t *testing.T) {
	tests := []struct {
		name              string
		objs              []runtime.Object
		failRestoreCreate bool
		wantErr           error
		wantFluxResumed   bool
	}{
		{
			name: "scale-down fails before the PVC is touched: flux is resumed",
			objs: []runtime.Object{
				newBackupObj("manual-demo-1", "velero-system", "Completed"),
				newHelmReleaseObj("vcluster-demo", "vcluster-demo"),
				newKustomizationObj("tenant-demo", "flux-system"),
				// no StatefulSet: ScaleVClusterWorkloads fails
			},
			wantErr:         ErrRestoreStageFailed,
			wantFluxResumed: true,
		},
		{
			name: "restore creation fails after the PVC is already gone: flux stays suspended",
			objs: []runtime.Object{
				newBackupObj("manual-demo-1", "velero-system", "Completed"),
				newHelmReleaseObj("vcluster-demo", "vcluster-demo"),
				newKustomizationObj("tenant-demo", "flux-system"),
				newStatefulSetObj("vcluster-demo", "vcluster-demo", 1),
				newPVCObj("data-vcluster-demo-0", "vcluster-demo"),
			},
			failRestoreCreate: true,
			wantErr:           ErrRestoreStageFailedVolumeGone,
			wantFluxResumed:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var helmReleasePatches int
			reactor := func(action clienttesting.Action) (bool, runtime.Object, error) {
				if action.GetVerb() == "patch" && action.GetResource().Resource == "helmreleases" {
					helmReleasePatches++
				}
				if tt.failRestoreCreate && action.GetVerb() == "create" && action.GetResource().Resource == "restores" {
					return true, nil, fmt.Errorf("simulated failure creating restore")
				}
				return false, nil, nil
			}
			k8s := kubernetes.NewTestStatusClientWithReactor(reactor, tt.objs...)
			s := newVeleroTestService(k8s)

			_, err := s.CreateVeleroRestore(context.Background(), adminActor(), "demo", "preprod", "manual-demo-1", "")

			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("expected %v, got %v", tt.wantErr, err)
			}
			// One patch is the initial SetFluxSuspend(true); a second one only
			// happens if abortInPlaceRestore ran SetFluxSuspend(false).
			gotResumed := helmReleasePatches >= 2
			if gotResumed != tt.wantFluxResumed {
				t.Errorf("flux resumed = %v (helmrelease patches = %d), want %v", gotResumed, helmReleasePatches, tt.wantFluxResumed)
			}
		})
	}
}

// --- GetVeleroRestoreStatus: a single failed resume attempt is pending, not
// failed outright -------------------------------------------------------
//
// The background watcher (resumeAfterInPlaceRestore) retries the resume
// independently of any polling, so a request-driven poll that fails its own
// attempt must not report ResumeFailed — that would freeze the UI on a
// stale message while the background watcher fixes it moments later. It's
// only a settled failure (ResumeFailed) once something actually gave up,
// which here means the state was resolved that way beforehand (simulating
// what resolveVeleroResume would record after the 2h background timeout).

func TestGetVeleroRestoreStatus_UnresolvedResumeFailureIsPendingNotFailed(t *testing.T) {
	// Restore is Completed, but there's no HelmRelease/Kustomization to
	// resume: this poll's own attempt fails. Nothing has settled the outcome
	// yet, so it must read as "still trying", not "given up".
	k8s := kubernetes.NewTestStatusClient(
		newRestoreObj("vm-manual-demo-1-123", "velero-system", "Completed"),
	)
	s := newVeleroTestService(k8s)

	view, err := s.GetVeleroRestoreStatus(context.Background(), "demo", "vm-manual-demo-1-123", "preprod", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if view.Phase != "Completed" {
		t.Fatalf("Phase = %q, want Completed", view.Phase)
	}
	if view.ResumeFailed {
		t.Error("expected ResumeFailed to stay false for an unsettled single attempt")
	}
	if !view.ResumePending {
		t.Error("expected ResumePending to be true so the UI keeps polling")
	}
}

func TestGetVeleroRestoreStatus_ResumeSuccessIsReportedAndNotPending(t *testing.T) {
	k8s := kubernetes.NewTestStatusClient(
		newRestoreObj("vm-manual-demo-1-123", "velero-system", "Completed"),
		newHelmReleaseObj("vcluster-demo", "vcluster-demo"),
		newKustomizationObj("tenant-demo", "flux-system"),
	)
	s := newVeleroTestService(k8s)

	view, err := s.GetVeleroRestoreStatus(context.Background(), "demo", "vm-manual-demo-1-123", "preprod", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if view.ResumeFailed {
		t.Errorf("expected ResumeFailed to be false, got ResumeError=%q", view.ResumeError)
	}
	if view.ResumePending {
		t.Error("expected ResumePending to be false once the resume actually succeeded")
	}
}

func TestGetVeleroRestoreStatus_NonTerminalPhaseDoesNotAttemptResume(t *testing.T) {
	// Restore still in progress and no HelmRelease/Kustomization seeded: if
	// resume were attempted here it would fail and wrongly set ResumeFailed.
	k8s := kubernetes.NewTestStatusClient(
		newRestoreObj("vm-manual-demo-1-123", "velero-system", "InProgress"),
	)
	s := newVeleroTestService(k8s)

	view, err := s.GetVeleroRestoreStatus(context.Background(), "demo", "vm-manual-demo-1-123", "preprod", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if view.ResumeFailed {
		t.Error("expected ResumeFailed to stay false while the restore is still in progress")
	}
	if view.ResumePending {
		t.Error("expected ResumePending to stay false while the restore is still in progress")
	}
}

func TestGetVeleroRestoreStatus_UsesBackgroundResolvedFailureWithoutRetrying(t *testing.T) {
	// The background watcher has already given up on this restore (recorded
	// via resolveVeleroResume, as it would be after its 2h timeout). No
	// HelmRelease/Kustomization is seeded, so if GetVeleroRestoreStatus made
	// its own attempt instead of trusting the resolved state, the patch
	// count below would catch it.
	k8s := kubernetes.NewTestStatusClient(
		newRestoreObj("vm-manual-demo-1-123", "velero-system", "Completed"),
	)
	s := newVeleroTestService(k8s)
	s.resolveVeleroResume("vm-manual-demo-1-123", true, "boom")

	view, err := s.GetVeleroRestoreStatus(context.Background(), "demo", "vm-manual-demo-1-123", "preprod", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !view.ResumeFailed {
		t.Error("expected the previously resolved failure to be reported")
	}
	if view.ResumeError != "boom" {
		t.Errorf("ResumeError = %q, want %q", view.ResumeError, "boom")
	}
	if view.ResumePending {
		t.Error("expected ResumePending to be false: the outcome is already settled")
	}
}

func TestGetVeleroRestoreStatus_UsesBackgroundResolvedSuccessWithoutRetrying(t *testing.T) {
	k8s := kubernetes.NewTestStatusClient(
		newRestoreObj("vm-manual-demo-1-123", "velero-system", "Completed"),
	)
	s := newVeleroTestService(k8s)
	s.resolveVeleroResume("vm-manual-demo-1-123", false, "")

	view, err := s.GetVeleroRestoreStatus(context.Background(), "demo", "vm-manual-demo-1-123", "preprod", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if view.ResumeFailed || view.ResumePending {
		t.Errorf("expected an already-resolved success to read as resolved, got ResumeFailed=%v ResumePending=%v", view.ResumeFailed, view.ResumePending)
	}
}

// --- GetVeleroRestoreStatus: VolumeDestroyed nuances a Failed in-place
// restore ----------------------------------------------------------------
//
// An in-place restore always deletes the PVC before creating the Restore
// object, so a Failed restore always means the volume is gone — the UI must
// say so instead of just "Flux repris".

func TestGetVeleroRestoreStatus_FailedInPlaceRestoreFlagsVolumeDestroyed(t *testing.T) {
	k8s := kubernetes.NewTestStatusClient(
		newRestoreObj("vm-manual-demo-1-123", "velero-system", "Failed"),
		newHelmReleaseObj("vcluster-demo", "vcluster-demo"),
		newKustomizationObj("tenant-demo", "flux-system"),
	)
	s := newVeleroTestService(k8s)

	view, err := s.GetVeleroRestoreStatus(context.Background(), "demo", "vm-manual-demo-1-123", "preprod", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !view.VolumeDestroyed {
		t.Error("expected VolumeDestroyed to be true for a Failed in-place restore")
	}
}

func TestGetVeleroRestoreStatus_CompletedRestoreDoesNotFlagVolumeDestroyed(t *testing.T) {
	k8s := kubernetes.NewTestStatusClient(
		newRestoreObj("vm-manual-demo-1-123", "velero-system", "Completed"),
		newHelmReleaseObj("vcluster-demo", "vcluster-demo"),
		newKustomizationObj("tenant-demo", "flux-system"),
	)
	s := newVeleroTestService(k8s)

	view, err := s.GetVeleroRestoreStatus(context.Background(), "demo", "vm-manual-demo-1-123", "preprod", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if view.VolumeDestroyed {
		t.Error("expected VolumeDestroyed to stay false for a Completed restore")
	}
}
