package service

import (
	"context"
	"log/slog"

	"github.com/gmalfray/vcluster-manager/internal/argocd"
	"github.com/gmalfray/vcluster-manager/internal/helmcharts"
	"github.com/gmalfray/vcluster-manager/internal/models"
)

// DashboardGroup is one environment section of the home page: its env key, its
// human label and the vclusters it holds (config vclusters plus synthetic
// deleting/cleaning entries).
type DashboardGroup struct {
	Env   string                 `json:"env"`
	Label string                 `json:"label"`
	Items []models.DashboardItem `json:"items"`
}

// DashboardData is the aggregated home-page view: vclusters grouped by env,
// the summary cards, the release banner (latest vcluster/ArgoCD releases,
// available K8s versions) and the current platform chart/K8s/ArgoCD versions
// with any pending update MRs. It is the single result type shared by both
// adapters (the web layer spreads it into the template data, the REST layer
// serializes it).
//
// Every field is best-effort: the integration clients are optional and their
// per-field lookups are allowed to fail (logged, then skipped), leaving the
// zero value — exactly like the original handler, which simply omitted the
// map key on failure. Since Go templates treat an absent key and a zero
// value identically, the rendered page doesn't change.
type DashboardData struct {
	Groups []DashboardGroup `json:"groups"`

	// Summary cards.
	SummaryTotalPreprod  int `json:"summary_total_preprod"`
	SummaryTotalProd     int `json:"summary_total_prod"`
	SummaryTotal         int `json:"summary_total"`
	SummaryArgoCDCount   int `json:"summary_argocd_count"`
	SummaryNoArgoCDCount int `json:"summary_no_argocd_count"`
	SummaryBackupCount   int `json:"summary_backup_count"`
	SummaryNoBackupCount int `json:"summary_no_backup_count"`
	SummaryPendingCount  int `json:"summary_pending_count"`

	// Release banner.
	LatestRelease       *models.ReleaseInfo `json:"latest_release,omitempty"`
	LatestArgoCDRelease *models.ReleaseInfo `json:"latest_argocd_release,omitempty"`
	K8sVersions         []string            `json:"k8s_versions,omitempty"`

	// Helm updater (global vcluster chart + default K8s version).
	HelmUpdaterEnabled  bool                  `json:"helm_updater_enabled"`
	PreprodChartVersion string                `json:"preprod_chart_version,omitempty"`
	PreprodK8sVersion   string                `json:"preprod_k8s_version,omitempty"`
	ProdChartVersion    string                `json:"prod_chart_version,omitempty"`
	ProdK8sVersion      string                `json:"prod_k8s_version,omitempty"`
	PendingChartMR      *helmcharts.PendingMR `json:"pending_chart_mr,omitempty"`
	PendingK8sMR        *helmcharts.PendingMR `json:"pending_k8s_mr,omitempty"`

	// ArgoCD updater (global ArgoCD version).
	ArgoCDUpdaterEnabled bool              `json:"argocd_updater_enabled"`
	PreprodArgoCDVersion string            `json:"preprod_argocd_version,omitempty"`
	ProdArgoCDVersion    string            `json:"prod_argocd_version,omitempty"`
	PendingArgoCDMR      *argocd.PendingMR `json:"pending_argocd_mr,omitempty"`
}

