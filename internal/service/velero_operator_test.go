package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"

	"github.com/gmalfray/vcluster-manager/internal/kubernetes"
)

// What an operator-style caller needs on top of the web/REST path: a way to run
// the sequence without the service's own watcher, a way to read back what an
// interrupted sequence left behind, and an explicit abort. See
// docs/poc-operator-tech-decision.md.

// restoreFixture is the full set of objects an in-place restore needs to run
// through every stage successfully.
func restoreFixture() []runtime.Object {
	return []runtime.Object{
		newBackupObj("manual-demo-1", "velero-system", "Completed"),
		newHelmReleaseObj("vcluster-demo", "vcluster-demo"),
		newKustomizationObj("tenant-demo", "flux-system"),
		newStatefulSetObj("vcluster-demo", "vcluster-demo", 1),
		newPVCObj("data-vcluster-demo-0", "vcluster-demo"),
	}
}

// Who watches the restore to completion: the service, or the caller? Getting
// this wrong means two mechanisms driving the same Flux resume.
func TestCreateVeleroRestore_WhoWatchesTheAftermath(t *testing.T) {
	tests := []struct {
		name        string
		unwatched   bool
		wantWatcher bool
	}{
		{"CreateVeleroRestore watches, as it always did", false, true},
		{"CreateVeleroRestoreUnwatched leaves it to the caller", true, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			restoreGets := 0
			reactor := func(action clienttesting.Action) (bool, runtime.Object, error) {
				if action.GetVerb() == "get" && action.GetResource().Resource == "restores" {
					mu.Lock()
					restoreGets++
					mu.Unlock()
				}
				return false, nil, nil
			}
			k8s := kubernetes.NewTestStatusClientWithReactor(reactor, restoreFixture()...)
			s := newVeleroTestService(k8s)
			// Poll fast, so "did the watcher start" is answerable in milliseconds
			// instead of ten seconds.
			s.resumeWatchInterval = 2 * time.Millisecond

			var err error
			if tt.unwatched {
				_, err = s.CreateVeleroRestoreUnwatched(context.Background(), adminActor(), "demo", "preprod", "manual-demo-1", "")
			} else {
				_, err = s.CreateVeleroRestore(context.Background(), adminActor(), "demo", "preprod", "manual-demo-1", "")
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// Give a watcher, if there is one, plenty of ticks to show itself.
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				mu.Lock()
				seen := restoreGets
				mu.Unlock()
				if seen > 0 {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}

			mu.Lock()
			seen := restoreGets
			mu.Unlock()
			if got := seen > 0; got != tt.wantWatcher {
				t.Errorf("watcher en fond = %v (%d gets sur restores), attendu %v", got, seen, tt.wantWatcher)
			}
		})
	}
}

// --- InspectInterruptedRestore ---
//
// This is what replaces persisting each step as it happens: the two facts that
// matter are read back from the cluster.

func TestInspectInterruptedRestore_VolumeStillThere(t *testing.T) {
	k8s := kubernetes.NewTestStatusClient(restoreFixture()...)
	s := newVeleroTestService(k8s)

	view, err := s.InspectInterruptedRestore(context.Background(), "demo", "preprod")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if view.VolumeGone {
		t.Error("volume signalé disparu alors que le PVC est là")
	}
	if view.ActiveRestoreName != "" {
		t.Errorf("restore actif inventé: %q", view.ActiveRestoreName)
	}
}

func TestInspectInterruptedRestore_VolumeGone(t *testing.T) {
	// Same fixture minus the PVC: the sequence got past the point of no return.
	k8s := kubernetes.NewTestStatusClient(
		newBackupObj("manual-demo-1", "velero-system", "Completed"),
		newHelmReleaseObj("vcluster-demo", "vcluster-demo"),
		newStatefulSetObj("vcluster-demo", "vcluster-demo", 0),
	)
	s := newVeleroTestService(k8s)

	view, err := s.InspectInterruptedRestore(context.Background(), "demo", "preprod")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !view.VolumeGone {
		t.Error("PVC absent mais volume signalé intact — reprendre Flux ici recréerait un volume vide")
	}
}

func TestInspectInterruptedRestore_VolumeBeingDeletedCountsAsGone(t *testing.T) {
	// Delete only returns once the apiserver has set deletionTimestamp, so a
	// process killed during the deletion must still be told the volume is gone.
	pvc := newPVCObj("data-vcluster-demo-0", "vcluster-demo")
	meta, _, _ := unstructured.NestedMap(pvc.Object, "metadata")
	meta["deletionTimestamp"] = "2026-08-06T21:00:00Z"
	_ = unstructured.SetNestedMap(pvc.Object, meta, "metadata")

	k8s := kubernetes.NewTestStatusClient(
		newBackupObj("manual-demo-1", "velero-system", "Completed"),
		newHelmReleaseObj("vcluster-demo", "vcluster-demo"),
		newStatefulSetObj("vcluster-demo", "vcluster-demo", 0),
		pvc,
	)
	s := newVeleroTestService(k8s)

	view, err := s.InspectInterruptedRestore(context.Background(), "demo", "preprod")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !view.VolumeGone {
		t.Error("PVC en Terminating traité comme intact")
	}
}

