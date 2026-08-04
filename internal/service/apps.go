package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strings"

	"github.com/gmalfray/vcluster-manager/internal/audit"
	"github.com/gmalfray/vcluster-manager/internal/gitops"
	"github.com/gmalfray/vcluster-manager/internal/models"
)

// Apps-domain sentinel errors. Each adapter maps them to its own transport
// (web = toast + status ; REST = HTTP status + JSON body), same as the
// generic ErrForbidden/ErrK8sUnavailable sentinels.
var (
	// ErrAppInvalidTarget means the migration target vcluster is empty or is
	// the source itself.
	ErrAppInvalidTarget = errors.New("invalid target vcluster")
	// ErrAppMissingFilePath means the Application manifest path was not provided.
	ErrAppMissingFilePath = errors.New("missing app file path")
	// ErrAppGitLabUnavailable means no GitLab client is configured (app-manifests
	// repos live in GitLab).
	ErrAppGitLabUnavailable = errors.New("gitlab client unavailable")
)

// MigrateOpError wraps an operational failure of MigrateApp with enough
// context for the adapter to rebuild the exact toast the pre-extraction
// handler produced. Only used for genuine runtime failures, not for the guard
// sentinels above.
type MigrateOpError struct {
	// Stage identifies where the migration failed: "list-source", "read-file"
	// or "commit-target".
	Stage string
	// File is the offending manifest path (set for the "read-file" stage).
	File string
	// Err is the underlying error.
	Err error
}

func (e *MigrateOpError) Error() string {
	if e.File != "" {
		return fmt.Sprintf("migrate app: %s (%s): %v", e.Stage, e.File, e.Err)
	}
	return fmt.Sprintf("migrate app: %s: %v", e.Stage, e.Err)
}

func (e *MigrateOpError) Unwrap() error { return e.Err }

// MigrateResult reports the outcome of an app migration. When
// DeleteSourceFailed is true the manifests were copied to the target but the
// optional cleanup of the source failed — the migration still counts as done,
// so no error is returned; the adapter surfaces a warning toast instead.
type MigrateResult struct {
	AppName       string `json:"app_name"`
	SourceName    string `json:"source_name"`
	TargetName    string `json:"target_name"`
	FilesMigrated int    `json:"files_migrated"`

	DeleteSourceRequested bool   `json:"delete_source_requested"`
	DeleteSourceFailed    bool   `json:"delete_source_failed"`
	DeleteSourceError     string `json:"delete_source_error,omitempty"`
}

// appManifestBranch maps an environment to its app-manifests branch.
func appManifestBranch(env string) string {
	if env == "prod" {
		return "master"
	}
	return "preprod"
}

// MigrationTargets returns the vclusters eligible as migration targets for the
// given source in the given environment: ArgoCD-enabled vclusters other than
// the source itself. Read-only, no privilege required.
func (s *Service) MigrationTargets(ctx context.Context, name, env string) []string {
	env = envOrDefault(env)
	vclusters, _ := s.parser.ListVClusters(ctx, env)
	var targets []string
	for _, vc := range vclusters {
		if vc.ArgoCD && vc.Name != name {
			targets = append(targets, vc.Name)
		}
	}
	return targets
}

// GetApps returns the ArgoCD Applications of a vcluster together with the
// source they were read from ("live" when queried directly from the
// vcluster API, "repo" when read back from the app-manifests GitLab repo, ""
// when nothing could be listed). Read-only, no privilege required.
//
// The returned apps are raw: the caller annotates in-flight migration state
// (a handlers-level concern, tracked in memory by the web adapter) before
// rendering.
//
// The error return exists for symmetry with the REST projection but is
// always nil today: an unreachable vcluster falls back to the repo, and a
// missing/unreadable repo yields an empty list, mirroring the
// pre-extraction handler which always rendered a (possibly empty) fragment.
func (s *Service) GetApps(ctx context.Context, name, env string) ([]models.ArgoApp, string, error) {
	env = envOrDefault(env)

	// Source of truth: Application objects read directly from the vcluster.
	if k8s := s.k8sForEnv(env); k8s != nil {
		apps, err := k8s.ListVClusterArgoApps(ctx, name)
		if err == nil {
			return apps, "live", nil
		}
		slog.Warn("GetApps: vcluster API unavailable, falling back to repo", "vcluster", name, "err", err)
	}

	// Fallback: reconstruct the apps from the app-manifests GitLab repo.
	if s.gitlab == nil {
		return nil, "", nil
	}

	branch := appManifestBranch(env)
	files, err := s.gitlab.ListAppManifestFiles(name, branch)
	if err != nil {
		slog.Error("GetApps: list files failed", "vcluster", name, "err", err)
		return nil, "", nil
	}

	var apps []models.ArgoApp
	for _, filePath := range files {
		content, err := s.gitlab.GetAppManifestFile(name, branch, filePath)
		if err != nil {
			slog.Warn("GetApps: get file failed", "path", filePath, "err", err)
			continue
		}
		apps = append(apps, gitops.ParseArgoApps(filePath, content)...)
	}
	return apps, "repo", nil
}

