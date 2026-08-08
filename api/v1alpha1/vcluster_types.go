package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// VClusterType is the provisioning backend. The discriminant exists from the
// very first schema on purpose (ADR-001, suite point 1): adding Cluster API
// later must be one more backend, not a breaking rewrite of VClusterSpec.
//
// +kubebuilder:validation:Enum=vcluster;capi
type VClusterType string

const (
	// VClusterTypeVCluster is the only backend implemented today.
	VClusterTypeVCluster VClusterType = "vcluster"
	// VClusterTypeCAPI is reserved. It is refused by validation until Cluster
	// API is actually implemented — see the CEL rule on VClusterSpec. Reserving
	// the value without accepting it is the point: the enum does not change the
	// day CAPI lands, only the rule does.
	VClusterTypeCAPI VClusterType = "capi"
)

// ArgoCDSpec configures the tenant's ArgoCD instance.
type ArgoCDSpec struct {
	// +optional
	Enabled bool `json:"enabled,omitempty"`
	// Version overrides the platform-wide ArgoCD version for this vcluster only.
	// Bornée pour la même raison que RBACGroups : elle est publiée telle quelle
	// en variable de substitution.
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9_.+-]*$`
	// +kubebuilder:validation:MaxLength=128
	// +optional
	Version string `json:"version,omitempty"`
	// RBACGroups are the OIDC groups granted access, rendered into ArgoCD's
	// policy.csv by the controller.
	//
	// Le pattern borne ce qui peut entrer dans une ligne de policy.csv. Il est
	// plus étroit que ce que Keycloak accepte, délibérément : ces valeurs sont
	// rendues dans un scalaire bloc YAML et publiées en variable de substitution
	// que Flux remplace textuellement avant de parser. Un saut de ligne au
	// milieu d'un nom de groupe suffit à terminer le bloc et à injecter des clés
	// arbitraires dans le manifeste appliqué.
	// +kubebuilder:validation:items:Pattern=`^[A-Za-z0-9_.:@/-]+$`
	// +kubebuilder:validation:items:MaxLength=253
	// +optional
	RBACGroups []string `json:"rbacGroups,omitempty"`
}

// VeleroSpec is the *permanent backup policy* — a desired state, unlike a
// one-off "back up now" order, which is an annotation on VClusterVeleroOps
// (see docs/design-backup-restore-annotation.md §1).
type VeleroSpec struct {
	// +optional
	Enabled bool `json:"enabled,omitempty"`
	// Hour is the daily backup time, "HH:MM". The controller turns it into a
	// cron expression; the CR keeps the form a human can read in a diff.
	// +kubebuilder:validation:Pattern=`^([01][0-9]|2[0-3]):[0-5][0-9]$`
	// +optional
	Hour string `json:"hour,omitempty"`
	// TTL is the retention in short form — "30j", "12h", "90m".
	//
	// Deliberately not Velero's raw form ("720h0m0s"): this CR is what a human
	// reads when reviewing the diff on a protected branch. Converting to Velero's
	// form is the controller's job, not the reader's.
	// +kubebuilder:validation:Pattern=`^[0-9]+[jhm]$`
	// +optional
	TTL string `json:"ttl,omitempty"`
}

// QuotaSpec caps what the tenant may consume.
//
// Expressed positively (`enabled`) rather than as the current `NoQuotas`
// negation: an absent block must mean "quotas on", so that forgetting the block
// is the safe outcome rather than an unbounded tenant.
type QuotaSpec struct {
	// +optional
	Enabled bool `json:"enabled,omitempty"`
	// +optional
	CPU string `json:"cpu,omitempty"`
	// +optional
	Memory string `json:"memory,omitempty"`
	// +optional
	Storage string `json:"storage,omitempty"`
}

// FluxCDSpec bootstraps a Flux inside the tenant, pointed at its own repo.
type FluxCDSpec struct {
	// RepoURL est publiée en variable de substitution, donc bornée : ni saut de
	// ligne, ni espace, ni guillemet ne doivent pouvoir atteindre un manifeste
	// que Flux applique avec son propre ServiceAccount.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^(https://|ssh://|git@)[A-Za-z0-9_.:@/~-]+$`
	RepoURL string `json:"repoURL"`
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9._/-]*$`
	// +optional
	Branch string `json:"branch,omitempty"`
	// +kubebuilder:validation:MaxLength=1024
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9._/-]*$`
	// +optional
	Path string `json:"path,omitempty"`
}

