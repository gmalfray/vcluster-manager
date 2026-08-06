// Package veleroops declares the narrow seam between the reconciler and
// internal/service. The reconciler is a third adapter of the service (design
// §7), next to internal/handlers (web) and internal/api (REST) — so this
// interface is written in the *service's own types*, not in a parallel set of
// DTOs, and every method matches a real service method signature for signature.
//
// seam_assert.go asserts that *service.Service satisfies it. That assertion is
// the point of this package: it is no longer possible for the POC to claim it
// reuses the service while quietly diverging from it.
package veleroops

import (
	"context"

	"github.com/gmalfray/vcluster-manager/internal/models"
	"github.com/gmalfray/vcluster-manager/internal/service"
)

// Ops is everything the reconciler needs from the service.
type Ops interface {
	// TriggerVeleroBackup creates an on-demand backup.
	TriggerVeleroBackup(ctx context.Context, actor models.Actor, name, env string) (service.VeleroBackupCreated, error)

	// GetVeleroBackupPhase polls one backup's phase.
	GetVeleroBackupPhase(ctx context.Context, backup, env string) (string, error)

	// CreateVeleroRestoreWithHooks runs the restore sequence, announcing each
	// destructive step through hooks.OnStage so the reconciler can persist it,
	// and with hooks.OwnsFollowUp set so the service does not also start its
	// own background watcher.
	CreateVeleroRestoreWithHooks(ctx context.Context, actor models.Actor, name, env, backupName, targetName string, hooks service.RestoreHooks) (service.VeleroRestoreView, error)

	// GetVeleroRestoreStatus polls a restore and, for an in-place one, reports
	// whether Flux is actually back.
	GetVeleroRestoreStatus(ctx context.Context, name, restoreName, env string, inPlace bool) (service.VeleroRestoreStatusView, error)

	// AbortInPlaceRestore resumes Flux on a vcluster left suspended by a
	// sequence that stopped before the point of no return.
	AbortInPlaceRestore(ctx context.Context, actor models.Actor, name, env string) error
}

// IsTerminalBackupPhase reports whether a Velero backup phase is settled. The
// restore counterpart lives in the service (service.IsTerminalRestorePhase) and
// is used directly rather than copied.
func IsTerminalBackupPhase(phase string) bool {
	return phase == "Completed" || phase == "Failed" || phase == "PartiallyFailed" ||
		phase == "FailedValidation" || phase == "Deleting"
}