// MigrateApp copies an ArgoCD Application (and every manifest sharing its
// directory) from one vcluster's app-manifests to another, in a single
// atomic GitLab commit, optionally deleting the originals from the source.
// Admin only (RBAC enforced here, so both adapters inherit it).
//
// Guard failures return the ErrApp* sentinels; runtime failures return a
// *MigrateOpError. A successful copy whose optional source cleanup failed
// returns a result with DeleteSourceFailed set and a nil error.
func (s *Service) MigrateApp(ctx context.Context, actor models.Actor, sourceName, env, appName, filePath, targetName string, deleteSource bool) (MigrateResult, error) {
	if !actor.IsAdmin {
		return MigrateResult{}, ErrForbidden
	}
	env = envOrDefault(env)

	if targetName == "" || targetName == sourceName {
		return MigrateResult{}, ErrAppInvalidTarget
	}
	if filePath == "" {
		return MigrateResult{}, ErrAppMissingFilePath
	}
	if s.gitlab == nil {
		return MigrateResult{}, ErrAppGitLabUnavailable
	}

	branch := appManifestBranch(env)

	// Determine which files to migrate: every file in the Application
	// manifest's directory (or just the file itself when it lives at the
	// repo root).
	dir := path.Dir(filePath)
	allFiles, err := s.gitlab.ListAppManifestFiles(sourceName, branch)
	if err != nil {
		return MigrateResult{}, &MigrateOpError{Stage: "list-source", Err: err}
	}

	var dirFiles []string
	if dir == "." {
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

	// List files already present in the target to decide create vs update.
	existingInTarget := map[string]bool{}
	if targetFiles, err2 := s.gitlab.ListAppManifestFiles(targetName, branch); err2 == nil {
		for _, f := range targetFiles {
			existingInTarget[f] = true
		}
	}

	// Read source files and build per-file commit actions.
	var commitActions []gitops.CommitAction
	for _, f := range dirFiles {
		content, err := s.gitlab.GetAppManifestFile(sourceName, branch, f)
		if err != nil {
			return MigrateResult{}, &MigrateOpError{Stage: "read-file", File: f, Err: err}
		}
		action := "create"
		if existingInTarget[f] {
			action = "update"
		}
		commitActions = append(commitActions, gitops.CommitAction{Action: action, Path: f, Content: content})
	}

	// Commit to the target in a single atomic commit.
	commitMsg := fmt.Sprintf("feat: migrate app %s from %s (%d files)", appName, sourceName, len(dirFiles))
	if err = s.gitlab.CommitToAppManifests(targetName, branch, commitMsg, commitActions); err != nil {
		return MigrateResult{}, &MigrateOpError{Stage: "commit-target", Err: err}
	}

	audit.LogActor(actor.Username, "migrate-app", appName, env, "target", targetName)

	result := MigrateResult{
		AppName:               appName,
		SourceName:            sourceName,
		TargetName:            targetName,
		FilesMigrated:         len(dirFiles),
		DeleteSourceRequested: deleteSource,
	}

	// Optionally delete the migrated files from the source. A failure here
	// does not undo the migration: report it as a soft warning via the result.
	if deleteSource {
		var delActions []gitops.CommitAction
		for _, f := range dirFiles {
			delActions = append(delActions, gitops.CommitAction{Action: "delete", Path: f})
		}
		delMsg := fmt.Sprintf("feat: remove migrated app %s (%d files)", appName, len(dirFiles))
		if err := s.gitlab.CommitToAppManifests(sourceName, branch, delMsg, delActions); err != nil {
			result.DeleteSourceFailed = true
			result.DeleteSourceError = err.Error()
		}
	}

	return result, nil
}

// CreateAppManifestsRepo creates the app-manifests GitLab repo backing a
// vcluster's ArgoCD Applications. Admin only. env is only used for the audit
// entry — repo creation itself is not environment-scoped.
func (s *Service) CreateAppManifestsRepo(ctx context.Context, actor models.Actor, name, env string) (int64, error) {
	if !actor.IsAdmin {
		return 0, ErrForbidden
	}
	if s.gitlab == nil {
		return 0, ErrAppGitLabUnavailable
	}

	projectID, err := s.gitlab.CreateAppManifestsRepo(name)
	if err != nil {
		return 0, err
	}

	audit.LogActor(actor.Username, "create-app-manifests-repo", name, envOrDefault(env))

	return projectID, nil
}
