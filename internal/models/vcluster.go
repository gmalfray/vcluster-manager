package models

// VCluster represents a vcluster configuration parsed from the fluxprod repo.
type VCluster struct {
	Name             string
	Env              string // "preprod" or "prod"
	ArgoCD           bool
	RBACGroups       []string
	Velero           VeleroConfig
	Quotas           QuotaConfig
	NoQuotas         bool
	K8sVersionConfig string // controlPlane.distro.k8s.version from values.yaml (if set)
	ArgoCDVersion    string // per-vcluster ArgoCD version override (empty = global)
	FluxCD           FluxCDConfig
}

// FluxCDConfig holds the FluxCD bootstrap configuration for a vcluster.
type FluxCDConfig struct {
	Enabled bool
	RepoURL string // ssh://git@...
	Branch  string // e.g. master
	Path    string // e.g. clusters/pra2
}

type VeleroConfig struct {
	Enabled  bool
	Schedule string // raw cron expression
	Hour     int
	Minute   int
	TTL      string // e.g. "720h0m0s"
}

// VeleroBackupInfo holds info about a Velero Backup object.
type VeleroBackupInfo struct {
	Name           string
	Phase          string // Completed, Failed, InProgress, ...
	StartTime      string
	CompletionTime string
	ItemsBackedUp  int
	TotalItems     int
	Namespace      string // included namespace
	TTL            string
}

// VeleroRestoreInfo holds info about a Velero Restore object.
type VeleroRestoreInfo struct {
	Name       string
	Phase      string
	BackupName string
	StartTime  string
}

type QuotaConfig struct {
	CPU     string
	Memory  string
	Storage string
}

// CreateRequest represents a request to create a new vcluster.
type CreateRequest struct {
	Name          string   `json:"name"`
	ArgoCD        bool     `json:"argocd"`
	RBACGroups    []string `json:"rbac_groups"`
	VeleroEnabled bool     `json:"velero_enabled"`
	VeleroHour    string   `json:"velero_hour"` // "HH:MM"
	VeleroTTL     string   `json:"velero_ttl"`  // e.g. "720h0m0s"
	CPU           string   `json:"cpu"`
	Memory        string   `json:"memory"`
	Storage       string   `json:"storage"`
	NoQuotas      bool     `json:"no_quotas"`
	ArgoCDVersion string   `json:"argocd_version"`
	FluxCDEnabled bool     `json:"fluxcd_enabled"`
	FluxCDRepoURL string   `json:"fluxcd_repo_url"`
	FluxCDBranch  string   `json:"fluxcd_branch"`
	FluxCDPath    string   `json:"fluxcd_path"`
}

// UpdateRequest represents a request to update a vcluster's settings.
type UpdateRequest struct {
	VeleroEnabled bool     `json:"velero_enabled"`
	VeleroHour    string   `json:"velero_hour"`
	VeleroTTL     string   `json:"velero_ttl"`
	CPU           string   `json:"cpu"`
	Memory        string   `json:"memory"`
	Storage       string   `json:"storage"`
	NoQuotas      bool     `json:"no_quotas"`
	RBACGroups    []string `json:"rbac_groups"`
	K8sVersion    string   `json:"k8s_version"`
	ArgoCDVersion string   `json:"argocd_version"`
	FluxCDEnabled bool     `json:"fluxcd_enabled"`
	FluxCDRepoURL string   `json:"fluxcd_repo_url"`
	FluxCDBranch  string   `json:"fluxcd_branch"`
	FluxCDPath    string   `json:"fluxcd_path"`
}

// StatusInfo holds real-time status from the Kubernetes cluster.
type StatusInfo struct {
	HelmRelease       string `json:"helm_release"`       // Ready, NotReady, Unknown
	FluxKustomization string `json:"flux_kustomization"` // Applied, Failed, Unknown
	ChartVersion      string `json:"chart_version"`      // vcluster chart version from HelmRelease history
	K8sVersion        string `json:"k8s_version"`        // K8s server version inside the vcluster
	PodCount          int    `json:"pod_count"`
	CPUUsage          string `json:"cpu_usage"`
	MemoryUsage       string `json:"memory_usage"`
	StorageUsage      string `json:"storage_usage"`
	CPUPercent        int    `json:"cpu_percent"`
	MemoryPercent     int    `json:"memory_percent"`
	StoragePercent    int    `json:"storage_percent"`
	ProtectionEnabled bool   `json:"protection_enabled"`
}

// DashboardItem combines config and status for the dashboard view.
type DashboardItem struct {
	VCluster        VCluster
	Status          *StatusInfo
	APIHost         string
	ArgoURL         string
	ChartVersion    string // populated async via HTMX status fragment
	K8sVersion      string // populated async via HTMX status fragment
	PendingMR       bool   // true if vcluster exists in preprod but not yet merged to master
	PendingMRURL    string // URL of the open preprod→master MR
	Deleting        bool   // true if vcluster deletion is in progress (waiting for K8s reconciliation)
	DeletingMR      string // URL of the deletion MR for prod (if applicable)
	RancherCleaning bool   // true if rancher-cleanup job is running (pre-deletion step)
}

// ReleaseInfo holds information about the latest vcluster release from GitHub.
type ReleaseInfo struct {
	Tag         string
	PublishedAt string
}

// ArgoApp represents an ArgoCD Application found in an app-manifests repo.
type ArgoApp struct {
	Name           string
	Namespace      string // spec.destination.namespace
	FilePath       string // path in the app-manifests repo
	SourceRepoURL  string // spec.source.repoURL
	SourcePath     string // spec.source.path
	SourceBranch   string // spec.source.targetRevision
	Project        string // spec.project
	Migrating      bool   // true while a migration is in progress
	MigratingLabel string // e.g. "Migre vers platform" or "Depuis applications"
}
