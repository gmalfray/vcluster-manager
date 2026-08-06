package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"

	"github.com/gmalfray/vcluster-manager/internal/kubernetes"
)

// These cover what RestoreHooks adds for an operator-style caller: a durable
// record of how far the destructive sequence got, and the choice of who watches
// the aftermath. See docs/poc-operator-tech-decision.md §5 for why the
// reconciler cannot do its job without them.

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

func TestCreateVeleroRestoreWithHooks_AnnouncesEveryStage(t *testing.T) {
	k8s := kubernetes.NewTestStatusClient(restoreFixture()...)
	s := newVeleroTestService(k8s)

	var stages []RestoreStage
	hooks := RestoreHooks{
		OnStage: func(_ context.Context, st RestoreStage) error {
			stages = append(stages, st)
			return nil
		},
		OwnsFollowUp: true,
	}

	view, err := s.CreateVeleroRestoreWithHooks(context.Background(), adminActor(), "demo", "preprod", "manual-demo-1", "", hooks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if view.RestoreName == "" {
		t.Error("expected a non-empty restore name")
	}

	want := []RestoreStage{
		RestoreStageFluxSuspended,
		RestoreStageScaledDown,
		RestoreStagePVCDeleted,
		RestoreStageRestoreCreated,
	}
	if len(stages) != len(want) {
		t.Fatalf("stages = %v, want %v", stages, want)
	}
	for i := range want {
		if stages[i] != want[i] {
			t.Fatalf("stages = %v, want %v", stages, want)
		}
	}
}

// The contract that makes the record trustworthy: if a stage cannot be recorded,
// the step it announced does NOT run. Otherwise a caller could come back
// believing the volume is intact when it is not — the one mistake that silently
// destroys data.
func TestCreateVeleroRestoreWithHooks_UnrecordableStageAbortsBeforeTheStepRuns(t *testing.T) {
	var mu sync.Mutex
	var pvcDeletes, helmReleasePatches int
	reactor := func(action clienttesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		if action.GetVerb() == "delete" && action.GetResource().Resource == "persistentvolumeclaims" {
			pvcDeletes++
		}
		if action.GetVerb() == "patch" && action.GetResource().Resource == "helmreleases" {
			helmReleasePatches++
		}
		return false, nil, nil
	}
	k8s := kubernetes.NewTestStatusClientWithReactor(reactor, restoreFixture()...)
	s := newVeleroTestService(k8s)

	hooks := RestoreHooks{
		OnStage: func(_ context.Context, st RestoreStage) error {
			if st == RestoreStagePVCDeleted {
				return fmt.Errorf("simulated failure persisting the stage")
			}
			return nil
		},
		OwnsFollowUp: true,
	}

	_, err := s.CreateVeleroRestoreWithHooks(context.Background(), adminActor(), "demo", "preprod", "manual-demo-1", "", hooks)

	if !errors.Is(err, ErrRestoreStageFailed) {
		t.Fatalf("expected ErrRestoreStageFailed, got %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if pvcDeletes != 0 {
		t.Errorf("the PVC was deleted (%d delete calls) even though the stage could not be recorded", pvcDeletes)
	}
	// One patch is SetFluxSuspend(true); a second means the abort resumed Flux —
	// correct here, since the volume is untouched.
	if helmReleasePatches < 2 {
		t.Errorf("flux was not resumed after the abort (helmrelease patches = %d), volume is intact so it should be", helmReleasePatches)
	}
}

// OwnsFollowUp is finding #3 of the POC: without it, a reconciler and the
// service's own goroutine both drive the same resume.
func TestCreateVeleroRestoreWithHooks_OwnsFollowUpDecidesWhoWatches(t *testing.T) {
	tests := []struct {
		name         string
		ownsFollowUp bool
		wantWatcher  bool
	}{
		{"caller owns the follow-up: the service must not watch", true, false},
		{"nobody said otherwise: the service watches, as it always did", false, true},
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

			hooks := RestoreHooks{OwnsFollowUp: tt.ownsFollowUp}
			if _, err := s.CreateVeleroRestoreWithHooks(context.Background(), adminActor(), "demo", "preprod", "manual-demo-1", "", hooks); err != nil {
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
				t.Errorf("background watcher polling = %v (%d gets on restores), want %v", got, seen, tt.wantWatcher)
			}
		})
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
