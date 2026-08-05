package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

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

// --- isTerminalRestorePhase ---

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
		if got := isTerminalRestorePhase(tt.phase); got != tt.want {
			t.Errorf("isTerminalRestorePhase(%q) = %v, want %v", tt.phase, got, tt.want)
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

func TestCreateVeleroRestore_AbortsWhenPVCDeleteFails(t *testing.T) {
	// Suspend and scale-down both succeed (StatefulSet seeded, embedded
	// topology, no pod so the wait returns immediately), but there's no PVC
	// under the expected name to delete.
	k8s := kubernetes.NewTestStatusClient(
		newBackupObj("manual-demo-1", "velero-system", "Completed"),
		newHelmReleaseObj("vcluster-demo", "vcluster-demo"),
		newKustomizationObj("tenant-demo", "flux-system"),
		newStatefulSetObj("vcluster-demo", "vcluster-demo", 1),
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

// --- GetVeleroRestoreStatus: resume failure must not read as success --------

func TestGetVeleroRestoreStatus_ResumeFailureIsReportedNotSwallowed(t *testing.T) {
	// Restore is Completed, but there's no HelmRelease/Kustomization to
	// resume: the resume step fails. The status call itself must still
	// succeed (the restore's own phase is legitimate data) — the failure
	// belongs in ResumeFailed/ResumeError, not the returned error.
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
	if !view.ResumeFailed {
		t.Error("expected ResumeFailed to be true when Flux couldn't be resumed")
	}
	if view.ResumeError == "" {
		t.Error("expected ResumeError to carry the underlying failure")
	}
}

func TestGetVeleroRestoreStatus_ResumeSuccessIsReported(t *testing.T) {
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
}
