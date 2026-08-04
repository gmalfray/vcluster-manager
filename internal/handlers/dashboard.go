package handlers

import (
	"net/http"
)

// Dashboard renders the home page: vclusters grouped by environment, summary
// cards and the release/version banner. The aggregation itself lives in
// service.GetDashboard; this handler only turns the result into template data.
func (h *Handlers) Dashboard(w http.ResponseWriter, r *http.Request) {
	dash, _ := h.svc.GetDashboard(r.Context()) // error is always nil, see GetDashboard doc

	data := map[string]interface{}{
		"Groups": dash.Groups,
		"User":   h.getUser(r),
		// Summary cards
		"SummaryTotalPreprod":  dash.SummaryTotalPreprod,
		"SummaryTotalProd":     dash.SummaryTotalProd,
		"SummaryTotal":         dash.SummaryTotal,
		"SummaryArgoCDCount":   dash.SummaryArgoCDCount,
		"SummaryNoArgoCDCount": dash.SummaryNoArgoCDCount,
		"SummaryBackupCount":   dash.SummaryBackupCount,
		"SummaryNoBackupCount": dash.SummaryNoBackupCount,
		"SummaryPendingCount":  dash.SummaryPendingCount,
	}

	if dash.LatestRelease != nil {
		data["LatestRelease"] = dash.LatestRelease
	}

	if dash.HelmUpdaterEnabled {
		data["HelmUpdaterEnabled"] = true
		if dash.PreprodChartVersion != "" {
			data["PreprodChartVersion"] = dash.PreprodChartVersion
		}
		if dash.PreprodK8sVersion != "" {
			data["PreprodK8sVersion"] = dash.PreprodK8sVersion
		}
		if dash.ProdChartVersion != "" {
			data["ProdChartVersion"] = dash.ProdChartVersion
		}
		if dash.ProdK8sVersion != "" {
			data["ProdK8sVersion"] = dash.ProdK8sVersion
		}
		if dash.PendingChartMR != nil {
			data["PendingChartMR"] = dash.PendingChartMR
		}
		if dash.PendingK8sMR != nil {
			data["PendingK8sMR"] = dash.PendingK8sMR
		}
	}

	if dash.K8sVersions != nil {
		data["K8sVersions"] = dash.K8sVersions
	}
	if dash.LatestArgoCDRelease != nil {
		data["LatestArgoCDRelease"] = dash.LatestArgoCDRelease
	}

	if dash.ArgoCDUpdaterEnabled {
		data["ArgoCDUpdaterEnabled"] = true
		if dash.PreprodArgoCDVersion != "" {
			data["PreprodArgoCDVersion"] = dash.PreprodArgoCDVersion
		}
		if dash.ProdArgoCDVersion != "" {
			data["ProdArgoCDVersion"] = dash.ProdArgoCDVersion
		}
		if dash.PendingArgoCDMR != nil {
			data["PendingArgoCDMR"] = dash.PendingArgoCDMR
		}
	}

	h.render(w, "dashboard.html", data)
}
