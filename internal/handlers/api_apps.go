package handlers

import (
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"strings"

	"github.com/gmalfray/vcluster-manager/internal/gitops"
	"github.com/gmalfray/vcluster-manager/internal/models"
)

// ListApps returns an HTMX fragment listing ArgoCD Applications.
// It queries Application objects directly from the vcluster API (source of truth).
// Falls back to the app-manifests GitLab repo if the vcluster is unreachable.
func (h *Handlers) ListApps(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	env := r.URL.Query().Get("env")
	if env == "" {
		env = "preprod"
	}

	// List target vclusters (ArgoCD enabled, not the source) — needed for migration
	vclusters, _ := h.parser.ListVClusters(r.Context(), env)
	var targetVClusters []string
	for _, vc := range vclusters {
		if vc.ArgoCD && vc.Name != name {
			targetVClusters = append(targetVClusters, vc.Name)
		}
	}

	renderData := map[string]interface{}{
		"SourceVCluster":  name,
		"Env":             env,
		"User":            h.getUser(r),
		"TargetVClusters": targetVClusters,
	}

	// Try to read Application objects directly from the vcluster
	k8s := h.k8sForEnv(env)
	if k8s != nil {
		apps, err := k8s.ListVClusterArgoApps(r.Context(), name)
		if err == nil {
			// Annotate with migration state
			for i := range apps {
				if label := h.getMigrationLabel(env, name, apps[i].Name); label != "" {
					apps[i].Migrating = true
					apps[i].MigratingLabel = label
				}
			}
			renderData["Apps"] = apps
			renderData["AppsSource"] = "live"
			h.renderPartial(w, "apps_list.html", renderData)
			return
		}
		slog.Warn("ListApps: vcluster API unavailable, falling back to repo", "vcluster", name, "err", err)
	}

	// Fallback: read from app-manifests GitLab repo
	if h.gitlab == nil {
		h.renderPartial(w, "apps_list.html", renderData)
		return
	}

	branch := "preprod"
	if env == "prod" {
		branch = "master"
	}

	files, err := h.gitlab.ListAppManifestFiles(name, branch)
	if err != nil {
		slog.Error("ListApps: list files failed", "vcluster", name, "err", err)
		h.renderPartial(w, "apps_list.html", renderData)
		return
	}

	var apps []models.ArgoApp
	for _, filePath := range files {
		content, err := h.gitlab.GetAppManifestFile(name, branch, filePath)
		if err != nil {
			slog.Warn("ListApps: get file failed", "path", filePath, "err", err)
			continue
		}
		apps = append(apps, gitops.ParseArgoApps(filePath, content)...)
	}

	for i := range apps {
		if label := h.getMigrationLabel(env, name, apps[i].Name); label != "" {
			apps[i].Migrating = true
			apps[i].MigratingLabel = label
		}
	}

	renderData["Apps"] = apps
	renderData["AppsSource"] = "repo"
	h.renderPartial(w, "apps_list.html", renderData)
}

// MigrateApp copies an ArgoCD Application from one vcluster's app-manifests to another (admin only).
func (h *Handlers) MigrateApp(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}

	sourceName := r.PathValue("name")
	env := r.URL.Query().Get("env")
	if env == "" {
		env = "preprod"
	}

	appName := r.FormValue("app_name")
	filePath := r.FormValue("app_file_path")
	targetName := r.FormValue("target_vcluster")
	deleteSource := r.FormValue("delete_source") != ""

	if targetName == "" || targetName == sourceName {
		h.renderToast(w, "error", "Vcluster cible invalide")
		return
	}
	if filePath == "" {
		h.renderToast(w, "error", "Chemin du fichier manquant")
		return
	}

	if h.gitlab == nil {
		h.renderToast(w, "error", "GitLab client non disponible")
		return
	}

	branch := "preprod"
	if env == "prod" {
		branch = "master"
	}

	// Determine which files to migrate: all files in the same directory as the Application manifest.
	dir := path.Dir(filePath)
	allFiles, err := h.gitlab.ListAppManifestFiles(sourceName, branch)
	if err != nil {
		h.renderToast(w, "error", fmt.Sprintf("Erreur liste fichiers source : %v", err))
		return
	}

	var dirFiles []string
	if dir == "." {
		// Application at repo root: migrate only this file
		dirFiles = []string{filePath}
	} else {
		prefix := dir + "/"
		for _, f := range allFiles {
			if strings.HasPrefix(f, prefix) {
				dirFiles = append(dirFiles, f)
			}
		}
		if len(dirFiles) == 0 {
			dirFiles = []string{filePath}
		}
	}

	// List files already in target to determine create vs update per file
	existingInTarget := map[string]bool{}
	if targetFiles, err2 := h.gitlab.ListAppManifestFiles(targetName, branch); err2 == nil {
		for _, f := range targetFiles {
			existingInTarget[f] = true
		}
	}

	// Read source files and build per-file commit actions
	var commitActions []gitops.CommitAction
	for _, f := range dirFiles {
		content, err := h.gitlab.GetAppManifestFile(sourceName, branch, f)
		if err != nil {
			h.renderToast(w, "error", fmt.Sprintf("Erreur lecture %s : %v", f, err))
			return
		}
		action := "create"
		if existingInTarget[f] {
			action = "update"
		}
		commitActions = append(commitActions, gitops.CommitAction{Action: action, Path: f, Content: content})
	}

	// Commit to target in a single atomic commit
	commitMsg := fmt.Sprintf("feat: migrate app %s from %s (%d files)", appName, sourceName, len(dirFiles))
	if err = h.gitlab.CommitToAppManifests(targetName, branch, commitMsg, commitActions); err != nil {
		h.renderToast(w, "error", fmt.Sprintf("Erreur migration vers %s : %v", targetName, err))
		return
	}

	// Optionally delete all directory files from source
	if deleteSource {
		var delActions []gitops.CommitAction
		for _, f := range dirFiles {
			delActions = append(delActions, gitops.CommitAction{Action: "delete", Path: f})
		}
		delMsg := fmt.Sprintf("feat: remove migrated app %s (%d files)", appName, len(dirFiles))
		if err := h.gitlab.CommitToAppManifests(sourceName, branch, delMsg, delActions); err != nil {
			h.renderToast(w, "warning", fmt.Sprintf("App migree vers %s mais erreur suppression source : %v", targetName, err))
			return
		}
	}

	h.addMigration(env, sourceName, targetName, appName)
	h.renderToast(w, "success", fmt.Sprintf("App %s migree vers %s (%d fichiers)", appName, targetName, len(dirFiles)))
}