// newActiveRestoreObj builds a Velero Restore that ListActiveVeleroRestores will
// actually match: it filters on spec.includedNamespaces, which the shared
// newRestoreObj fixture does not set.
func newActiveRestoreObj(name, veleroNS, targetNS, phase string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "velero.io/v1",
		"kind":       "Restore",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": veleroNS,
		},
		"spec": map[string]interface{}{
			"includedNamespaces": []interface{}{targetNS},
		},
		"status": map[string]interface{}{
			"phase": phase,
		},
	}}
}

// The case a written record gets wrong: the restore was created but its name was
// never recorded. Only the cluster knows.
func TestInspectInterruptedRestore_FindsARunningRestore(t *testing.T) {
	k8s := kubernetes.NewTestStatusClient(
		newBackupObj("manual-demo-1", "velero-system", "Completed"),
		newHelmReleaseObj("vcluster-demo", "vcluster-demo"),
		newStatefulSetObj("vcluster-demo", "vcluster-demo", 0),
		newActiveRestoreObj("r-orphan", "velero-system", "vcluster-demo", "InProgress"),
	)
	s := newVeleroTestService(k8s)

	view, err := s.InspectInterruptedRestore(context.Background(), "demo", "preprod")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if view.ActiveRestoreName != "r-orphan" {
		t.Fatalf("restore en cours non retrouvé: %q", view.ActiveRestoreName)
	}
	if view.ActiveRestorePhase != "InProgress" {
		t.Errorf("phase = %q, attendu InProgress", view.ActiveRestorePhase)
	}
}

func TestInspectInterruptedRestore_RejectsInvalidName(t *testing.T) {
	s := newVeleroTestService(nil)
	if _, err := s.InspectInterruptedRestore(context.Background(), "../etc/passwd", "preprod"); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("expected ErrInvalidName, got %v", err)
	}
}

// --- AbortInPlaceRestore ---

func TestAbortInPlaceRestore_ForbiddenForNonAdmin(t *testing.T) {
	s := newVeleroTestService(nil)
	if err := s.AbortInPlaceRestore(context.Background(), plainActor(), "demo", "preprod"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected ErrForbidden, got %v", err)
	}
}

func TestAbortInPlaceRestore_RejectsInvalidNameBeforeK8sLookup(t *testing.T) {
	// No client configured: were the name check to run after the lookup, this
	// would come back as ErrK8sUnavailable instead.
	s := newVeleroTestService(nil)
	if err := s.AbortInPlaceRestore(context.Background(), adminActor(), "../etc/passwd", "preprod"); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("expected ErrInvalidName, got %v", err)
	}
}

func TestAbortInPlaceRestore_ResumesFlux(t *testing.T) {
	var mu sync.Mutex
	patches := 0
	reactor := func(action clienttesting.Action) (bool, runtime.Object, error) {
		if action.GetVerb() == "patch" && action.GetResource().Resource == "helmreleases" {
			mu.Lock()
			patches++
			mu.Unlock()
		}
		return false, nil, nil
	}
	k8s := kubernetes.NewTestStatusClientWithReactor(reactor,
		newHelmReleaseObj("vcluster-demo", "vcluster-demo"),
		newKustomizationObj("tenant-demo", "flux-system"),
	)
	s := newVeleroTestService(k8s)

	if err := s.AbortInPlaceRestore(context.Background(), adminActor(), "demo", "preprod"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if patches == 0 {
		t.Error("expected the HelmRelease to be patched to resume Flux")
	}
}

// --- GetVeleroBackupPhase ---

func TestGetVeleroBackupPhase_ReturnsThePhase(t *testing.T) {
	k8s := kubernetes.NewTestStatusClient(newBackupObj("manual-demo-1", "velero-system", "Completed"))
	s := newVeleroTestService(k8s)

	phase, err := s.GetVeleroBackupPhase(context.Background(), "manual-demo-1", "preprod")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if phase != "Completed" {
		t.Errorf("phase = %q, want Completed", phase)
	}
}

func TestGetVeleroBackupPhase_ValidatesBeforeK8sLookup(t *testing.T) {
	s := newVeleroTestService(nil)
	if _, err := s.GetVeleroBackupPhase(context.Background(), "../etc/passwd", "preprod"); !errors.Is(err, ErrInvalidBackupName) {
		t.Fatalf("expected ErrInvalidBackupName, got %v", err)
	}
	if _, err := s.GetVeleroBackupPhase(context.Background(), "", "preprod"); !errors.Is(err, ErrBackupNameRequired) {
		t.Fatalf("expected ErrBackupNameRequired, got %v", err)
	}
}
