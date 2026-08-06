// Package veleroops declares the narrow seam between the reconciler and
// internal/service. The reconciler is meant to be a third adapter of the
// service (design §7), next to internal/handlers (web) and internal/api
// (REST) — so this interface is written in the *service's own types*, not in a
// parallel set of DTOs. seam_assert.go then asserts, at compile time, that
// *service.Service really satisfies the part of it that exists today; what is
// missing is spelled out there rather than hand-waved.
package veleroops

import (
	"context"

	"github.com/gmalfray/vcluster-manager/internal/models"
	"github.com/gmalfray/vcluster-manager/internal/service"

	"github.com/gmalfray/vcluster-manager/poc/operator/api/v1alpha1"
)

// StageFunc is called by the restore sequence right before each destructive
// step, and must persist the stage durably before returning. If it returns an
// error the sequence aborts *before* performing the step it announced — which
// is the only way to guarantee that a stage recorded in status is never behind
// reality (the dangerous direction: believing the volume still exists when it
// does not).
type StageFunc func(ctx context.Context, stage v1alpha1.RestoreStage) error

// Ops is everything the reconciler needs. Signatures match
// internal/service/velero.go verbatim except where a comment says otherwise.
type Ops interface {
	// TriggerVeleroBackup: unchanged from the service.
	TriggerVeleroBackup(ctx context.Context, actor models.Actor, name, env string) (service.VeleroBackupCreated, error)

	// GetVeleroBackups: unchanged. Used to poll one backup's phase, which
	// costs a full list — the service exposes no per-backup accessor even
	// though internal/kubernetes has GetVeleroBackupPhase. Noted as a finding,
	// not worked around here.
	GetVeleroBackups(ctx context.Context, name, env string) (service.VeleroBackupsView, error)

	// CreateVeleroRestore is the service method plus one additive parameter:
	// onStage. Without it the whole destructive sequence (suspend Flux → scale
	// to 0 → delete PVC → create Restore) happens inside one blocking call,
	// so a controller that dies mid-call cannot know whether the PVC was
	// deleted — and cannot honour design §4 point 4. Its internal sequence and
	// its two sentinel errors are otherwise untouched.
	CreateVeleroRestore(ctx context.Context, actor models.Actor, name, env, backupName, targetName string, onStage StageFunc) (service.VeleroRestoreView, error)

	// GetVeleroRestoreStatus: unchanged from the service.
	GetVeleroRestoreStatus(ctx context.Context, name, restoreName, env string, inPlace bool) (service.VeleroRestoreStatusView, error)

	// AbortInPlaceRestore resumes Flux after a sequence that stopped *before*
	// the point of no return, so the vcluster is not left suspended at zero
	// replicas over an intact volume. The service does this today only from
	// inside CreateVeleroRestore (unexported abortInPlaceRestore); a
	// controller that resumes an interrupted sequence needs it exported.
	AbortInPlaceRestore(ctx context.Context, actor models.Actor, name, env string) error
}

// IsTerminalRestorePhase mirrors the service's unexported
// isTerminalRestorePhase. Duplicated here only because it is unexported; the
// full implementation should export it rather than keep two copies.
func IsTerminalRestorePhase(phase string) bool {
	return phase == "Completed" || phase == "Failed" || phase == "PartiallyFailed"
}

// IsTerminalBackupPhase reports whether a Velero backup phase is settled.
func IsTerminalBackupPhase(phase string) bool {
	return phase == "Completed" || phase == "Failed" || phase == "PartiallyFailed" ||
		phase == "FailedValidation" || phase == "Deleting"
}
