package veleroops

import (
	"context"

	"github.com/gmalfray/vcluster-manager/internal/models"
	"github.com/gmalfray/vcluster-manager/internal/service"
)

// serviceAsIsToday is the part of Ops that *service.Service already satisfies,
// method for method, with no shim. If someone changes one of these signatures,
// this file stops compiling — which is the point: the POC cannot quietly drift
// away from the real service it claims to reuse.
type serviceAsIsToday interface {
	TriggerVeleroBackup(ctx context.Context, actor models.Actor, name, env string) (service.VeleroBackupCreated, error)
	GetVeleroBackups(ctx context.Context, name, env string) (service.VeleroBackupsView, error)
	GetVeleroRestoreStatus(ctx context.Context, name, restoreName, env string, inPlace bool) (service.VeleroRestoreStatusView, error)
}

var _ serviceAsIsToday = (*service.Service)(nil)

// serviceRestoreAsIsToday is the current restore entry point: identical to
// Ops.CreateVeleroRestore minus the onStage parameter. Keeping it asserted
// separately measures the exact size of the change the operator needs — one
// added parameter, no logic moved out of the service.
type serviceRestoreAsIsToday interface {
	CreateVeleroRestore(ctx context.Context, actor models.Actor, name, env, backupName, targetName string) (service.VeleroRestoreView, error)
}

var _ serviceRestoreAsIsToday = (*service.Service)(nil)

// Sentinels the reconciler branches on. Asserting they exist keeps the POC's
// data-safety branches wired to the real errors rather than to string matching.
var (
	_ error = service.ErrRestoreStageFailed
	_ error = service.ErrRestoreStageFailedVolumeGone
	_ error = service.ErrForbidden
)

// Not satisfied by *service.Service today, and deliberately not shimmed:
//
//   - CreateVeleroRestore(..., onStage StageFunc): additive parameter.
//   - AbortInPlaceRestore(ctx, actor, name, env) error: export of the existing
//     unexported abortInPlaceRestore.
//
// Both are listed in docs/poc-operator-tech-decision.md as the prerequisites of
// the real implementation. The POC exercises them through a fake so the
// reconcile logic can be proven before the service is touched at all.
