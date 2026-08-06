package v1alpha1

import (
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
	// AnnBackupTTL is optional; empty means the app's VeleroDefaultTTL.
	AnnBackupTTL = "backup.vcluster.rebuild-it.fr/ttl"

	// AnnRestoreRequestedAt is the restore counterpart of AnnBackupRequestedAt.
	AnnRestoreRequestedAt = "restore.vcluster.rebuild-it.fr/requestedAt"
	// AnnRestoreFromBackup is the Velero backup to restore. Required.
	AnnRestoreFromBackup = "restore.vcluster.rebuild-it.fr/from-backup"
	// AnnRestoreTarget is the destination vcluster. Empty or equal to
	// spec.vclusterName means an in-place restore (the destructive path).
	AnnRestoreTarget = "restore.vcluster.rebuild-it.fr/target"
	// AnnRestoreRequestedBy is free-form traceability for a kubectl-driven
	// request; the authoritative audit entry is written by the service when it
	// patches the annotation (design §6 point 3).
	AnnRestoreRequestedBy = "restore.vcluster.rebuild-it.fr/requested-by"
)

// RestoreStage is how far the in-place restore sequence of
// service.CreateVeleroRestore has actually got. It is persisted in status
// *before* each step runs, which is the whole point: a controller that dies
// mid-sequence and comes back must know which side of the point of no return
// (StagePVCDeleted) it is on, because that decides whether resuming Flux is
// the repair or the thing that destroys the evidence.
type RestoreStage string

const (
	// StageFluxSuspended: Flux suspended, volume still intact.
	StageFluxSuspended RestoreStage = "FluxSuspended"
	// StageScaledDown: workloads at 0 replicas, volume still intact.
	StageScaledDown RestoreStage = "ScaledDown"
	// StagePVCDeleted: point of no return — the volume is gone. Resuming Flux
	// from here lets the StatefulSet recreate an empty PVC and hides the
	// failure (service.ErrRestoreStageFailedVolumeGone).
	StagePVCDeleted RestoreStage = "PVCDeleted"
	// StageRestoreCreated: the Velero Restore object exists; from here on it is
	// a poll-until-terminal, then resume Flux.
	StageRestoreCreated RestoreStage = "RestoreCreated"
)

// Condition types surfaced on the marker (design §3, §4, crd-vcluster.md §3.3).
const (
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

// VClusterVeleroOpsSpec is deliberately tiny: the marker holds no desired
// state. A backup or a restore is an order, not a desired state (design §1) —
// spec only says which vcluster the orders apply to.
type VClusterVeleroOpsSpec struct {
	// VClusterName is the vcluster this marker drives. Its namespace
	// (vcluster-<name>) is where the marker lives, so deleting the vcluster
	// garbage-collects the marker without an ownerReference.
	// +kubebuilder:validation:MinLength=1
	VClusterName string `json:"vclusterName"`

	// Env is vestigial for an in-cluster operator (crd-vcluster.md §2.1: one
	// operator per host cluster) and kept only so the POC can call the
	// existing service methods unchanged.
	// +optional
	Env string `json:"env,omitempty"`
}

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
	// RequestedTTL records a TTL asked for through AnnBackupTTL. The POC
	// records it but cannot honour it: TriggerVeleroBackup is hardcoded on
	// cfg.VeleroDefaultTTL. Written down rather than silently dropped.
	// +optional
	RequestedTTL string `json:"requestedTTL,omitempty"`
	// TTLHonoured is false whenever RequestedTTL is set, until the service
	// accepts a per-request TTL.
	// +optional
	TTLHonoured bool `json:"ttlHonoured,omitempty"`
}

// RestoreOpsStatus is a direct transcription of service.VeleroRestoreStatusView
// plus Stage: the controller does not invent a new contract, it writes the
// existing one to status instead of returning it over HTTP.
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
	// Stage survives a controller restart. See RestoreStage.
	// +optional
	Stage RestoreStage `json:"stage,omitempty"`
	// +optional
	Phase string `json:"phase,omitempty"`
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
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	Backup BackupOpsStatus `json:"backup,omitempty"`
	// +optional
	Restore RestoreOpsStatus `json:"restore,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// VClusterVeleroOps is the marker resource carrying backup/restore orders for
// one vcluster.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=vvo
// +kubebuilder:printcolumn:name="VCluster",type=string,JSONPath=`.spec.vclusterName`
// +kubebuilder:printcolumn:name="Restore",type=string,JSONPath=`.status.restore.phase`
// +kubebuilder:printcolumn:name="Stage",type=string,JSONPath=`.status.restore.stage`
// +kubebuilder:printcolumn:name="Backup",type=string,JSONPath=`.status.backup.phase`
type VClusterVeleroOps struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VClusterVeleroOpsSpec   `json:"spec,omitempty"`
	Status VClusterVeleroOpsStatus `json:"status,omitempty"`
}

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
