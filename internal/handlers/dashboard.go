package handlers

import (
	"log/slog"
	"net/http"

	"github.com/gmalfray/vcluster-manager/internal/models"
)

type envGroup struct {
	Env   string
	Label string
	Items []models.DashboardItem
}

func (h *Handlers) Dashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var groups []envGroup

	// Load preprod vclusters (from preprod branch)
	preprodVClusters, err := h.parser.ListVClusters(ctx, "preprod")
	if err != nil {
		slog.Warn("error listing vclusters", "env", "preprod", "err", err)
	}

	// Load prod vclusters config (from preprod branch, clusters/prod/)
	prodVClusters, err := h.parser.ListVClusters(ctx, "prod")
	if err != nil {
		slog.Warn("error listing vclusters", "env", "prod", "err", err)
	}

	// Check what's actually deployed on master branch
	masterNames := map[string]bool{}
	for _, name := range h.parser.ListVClusterNamesOnBranch(ctx, "master", "prod") {
		masterNames[name] = true
	}

	// Build preprod items
	var preprodItems []models.DashboardItem
	for _, vc := range preprodVClusters {
		item := models.DashboardItem{
			VCluster: vc,
			APIHost:  vc.Name + ".api." + h.cfg.BaseDomainPreprod,
		}
		if vc.ArgoCD {
			item.ArgoURL = "https://argocd." + vc.Name + "." + h.cfg.BaseDomainPreprod
		}
		preprodItems = append(preprodItems, item)
	}

	// Fetch open MR URL once for all pending items
	var openMRURL string
	if h.gitlab != nil {
		openMRURL, _, _ = h.gitlab.GetOpenPreprodMRInfo()
	}

	// Build prod items: mark as pending if not on master
	var prodItems []models.DashboardItem
	for _, vc := range prodVClusters {
		item := models.DashboardItem{
			VCluster: vc,
			APIHost:  vc.Name + ".api." + h.cfg.BaseDomainProd,
		}
		if vc.ArgoCD {
			item.ArgoURL = "https://argocd." + vc.Name + "." + h.cfg.BaseDomainProd
		}
		if !masterNames[vc.Name] {
			item.PendingMR = true
			item.PendingMRURL = openMRURL
		}
		prodItems = append(prodItems, item)
	}

	// Merge deleting entries: mark existing items or create synthetic items
	deletingEntries := h.cfg.ListDeleting()
	for _, de := range deletingEntries {
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
			// Files already deleted from repo, create synthetic item
			synthetic := models.DashboardItem{
				VCluster:   models.VCluster{Name: de.Name, Env: de.Env},
				Deleting:   true,
				DeletingMR: de.MRURL,
			}
			*items = append(*items, synthetic)
		}
	}

	// Merge cleaning entries: mark existing items or create synthetic items
	for _, ce := range h.cfg.ListCleaning() {
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
		groups = append(groups, envGroup{Env: "preprod", Label: h.cfg.ClusterLabel("preprod"), Items: preprodItems})
	}
	if len(prodItems) > 0 {
		groups = append(groups, envGroup{Env: "prod", Label: h.cfg.ClusterLabel("prod"), Items: prodItems})
	}

	// Compute summary stats
	type envStat struct {
		total  int
		argocd int
		backup int
	}
	computeStat := func(items []models.DashboardItem) envStat {
		s := envStat{total: len(items)}
		for _, it := range items {
			if it.VCluster.ArgoCD {
				s.argocd++
			}
			if it.VCluster.Velero.Enabled {
				s.backup++
			}
		}
		return s
	}
	pp := computeStat(preprodItems)
	pr := computeStat(prodItems)
	pendingCount := 0
	for _, it := range prodItems {
		if it.PendingMR {
			pendingCount++
		}
	}

	data := map[string]interface{}{
		"Groups": groups,
		"User":   h.getUser(r),
		// Summary cards
		"SummaryTotalPreprod":  pp.total,
		"SummaryTotalProd":     pr.total,
		"SummaryTotal":         pp.total + pr.total,
		"SummaryArgoCDCount":   pp.argocd + pr.argocd,
		"SummaryNoArgoCDCount": (pp.total - pp.argocd) + (pr.total - pr.argocd),
		"SummaryBackupCount":   pp.backup + pr.backup,
		"SummaryNoBackupCount": (pp.total - pp.backup) + (pr.total - pr.backup),
		"SummaryPendingCount":  pendingCount,
	}

	if h.ghReleases != nil {
		if release, err := h.ghReleases.GetLatestVClusterRelease(); err == nil {
			data["LatestRelease"] = release
		} else {
			slog.Warn("could not fetch latest vcluster release", "err", err)
		}
	}

	if h.helmUpdater != nil {
		data["HelmUpdaterEnabled"] = true
		// Preprod (branch preprod)
		if version, err := h.helmUpdater.GetCurrentChartVersion(ctx, "preprod"); err == nil {
			data["PreprodChartVersion"] = version
		} else {
			slog.Warn("could not fetch chart version", "branch", "preprod", "err", err)
		}
		if k8s, err := h.helmUpdater.GetDefaultK8sVersion(ctx, "preprod"); err == nil {
			data["PreprodK8sVersion"] = k8s
		} else {
			slog.Warn("could not fetch K8s version", "branch", "preprod", "err", err)
		}
		// Prod (branch master)
		if version, err := h.helmUpdater.GetCurrentChartVersion(ctx, "master"); err == nil {
			data["ProdChartVersion"] = version
		} else {
			slog.Warn("could not fetch chart version", "branch", "master", "err", err)
		}
		if k8s, err := h.helmUpdater.GetDefaultK8sVersion(ctx, "master"); err == nil {
			data["ProdK8sVersion"] = k8s
		} else {
			slog.Warn("could not fetch K8s version", "branch", "master", "err", err)
		}
		// Pending MRs
		if mr := h.helmUpdater.GetPendingChartMR(); mr != nil {
			data["PendingChartMR"] = mr
		}
		if mr := h.helmUpdater.GetPendingK8sMR(); mr != nil {
			data["PendingK8sMR"] = mr
		}
	}

	if h.ghReleases != nil {
		if versions, err := h.ghReleases.GetAvailableK8sVersions(); err == nil {
			data["K8sVersions"] = versions
		} else {
			slog.Warn("could not fetch available K8s versions", "err", err)
		}
		if release, err := h.ghReleases.GetLatestArgoCDRelease(); err == nil {
			data["LatestArgoCDRelease"] = release
		} else {
			slog.Warn("could not fetch latest ArgoCD release", "err", err)
		}
	}

	if h.argocdUpdater != nil {
		data["ArgoCDUpdaterEnabled"] = true
		if version, err := h.argocdUpdater.GetGlobalVersion(ctx, "preprod"); err == nil {
			data["PreprodArgoCDVersion"] = version
		} else {
			slog.Warn("could not fetch ArgoCD version", "branch", "preprod", "err", err)
		}
		if version, err := h.argocdUpdater.GetGlobalVersion(ctx, "master"); err == nil {
			data["ProdArgoCDVersion"] = version
		} else {
			slog.Warn("could not fetch ArgoCD version", "branch", "master", "err", err)
		}
		if mr := h.argocdUpdater.GetPendingMR(); mr != nil {
			data["PendingArgoCDMR"] = mr
		}
	}

	h.render(w, "dashboard.html", data)
}