// GetDashboard aggregates the home-page view: lists the preprod and prod
// vclusters, marks prod entries not yet merged to master as pending, merges
// in-flight deleting/cleaning entries, computes the summary cards and
// enriches everything with the release banner and platform versions.
//
// Read-only, no privilege required. The error return exists for symmetry with
// the other domains but is always nil today: every integration failure is
// tolerated and logged, same as the pre-extraction handler which always
// rendered a (possibly degraded) page.
func (s *Service) GetDashboard(ctx context.Context) (DashboardData, error) {
	var data DashboardData

	// Load preprod vclusters (from preprod branch).
	preprodVClusters, err := s.parser.ListVClusters(ctx, "preprod")
	if err != nil {
		slog.Warn("error listing vclusters", "env", "preprod", "err", err)
	}

	// Load prod vclusters config (from preprod branch, clusters/prod/).
	prodVClusters, err := s.parser.ListVClusters(ctx, "prod")
	if err != nil {
		slog.Warn("error listing vclusters", "env", "prod", "err", err)
	}

	// Check what's actually deployed on master branch.
	masterNames := map[string]bool{}
	for _, name := range s.parser.ListVClusterNamesOnBranch(ctx, "master", "prod") {
		masterNames[name] = true
	}

	// Build preprod items.
	var preprodItems []models.DashboardItem
	for _, vc := range preprodVClusters {
		item := models.DashboardItem{
			VCluster: vc,
			APIHost:  vc.Name + ".api." + s.cfg.BaseDomainPreprod,
		}
		if vc.ArgoCD {
			item.ArgoURL = "https://argocd." + vc.Name + "." + s.cfg.BaseDomainPreprod
		}
		preprodItems = append(preprodItems, item)
	}

	// Fetch open MR URL once for all pending items.
	var openMRURL string
	if s.gitlab != nil {
		openMRURL, _, _ = s.gitlab.GetOpenPreprodMRInfo()
	}

	// Build prod items: mark as pending if not on master.
	var prodItems []models.DashboardItem
	for _, vc := range prodVClusters {
		item := models.DashboardItem{
			VCluster: vc,
			APIHost:  vc.Name + ".api." + s.cfg.BaseDomainProd,
		}
		if vc.ArgoCD {
			item.ArgoURL = "https://argocd." + vc.Name + "." + s.cfg.BaseDomainProd
		}
		if !masterNames[vc.Name] {
			item.PendingMR = true
			item.PendingMRURL = openMRURL
		}
		prodItems = append(prodItems, item)
	}

	// Merge deleting entries: mark existing items or create synthetic items.
	for _, de := range s.cfg.ListDeleting() {
		found := false
		items := &preprodItems
		if de.Env == "prod" {
			items = &prodItems
		}
		for i := range *items {
			if (*items)[i].VCluster.Name == de.Name {
				(*items)[i].Deleting = true
				(*items)[i].DeletingMR = de.MRURL
				found = true
				break
			}
		}
		if !found {
			// Files already deleted from repo, create synthetic item.
			synthetic := models.DashboardItem{
				VCluster:   models.VCluster{Name: de.Name, Env: de.Env},
				Deleting:   true,
				DeletingMR: de.MRURL,
			}
			*items = append(*items, synthetic)
		}
	}

	// Merge cleaning entries: mark existing items or create synthetic items.
	for _, ce := range s.cfg.ListCleaning() {
		items := &preprodItems
		if ce.Env == "prod" {
			items = &prodItems
		}
		found := false
		for i := range *items {
			if (*items)[i].VCluster.Name == ce.Name {
				(*items)[i].RancherCleaning = true
				found = true
				break
			}
		}
		if !found {
			synthetic := models.DashboardItem{
				VCluster:        models.VCluster{Name: ce.Name, Env: ce.Env},
				RancherCleaning: true,
			}
			*items = append(*items, synthetic)
		}
	}

	if len(preprodItems) > 0 {
		data.Groups = append(data.Groups, DashboardGroup{Env: "preprod", Label: s.cfg.ClusterLabel("preprod"), Items: preprodItems})
	}
	if len(prodItems) > 0 {
		data.Groups = append(data.Groups, DashboardGroup{Env: "prod", Label: s.cfg.ClusterLabel("prod"), Items: prodItems})
	}

	// Compute summary stats.
	type envStat struct {
		total  int
		argocd int
		backup int
	}
	computeStat := func(items []models.DashboardItem) envStat {
		st := envStat{total: len(items)}
		for _, it := range items {
			if it.VCluster.ArgoCD {
				st.argocd++
			}
			if it.VCluster.Velero.Enabled {
				st.backup++
			}
		}
		return st
	}
	pp := computeStat(preprodItems)
	pr := computeStat(prodItems)
	pendingCount := 0
	for _, it := range prodItems {
		if it.PendingMR {
			pendingCount++
		}
	}

	data.SummaryTotalPreprod = pp.total
	data.SummaryTotalProd = pr.total
	data.SummaryTotal = pp.total + pr.total
	data.SummaryArgoCDCount = pp.argocd + pr.argocd
	data.SummaryNoArgoCDCount = (pp.total - pp.argocd) + (pr.total - pr.argocd)
	data.SummaryBackupCount = pp.backup + pr.backup
	data.SummaryNoBackupCount = (pp.total - pp.backup) + (pr.total - pr.backup)
	data.SummaryPendingCount = pendingCount

	if s.ghReleases != nil {
		if release, err := s.ghReleases.GetLatestVClusterRelease(); err == nil {
			data.LatestRelease = release
		} else {
			slog.Warn("could not fetch latest vcluster release", "err", err)
		}
	}

	if s.helmUpdater != nil {
		data.HelmUpdaterEnabled = true
		// Preprod (branch preprod).
		if version, err := s.helmUpdater.GetCurrentChartVersion(ctx, "preprod"); err == nil {
			data.PreprodChartVersion = version
		} else {
			slog.Warn("could not fetch chart version", "branch", "preprod", "err", err)
		}
		if k8s, err := s.helmUpdater.GetDefaultK8sVersion(ctx, "preprod"); err == nil {
			data.PreprodK8sVersion = k8s
		} else {
			slog.Warn("could not fetch K8s version", "branch", "preprod", "err", err)
		}
		// Prod (branch master).
		if version, err := s.helmUpdater.GetCurrentChartVersion(ctx, "master"); err == nil {
			data.ProdChartVersion = version
		} else {
			slog.Warn("could not fetch chart version", "branch", "master", "err", err)
		}
		if k8s, err := s.helmUpdater.GetDefaultK8sVersion(ctx, "master"); err == nil {
			data.ProdK8sVersion = k8s
		} else {
			slog.Warn("could not fetch K8s version", "branch", "master", "err", err)
		}
		// Pending MRs.
		if mr := s.helmUpdater.GetPendingChartMR(); mr != nil {
			data.PendingChartMR = mr
		}
		if mr := s.helmUpdater.GetPendingK8sMR(); mr != nil {
			data.PendingK8sMR = mr
		}
	}

	if s.ghReleases != nil {
		if versions, err := s.ghReleases.GetAvailableK8sVersions(); err == nil {
			data.K8sVersions = versions
		} else {
			slog.Warn("could not fetch available K8s versions", "err", err)
		}
		if release, err := s.ghReleases.GetLatestArgoCDRelease(); err == nil {
			data.LatestArgoCDRelease = release
		} else {
			slog.Warn("could not fetch latest ArgoCD release", "err", err)
		}
	}

	if s.argocdUpdater != nil {
		data.ArgoCDUpdaterEnabled = true
		if version, err := s.argocdUpdater.GetGlobalVersion(ctx, "preprod"); err == nil {
			data.PreprodArgoCDVersion = version
		} else {
			slog.Warn("could not fetch ArgoCD version", "branch", "preprod", "err", err)
		}
		if version, err := s.argocdUpdater.GetGlobalVersion(ctx, "master"); err == nil {
			data.ProdArgoCDVersion = version
		} else {
			slog.Warn("could not fetch ArgoCD version", "branch", "master", "err", err)
		}
		if mr := s.argocdUpdater.GetPendingMR(); mr != nil {
			data.PendingArgoCDMR = mr
		}
	}

	return data, nil
}