// VClusterSpec is the desired state of a vcluster.
//
// Two things are deliberately absent (crd-vcluster.md §2.1 and §2.2):
//
//   - **The name and the cell.** The name is `metadata.name`; the cell is where
//     the CR is applied (`clusters/<cell>/vclusters/<name>/`), reconciled by the
//     operator instance running there. A `spec.cell` field would be a second
//     source of truth for the same fact, free to diverge from the Git path.
//   - **Everything the generator derives** — API host, domain, wildcard secret,
//     ArgoCD client ID and URL, target revision, TLS secret, pod security. They
//     are functions of the name, the cell and the operator's own configuration,
//     recomputed every reconcile. Freezing them in Git would pin a value only the
//     operator should produce.
//
// +kubebuilder:validation:XValidation:rule="self.type != 'capi'",message="type: capi est réservé — Cluster API n'est pas encore implémenté (docs/etude-cluster-api.md). La valeur existe dans l'énumération pour que son arrivée ne casse pas le schéma."
type VClusterSpec struct {
	// Type is the provisioning backend.
	//
	// Optionnel avec une valeur par défaut, et non requis : une valeur par défaut
	// ne s'applique qu'à un champ ABSENT du JSON. En le déclarant requis, le
	// client Go sérialisait `type: ""`, que l'énumération refusait — un CR
	// minimal devenait impossible à écrire, ce qui contredit « un objet de vingt
	// lignes lisible en revue ».
	// +kubebuilder:default=vcluster
	// +optional
	Type VClusterType `json:"type,omitempty"`

	// Owner is who answers for this vcluster. Required, and it has no equivalent
	// in the current code: without an owner, neither the resource budget nor the
	// protected-branch review has anyone to talk to.
	// +kubebuilder:validation:MinLength=1
	Owner string `json:"owner"`

	// DeletionProtection must be lifted in its own commit, separate from the one
	// that actually removes the CR (ADR-001). The finalizer checks it again at
	// the last possible moment — see crd-vcluster.md §4.3.
	// +optional
	DeletionProtection bool `json:"deletionProtection,omitempty"`

	// Suspend is the reversible half of deletion: the controller suspends Flux,
	// scales the vcluster to zero and starts the grace period, but destroys
	// nothing. A `git revert` of that commit brings everything back.
	//
	// It is NOT metadata.deletionTimestamp, and that is the whole point: once
	// Kubernetes sets a deletionTimestamp the decision is irreversible, so the
	// "cancellable grace period" has to happen before it (crd-vcluster.md §4.2).
	// +optional
	Suspend bool `json:"suspend,omitempty"`

	// +optional
	ArgoCD *ArgoCDSpec `json:"argoCD,omitempty"`
	// +optional
	Velero *VeleroSpec `json:"velero,omitempty"`
	// +optional
	Quotas *QuotaSpec `json:"quotas,omitempty"`
	// K8sVersion pins the Kubernetes version inside the vcluster.
	// +optional
	K8sVersion string `json:"k8sVersion,omitempty"`
	// +optional
	FluxCD *FluxCDSpec `json:"fluxCD,omitempty"`
}

// VClusterPhase is the one-word summary. The detail lives in conditions.
// +kubebuilder:validation:Enum=Pending;Provisioning;Ready;Degraded;Suspended;Deleting;Failed
type VClusterPhase string

const (
	VClusterPhasePending      VClusterPhase = "Pending"
	VClusterPhaseProvisioning VClusterPhase = "Provisioning"
	VClusterPhaseReady        VClusterPhase = "Ready"
	VClusterPhaseDegraded     VClusterPhase = "Degraded"
	VClusterPhaseSuspended    VClusterPhase = "Suspended"
	VClusterPhaseDeleting     VClusterPhase = "Deleting"
	VClusterPhaseFailed       VClusterPhase = "Failed"
)

// Condition types (crd-vcluster.md §3.3). Ready is the aggregate Flux can use as
// a Kustomization health check — which is what makes a failure visible in
// `flux get kustomizations` without needing an admission webhook.
const (
	// CondAccepted est partagée avec le marqueur, déclarée dans
	// vclusterveleroops_types.go : la garde de placement est la même règle sur
	// les deux CRD, elle mérite un seul nom.
	CondVClusterReady           = "Ready"
	CondResourcesProvisioned    = "ResourcesProvisioned"
	CondBudgetOK                = "BudgetOK"
	CondArgoCDReady             = "ArgoCDReady"
	CondVaultConfigured         = "VaultConfigured"
	CondRancherPaired           = "RancherPaired"
	CondDeletionProtected       = "DeletionProtected"
	CondVClusterBackupCompleted = "BackupCompleted"
	// CondNamespaceRemoved porte la dernière étape de la suppression. Elle existe
	// parce que la disparition d'un namespace se CONSTATE : la demander ne fait
	// que poser un deletionTimestamp, et sans condition dédiée le finalizer
	// n'aurait ni où écrire ce qu'il attend, ni d'ancre de délai pour renoncer.
	CondNamespaceRemoved = "NamespaceRemoved"
)

// ResourceUsage mirrors what the dashboard already displays.
type ResourceUsage struct {
	// +optional
	CPU string `json:"cpu,omitempty"`
	// +optional
	Memory string `json:"memory,omitempty"`
	// +optional
	Storage string `json:"storage,omitempty"`
}

// RancherStatus reports the pairing states the UI already knows how to render.
type RancherStatus struct {
	// +optional
	State string `json:"state,omitempty"`
	// +optional
	Paired bool `json:"paired,omitempty"`
}

