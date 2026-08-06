package veleroops

import (
	"github.com/gmalfray/vcluster-manager/internal/service"
)

// The seam, proven at compile time: the real service satisfies everything the
// reconciler needs, with no shim, no adapter type and no duplicated logic. If
// any of these signatures moves, this file stops compiling.
//
// This used to be a partial assertion — the POC ran against a fake because two
// methods did not exist yet (a stage hook on the restore sequence, and an
// exported abort). Both are now in the service, additive, so the assertion
// covers the whole interface.
var _ Ops = (*service.Service)(nil)

// Sentinels the reconciler branches on. Asserting they exist keeps its
// data-safety branches wired to the real errors rather than to string matching.
var (
	_ error = service.ErrRestoreStageFailed
	_ error = service.ErrRestoreStageFailedVolumeGone
	_ error = service.ErrForbidden
)
