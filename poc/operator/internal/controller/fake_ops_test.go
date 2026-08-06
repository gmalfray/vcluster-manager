package controller

import (
	"context"
	"sync"

	"github.com/gmalfray/vcluster-manager/internal/models"
	"github.com/gmalfray/vcluster-manager/internal/service"

	"github.com/gmalfray/vcluster-manager/poc/operator/internal/veleroops"
)

// fakeOps stands in for *service.Service. It fakes only what talks to a live
// cluster (Velero, Flux, PVCs); its shapes are the service's own types, and
// veleroops/seam_assert.go proves the real service satisfies the same interface.
// So these tests exercise the reconcile semantics without a cluster, while the
// service's own tests cover the restore sequence itself.
type fakeOps struct {
	mu sync.Mutex

	// scripted behaviour
	backupName   string
	backupErr    error
	backupPhases []string // consumed one per GetVeleroBackupPhase call, last repeats
	restoreName  string
	restoreErr   error
	// restoreStatus is consumed one per GetVeleroRestoreStatus call, last repeats.
	restoreStatus []service.VeleroRestoreStatusView
	inspectView   service.InterruptedRestoreView
	inspectErr    error
	abortErr      error
	// killDuringSequence simulates the process dying mid-sequence: the call never
	// returns, so nothing gets written to status afterwards.
	killDuringSequence bool

	// observed calls
	triggerBackupCalls int
	createRestoreCalls int
	inspectCalls       int
	abortCalls         int
}

var _ veleroops.Ops = (*fakeOps)(nil)

type errKilled struct{}

func (*errKilled) Error() string { return "process killed mid-sequence" }

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

func (f *fakeOps) CreateVeleroRestoreUnwatched(_ context.Context, _ models.Actor, name, env, backupName, targetName string) (service.VeleroRestoreView, error) {
	f.mu.Lock()
	f.createRestoreCalls++
	kill := f.killDuringSequence
	err := f.restoreErr
	restoreName := f.restoreName
	f.mu.Unlock()

	if kill {
		panic(&errKilled{})
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

func (f *fakeOps) InspectInterruptedRestore(_ context.Context, _, _ string) (service.InterruptedRestoreView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inspectCalls++
	return f.inspectView, f.inspectErr
}

func (f *fakeOps) GetVeleroRestoreStatus(_ context.Context, name, restoreName, env string, inPlace bool) (service.VeleroRestoreStatusView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
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

func (f *fakeOps) counts() (trigger, create, inspect, abort int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.triggerBackupCalls, f.createRestoreCalls, f.inspectCalls, f.abortCalls
}
