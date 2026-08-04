package service

import (
	"context"
	"errors"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gmalfray/vcluster-manager/internal/gitops"
	"github.com/gmalfray/vcluster-manager/internal/kubernetes"
	"github.com/gmalfray/vcluster-manager/internal/models"
)

// newAppsTestService wires a Service whose parser reads vcluster config
// through a fake GitLab server (reusing the fakeGitLab test double from
// dashboard_test.go) — enough to exercise MigrationTargets against real
// listing/parsing logic instead of stubbing it away.
func newAppsTestService(t *testing.T, fg *fakeGitLab) *Service {
	t.Helper()
	srv := httptest.NewServer(fg)
	t.Cleanup(srv.Close)

	gl, err := gitops.NewGitLabClient(gitops.GitLabClientConfig{URL: srv.URL, Token: "test-token", ProjectID: "1"})
	if err != nil {
		t.Fatalf("gitops.NewGitLabClient: %v", err)
	}
	t.Cleanup(gl.Close)

	parser := gitops.NewParser()
	parser.SetGitLabClient(gl)

	var mu sync.RWMutex
	return New(Deps{
		Parser:       parser,
		K8sClients:   map[string]*kubernetes.StatusClient{},
		K8sClientsMu: &mu,
	})
}

func TestMigrateApp_ForbiddenForNonAdmin(t *testing.T) {
	s := newTestService()
	_, err := s.MigrateApp(context.Background(), models.Actor{Username: "bob", IsAdmin: false}, "source", "preprod", "myapp", "apps/myapp.yaml", "target", false)
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected ErrForbidden for non-admin, got %v", err)
	}
}

func TestMigrateApp_RejectsInvalidSourceName(t *testing.T) {
	s := newTestService()
	_, err := s.MigrateApp(context.Background(), models.Actor{Username: "alice", IsAdmin: true}, "../etc/passwd", "preprod", "myapp", "apps/myapp.yaml", "target", false)
	if !errors.Is(err, ErrInvalidName) {
		t.Fatalf("expected ErrInvalidName for a malformed source name, got %v", err)
	}
}

func TestMigrateApp_RejectsInvalidTargetName(t *testing.T) {
	s := newTestService()
	_, err := s.MigrateApp(context.Background(), models.Actor{Username: "alice", IsAdmin: true}, "source", "preprod", "myapp", "apps/myapp.yaml", "../etc/passwd", false)
	if !errors.Is(err, ErrInvalidName) {
		t.Fatalf("expected ErrInvalidName for a malformed target name, got %v", err)
	}
}

func TestMigrateApp_InvalidTarget_Empty(t *testing.T) {
	s := newTestService()
	_, err := s.MigrateApp(context.Background(), models.Actor{Username: "alice", IsAdmin: true}, "source", "preprod", "myapp", "apps/myapp.yaml", "", false)
	if !errors.Is(err, ErrAppInvalidTarget) {
		t.Fatalf("expected ErrAppInvalidTarget for empty target, got %v", err)
	}
}

func TestMigrateApp_InvalidTarget_SameAsSource(t *testing.T) {
	s := newTestService()
	_, err := s.MigrateApp(context.Background(), models.Actor{Username: "alice", IsAdmin: true}, "source", "preprod", "myapp", "apps/myapp.yaml", "source", false)
	if !errors.Is(err, ErrAppInvalidTarget) {
		t.Fatalf("expected ErrAppInvalidTarget when target equals source, got %v", err)
	}
}

func TestMigrateApp_MissingFilePath(t *testing.T) {
	s := newTestService()
	_, err := s.MigrateApp(context.Background(), models.Actor{Username: "alice", IsAdmin: true}, "source", "preprod", "myapp", "", "target", false)
	if !errors.Is(err, ErrAppMissingFilePath) {
		t.Fatalf("expected ErrAppMissingFilePath for empty file path, got %v", err)
	}
}

func TestMigrateApp_GitLabUnavailable(t *testing.T) {
	s := newTestService()
	_, err := s.MigrateApp(context.Background(), models.Actor{Username: "alice", IsAdmin: true}, "source", "preprod", "myapp", "apps/myapp.yaml", "target", false)
	if !errors.Is(err, ErrAppGitLabUnavailable) {
		t.Fatalf("expected ErrAppGitLabUnavailable when no GitLab client is configured, got %v", err)
	}
}

