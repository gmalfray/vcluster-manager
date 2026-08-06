package controller

import (
	"context"
	"sync"

	"github.com/gmalfray/vcluster-manager/internal/models"
	"github.com/gmalfray/vcluster-manager/internal/service"

	"github.com/gmalfray/vcluster-manager/poc/operator/api/v1alpha1"
	"github.com/gmalfray/vcluster-manager/poc/operator/internal/veleroops"
)

// fakeOps stands in for *service.Service. It fakes only what talks to a live
// cluster (Velero, Flux, PVCs); its shapes are the service's own types, and
// veleroops/seam_assert.go proves the real service satisfies the same interface.
// So these tests exercise the reconcile semantics without a cluster, while the
// service's own tests cover the sequence itself.
type fakeOps struct {
	mu sync.Mutex

	// scripted behaviour
	backupName   string
	backupErr    error
	backupPhases []string // consumed one per GetVeleroBackupPhase call
	restoreName  string
	restoreErr   error
	stagesToRun  []service.RestoreStage
	// restoreStatus is consumed one per GetVeleroRestoreStatus call; the last
	// entry repeats.
	restoreStatus  []service.VeleroRestoreStatusView
	abortErr       error
	failAfterStage service.RestoreStage // stop the sequence here with an error
	// panicAfterStage simulates the process being killed right after a stage was
	// durably recorded: the reconcile never returns, so nothing else is written
	// to status. Only what the stage hook already persisted survives.
	panicAfterStage service.RestoreStage

	// observed calls
	triggerBackupCalls int
	createRestoreCalls int
	statusCalls        int
	abortCalls         int
	ownedFollowUp      bool
	stagesReported     []v1alpha1.RestoreStage
}

var _ veleroops.Ops = (*fakeOps)(nil)

// errCrash is what a killed process looks like from inside the sequence: the
// step after the last reported stage never happens.
type errCrash struct{ stage service.RestoreStage }

func (e *errCrash) Error() string { return "process killed after stage " + string(e.stage) }

func (f *fakeOps) TriggerVeleroBackup(_ context.Context, _ models.Actor, name, env string) (service.VeleroBackupCreated, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.triggerBackupCalls++
	if f.backupErr != nil {
		return service.VeleroBackupCreated{}, f.backupErr
	}
	return service.VeleroBackupCreated{BackupName: f.backupName, Name: name, Env: env}, nil
}

func (f *fakeOps) GetVeleroBackupPhase(_ context.Context, _, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	phase := "InProgress"
	if len(f.backupPhases) > 0 {
		phase = f.backupPhases[0]
		if len(f.backupPhases) > 1 {
			f.backupPhases = f.backupPhases[1:]
		}
	}
	return phase, nil
}

func (f *fakeOps) CreateVeleroRestoreWithHooks(ctx context.Context, _ models.Actor, name, env, backupName, targetName string, hooks service.RestoreHooks) (service.VeleroRestoreView, error) {
	f.mu.Lock()
	f.createRestoreCalls++
	f.ownedFollowUp = hooks.OwnsFollowUp
	stages := f.stagesToRun
	crashAt := f.failAfterStage
	killAt := f.panicAfterStage
	err := f.restoreErr
	restoreName := f.restoreName
	f.mu.Unlock()

	for _, stage := range stages {
		if hooks.OnStage != nil {
			if reportErr := hooks.OnStage(ctx, stage); reportErr != nil {
				return service.VeleroRestoreView{}, reportErr
			}
		}
		f.mu.Lock()
		f.stagesReported = append(f.stagesReported, v1alpha1.RestoreStage(stage))
		f.mu.Unlock()
		if killAt != "" && stage == killAt {
			panic(&errCrash{stage: stage})
		}
		if crashAt != "" && stage == crashAt {
			// The announced step is recorded; everything after it never runs.
			return service.VeleroRestoreView{}, &errCrash{stage: stage}
		}
	}
	if err != nil {
		return service.VeleroRestoreView{}, err
	}
	return service.VeleroRestoreView{
		RestoreName: restoreName,
		Phase:       "New",
		Name:        name,
		Env:         env,
		BackupName:  backupName,
		InPlace:     targetName == "" || targetName == name,
	}, nil
}

func (f *fakeOps) GetVeleroRestoreStatus(_ context.Context, name, restoreName, env string, inPlace bool) (service.VeleroRestoreStatusView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusCalls++
	view := service.VeleroRestoreStatusView{Phase: "InProgress"}
	if len(f.restoreStatus) > 0 {
		view = f.restoreStatus[0]
		if len(f.restoreStatus) > 1 {
			f.restoreStatus = f.restoreStatus[1:]
		}
	}
	view.RestoreName = restoreName
	view.Name = name
	view.Env = env
	view.InPlace = inPlace
	return view, nil
}

func (f *fakeOps) AbortInPlaceRestore(_ context.Context, _ models.Actor, _, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.abortCalls++
	return f.abortErr
}

func (f *fakeOps) counts() (trigger, create, status, abort int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.triggerBackupCalls, f.createRestoreCalls, f.statusCalls, f.abortCalls
}

func (f *fakeOps) stages() []v1alpha1.RestoreStage {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]v1alpha1.RestoreStage, len(f.stagesReported))
	copy(out, f.stagesReported)
	return out
}

func (f *fakeOps) ownedTheFollowUp() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ownedFollowUp
}