// VaultStatus mirrors the existing VaultSetupState (waiting/configuring/done/error).
type VaultStatus struct {
	// +optional
	Status string `json:"status,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
}

// LastBackup is the most recent Velero backup the controller knows about. The
// on-demand backup/restore *orders* live on VClusterVeleroOps; this is only the
// summary worth seeing next to the vcluster itself.
type LastBackup struct {
	// +optional
	Name string `json:"name,omitempty"`
	// +optional
	Phase string `json:"phase,omitempty"`
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

// DeletionStatus replaces the on-disk deleting.json and the ListCleaning()
// entries: the same information, but on the object it describes, so it survives
// a restart of the process instead of living in a file next to it.
type DeletionStatus struct {
	// GracePeriodEndsAt is when the reversible window closes. Until then, a
	// `git revert` of the suspend commit brings the vcluster back untouched.
	// +optional
	GracePeriodEndsAt *metav1.Time `json:"gracePeriodEndsAt,omitempty"`
	// Stage is how far the finalizer's sequence has got.
	// +optional
	Stage string `json:"stage,omitempty"`
	// Message explains a blocked deletion — typically deletionProtection still
	// true when the CR was removed (crd-vcluster.md §4.3).
	// +optional
	Message string `json:"message,omitempty"`
}

// VClusterStatus is written only by the operator, through the /status subresource.
type VClusterStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	Phase VClusterPhase `json:"phase,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// +optional
	ChartVersion string `json:"chartVersion,omitempty"`
	// +optional
	K8sVersion string `json:"k8sVersion,omitempty"`
	// +optional
	PodCount int32 `json:"podCount,omitempty"`
	// +optional
	ResourceUsage *ResourceUsage `json:"resourceUsage,omitempty"`
	// ProtectionEnabled mirrors the protect-deletion annotation on the host
	// namespace, so the state is readable without inspecting the namespace.
	// +optional
	ProtectionEnabled bool `json:"protectionEnabled,omitempty"`
	// +optional
	Rancher *RancherStatus `json:"rancher,omitempty"`
	// +optional
	Vault *VaultStatus `json:"vault,omitempty"`
	// +optional
	LastBackup *LastBackup `json:"lastBackup,omitempty"`
	// +optional
	Deletion *DeletionStatus `json:"deletion,omitempty"`
}

// VCluster is the source of truth for one vcluster: a small object, versioned in
// fluxprod, applied by Flux, expanded by the operator (ADR-001).
//
// Le nom « manager » est refusé à l'admission : tout ce qui est dérivé d'un nom
// de vcluster vaut "vcluster-" + nom, donc ce nom-là désigne le namespace de
// l'app et de l'opérateur. La règle est écrite en dur parce que CEL ne peut pas
// lire une constante Go — elle doit rester d'accord avec service.OperatorNamespace,
// et le test TestNameThatResolvesToTheOperatorNamespaceIsRefused garde l'autre
// moitié de la paire.
//
// La forme du nom est vérifiée au même endroit, pour la même raison : un CR
// refusé par le contrôleur après avoir passé l'admission laisse un
// `Accepted=False` qui traîne, plutôt qu'un refus net au `kubectl apply`. La
// règle doit rester d'accord avec service.ValidName (internal/service/vcluster.go) —
// audit N6, TODO.md.
//
// 54 n'est pas arbitraire : un namespace Kubernetes est une étiquette DNS
// RFC 1123, plafonnée à 63 caractères. Le préfixe "vcluster-" en consomme 9,
// il reste donc 63 - 9 = 54 pour le nom du vcluster lui-même. `{0,53}` après le
// premier caractère obligatoire fait bien 1 + 53 = 54 au total.
//
// +kubebuilder:validation:XValidation:rule="self.metadata.name != 'manager'",message="nom réservé : « vcluster-manager » est le namespace de l'application, un vcluster portant ce nom ferait de l'opérateur la cible de ses propres sauvegardes et suppressions"
// +kubebuilder:validation:XValidation:rule="self.metadata.name.matches('^[a-z][a-z0-9-]{0,53}$')",message="nom invalide : doit commencer par une lettre minuscule, ne contenir ensuite que des lettres minuscules, des chiffres et des tirets, et ne pas dépasser 54 caractères — vcluster-<nom> doit tenir dans la limite de 63 caractères d'un nom de namespace Kubernetes"
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=vc
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Owner",type=string,JSONPath=`.spec.owner`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Protégé",type=boolean,JSONPath=`.spec.deletionProtection`
// +kubebuilder:printcolumn:name="Âge",type=date,JSONPath=`.metadata.creationTimestamp`
type VCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VClusterSpec   `json:"spec,omitempty"`
	Status VClusterStatus `json:"status,omitempty"`
}

// VClusterList is the list form of VCluster.
//
// +kubebuilder:object:root=true
type VClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&VCluster{}, &VClusterList{})
}
