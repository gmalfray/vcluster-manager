package v1alpha1

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Trigger annotations. Names come verbatim from
// docs/design-backup-restore-annotation.md §3 and §4 — keeping them identical
// is what makes the later migration onto the VCluster CRD (§10) a change of
// carrier object only, not a change of contract.
const (
	// AnnBackupRequestedAt carries an RFC3339 timestamp that changes on every
	// backup request. Same value seen twice ⇒ nothing happens (§5 dedup).
	AnnBackupRequestedAt = "backup.vcluster.rebuild-it.fr/requestedAt"

	// AnnRestoreRequestedAt is the restore counterpart of AnnBackupRequestedAt.
	AnnRestoreRequestedAt = "restore.vcluster.rebuild-it.fr/requestedAt"
	// AnnRestoreFromBackup is the Velero backup to restore. Required.
	AnnRestoreFromBackup = "restore.vcluster.rebuild-it.fr/from-backup"
	// AnnRestoreTarget is the destination vcluster. Empty or equal to the
	// marker's own name means an in-place restore (the destructive path).
	AnnRestoreTarget = "restore.vcluster.rebuild-it.fr/target"
	// AnnRestoreRequestedBy is free-form traceability for a kubectl-driven
	// request; the authoritative audit entry is written by the service when it
	// patches the annotation (design §6 point 3).
	AnnRestoreRequestedBy = "restore.vcluster.rebuild-it.fr/requested-by"
)

// Condition types surfaced on the marker (design §3, §4, crd-vcluster.md §3.3).
const (
	// CondAccepted dit si l'objet a passé la garde de placement. False veut dire
	// qu'il a été ignoré ENTIÈREMENT, pas qu'une opération a échoué.
	CondAccepted          = "Accepted"
	CondBackupCompleted   = "BackupCompleted"
	CondRestoreInProgress = "RestoreInProgress"
	// CondRestoreRejectedBusy is the guard the imperative path does not have
	// today: two concurrent in-place restores can each start their own
	// scale-down/PVC-delete sequence on the same vcluster.
	CondRestoreRejectedBusy = "RestoreRejectedBusy"
	CondFluxResumePending   = "FluxResumePending"
	// CondRestoreNeedsRetry means the vcluster is intentionally left suspended
	// and only a new restore request fixes it.
	CondRestoreNeedsRetry = "RestoreNeedsRetry"
)

// ResumeGiveUpAfter bounds how long the controller keeps trying to resume Flux
// after a restore has settled. Past that it stops requeueing and says so, rather
// than retrying every 10s forever — a vcluster stuck suspended is a real problem
// someone has to be told about. Same budget as the goroutine this replaces.
const ResumeGiveUpAfter = 2 * time.Hour

// BackupOpsStatus reports the last handled backup request.
type BackupOpsStatus struct {
	// LastHandledRequestedAt is the annotation value already acted on — the
	// Flux `lastHandledReconcileAt` pattern.
	// +optional
	LastHandledRequestedAt string `json:"lastHandledRequestedAt,omitempty"`
	// +optional
	BackupName string `json:"backupName,omitempty"`
	// +optional
	Phase string `json:"phase,omitempty"`
}

// RestoreOpsStatus is a direct transcription of service.VeleroRestoreStatusView:
// the controller does not invent a new contract, it writes the existing one to
// status instead of returning it over HTTP.
//
// Note what is deliberately absent: a record of how far the destructive sequence
// got. Both facts that matter — is the volume gone, is a restore running — are
// read back from the cluster on recovery (service.InspectInterruptedRestore),
// which cannot go stale the way a written record can.
type RestoreOpsStatus struct {
	// +optional
	LastHandledRequestedAt string `json:"lastHandledRequestedAt,omitempty"`
	// +optional
	RestoreName string `json:"restoreName,omitempty"`
	// +optional
	FromBackup string `json:"fromBackup,omitempty"`
	// Target empty means in-place.
	// +optional
	Target string `json:"target,omitempty"`
	// +optional
	InPlace bool `json:"inPlace,omitempty"`
	// +optional
	Phase string `json:"phase,omitempty"`
	// FirstTerminalAt is when the restore was first seen settled. It is what
	// makes the give-up bound survive a controller restart, unlike the in-memory
	// deadline it replaces.
	// +optional
	FirstTerminalAt *metav1.Time `json:"firstTerminalAt,omitempty"`
	// +optional
	ResumePending bool `json:"resumePending,omitempty"`
	// +optional
	ResumeFailed bool `json:"resumeFailed,omitempty"`
	// +optional
	ResumeError string `json:"resumeError,omitempty"`
	// VolumeDestroyed mirrors the app's data-safety flag: the volume was
	// deleted and the restore did not repopulate it.
	// +optional
	VolumeDestroyed bool `json:"volumeDestroyed,omitempty"`
}

// VClusterVeleroOpsStatus is written only through the /status subresource, by
// the controller only (design §5).
type VClusterVeleroOpsStatus struct {
	// +optional
	Backup BackupOpsStatus `json:"backup,omitempty"`
	// +optional
	Restore RestoreOpsStatus `json:"restore,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// VClusterVeleroOps carries backup/restore orders for one vcluster.
//
// It has no spec at all, on purpose. The vcluster it drives is its own
// metadata.name, in namespace vcluster-<name> — adding a spec.vclusterName would
// be a third copy of the same string, and crd-vcluster.md §2.1 argues against
// carrying an environment in spec (the operator runs in the environment it
// reconciles). No spec also means metadata.generation never moves, which is why
// there is no observedGeneration to report either.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=vvo
// +kubebuilder:printcolumn:name="Restore",type=string,JSONPath=`.status.restore.phase`
// +kubebuilder:printcolumn:name="Backup",type=string,JSONPath=`.status.backup.phase`
type VClusterVeleroOps struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Status VClusterVeleroOpsStatus `json:"status,omitempty"`
}

// VClusterName is the vcluster this marker drives.
func (o *VClusterVeleroOps) VClusterName() string { return o.Name }

// VClusterVeleroOpsList is the list form of VClusterVeleroOps.
//
// +kubebuilder:object:root=true
type VClusterVeleroOpsList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VClusterVeleroOps `json:"items"`
}

func init() {
	SchemeBuilder.Register(&VClusterVeleroOps{}, &VClusterVeleroOpsList{})
}