// Guard order matters: target validity is checked before the file path, and
// both are checked before the GitLab client — mirrors the pre-extraction
// handler so the adapter always surfaces the same first error.
func TestMigrateApp_GuardOrder_TargetBeforeFilePath(t *testing.T) {
	s := newTestService()
	_, err := s.MigrateApp(context.Background(), models.Actor{Username: "alice", IsAdmin: true}, "source", "preprod", "myapp", "", "", false)
	if !errors.Is(err, ErrAppInvalidTarget) {
		t.Fatalf("expected ErrAppInvalidTarget to be checked before the file path, got %v", err)
	}
}

func TestCreateAppManifestsRepo_ForbiddenForNonAdmin(t *testing.T) {
	s := newTestService()
	_, err := s.CreateAppManifestsRepo(context.Background(), models.Actor{Username: "bob", IsAdmin: false}, "demo", "preprod")
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected ErrForbidden for non-admin, got %v", err)
	}
}

func TestCreateAppManifestsRepo_GitLabUnavailable(t *testing.T) {
	s := newTestService()
	_, err := s.CreateAppManifestsRepo(context.Background(), models.Actor{Username: "alice", IsAdmin: true}, "demo", "preprod")
	if !errors.Is(err, ErrAppGitLabUnavailable) {
		t.Fatalf("expected ErrAppGitLabUnavailable when no GitLab client is configured, got %v", err)
	}
}

func TestGetApps_NoClientNoGitLab_ReturnsEmpty(t *testing.T) {
	s := newTestService()
	apps, source, err := s.GetApps(context.Background(), "demo", "")
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if apps != nil {
		t.Fatalf("expected no apps, got %v", apps)
	}
	if source != "" {
		t.Fatalf("expected empty source when nothing could be listed, got %q", source)
	}
}

func TestMigrationTargets_ExcludesSourceAndNonArgoCD(t *testing.T) {
	fg := newFakeGitLab()
	fg.addTree("preprod", "clusters/preprod/vclusters",
		"clusters/preprod/vclusters/source/values.yaml",
		"clusters/preprod/vclusters/argo-target/values.yaml",
		"clusters/preprod/vclusters/no-argo/values.yaml",
	)
	fg.addFile("preprod", "clusters/preprod/vclusters/source/values.yaml", "")
	fg.addFile("preprod", "clusters/preprod/vclusters/argo-target/values.yaml", "")
	fg.addFile("preprod", "clusters/preprod/vclusters/no-argo/values.yaml", "")
	// Only argo-target and (irrelevant, since it's the source) source have an
	// ArgoCD tenant directory.
	fg.addTree("preprod", "clusters/preprod/vclusters/source/tenant/argocd", "clusters/preprod/vclusters/source/tenant/argocd/kustomization.yaml")
	fg.addTree("preprod", "clusters/preprod/vclusters/argo-target/tenant/argocd", "clusters/preprod/vclusters/argo-target/tenant/argocd/kustomization.yaml")

	s := newAppsTestService(t, fg)
	targets := s.MigrationTargets(context.Background(), "source", "preprod")

	if len(targets) != 1 || targets[0] != "argo-target" {
		t.Fatalf("expected [argo-target] (ArgoCD-enabled, not the source), got %v", targets)
	}
}

func TestMigrationTargets_EmptyEnvDefaultsToPreprod(t *testing.T) {
	fg := newFakeGitLab()
	fg.addTree("preprod", "clusters/preprod/vclusters", "clusters/preprod/vclusters/argo-target/values.yaml")
	fg.addFile("preprod", "clusters/preprod/vclusters/argo-target/values.yaml", "")
	fg.addTree("preprod", "clusters/preprod/vclusters/argo-target/tenant/argocd", "clusters/preprod/vclusters/argo-target/tenant/argocd/kustomization.yaml")

	s := newAppsTestService(t, fg)
	targets := s.MigrationTargets(context.Background(), "source", "")

	if len(targets) != 1 || targets[0] != "argo-target" {
		t.Fatalf("expected empty env to default to preprod and find argo-target, got %v", targets)
	}
}

func TestAppManifestBranch(t *testing.T) {
	if got := appManifestBranch("prod"); got != "master" {
		t.Fatalf("expected master branch for prod, got %q", got)
	}
	if got := appManifestBranch("preprod"); got != "preprod" {
		t.Fatalf("expected preprod branch for preprod, got %q", got)
	}
	if got := appManifestBranch(""); got != "preprod" {
		t.Fatalf("expected preprod branch as default, got %q", got)
	}
}
