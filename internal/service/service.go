// Package service holds the business logic of vcluster-manager, decoupled from
// the HTTP transport and from HTML rendering. It never imports net/http and
// never writes to an http.ResponseWriter: methods take typed inputs (models
// DTOs + models.Actor) and return typed results + error.
//
// Two thin adapters consume it:
//   - internal/handlers (web): parses forms, calls the service, renders HTML/HTMX
//   - internal/api (REST):     decodes JSON, calls the service, encodes JSON
//
// This is the seam that makes the future operator/API split possible: the
// adapters change, the logic does not.
package service

import (
	"errors"
	"sync"
	"time"

	"github.com/gmalfray/vcluster-manager/internal/argocd"
	"github.com/gmalfray/vcluster-manager/internal/config"
	"github.com/gmalfray/vcluster-manager/internal/github"
	"github.com/gmalfray/vcluster-manager/internal/gitops"
	"github.com/gmalfray/vcluster-manager/internal/helmcharts"
	"github.com/gmalfray/vcluster-manager/internal/keycloak"
	"github.com/gmalfray/vcluster-manager/internal/kubernetes"
	"github.com/gmalfray/vcluster-manager/internal/notify"
	"github.com/gmalfray/vcluster-manager/internal/rancher"
	"github.com/gmalfray/vcluster-manager/internal/vault"
)

// Sentinel errors returned by the service. Each adapter maps them to its own
// transport (web = toast + status ; REST = HTTP status + JSON body).
var (
	// ErrForbidden means the actor lacks the required privilege (admin).
	ErrForbidden = errors.New("forbidden")
	// ErrK8sUnavailable means no Kubernetes client is configured for the env.
	ErrK8sUnavailable = errors.New("kubernetes client unavailable")
)

// Deps bundles everything the service needs. Using a struct (instead of a long
// positional constructor) keeps the signature stable as new domains are
// extracted and start needing more dependencies.
//
// K8sClients and K8sClientsMu MUST be the same instances used by the handlers
// layer (the map is mutated at runtime under the mutex), so pass the map value
// (reference type) and a pointer to the shared mutex.
type Deps struct {
	Cfg           *config.Config
	Parser        *gitops.Parser
	Generator     *gitops.Generator
	GitLab        *gitops.GitLabClient
	Keycloak      *keycloak.Client
	Rancher       *rancher.Client
	Vault         *vault.Client
	GHReleases    *github.ReleaseClient
	HelmUpdater   *helmcharts.Updater
	ArgoCDUpdater *argocd.Updater
	Notifier      *notify.Notifier
	K8sClients    map[string]*kubernetes.StatusClient
	K8sClientsMu  *sync.RWMutex
}

// Service is the aggregate entry point to the business logic. During the
// incremental extraction it starts small (only the domains already migrated)
// and grows as handlers are moved over.
type Service struct {
	cfg           *config.Config
	parser        *gitops.Parser
	generator     *gitops.Generator
	gitlab        *gitops.GitLabClient
	keycloak      *keycloak.Client
	rancher       *rancher.Client
	vault         *vault.Client
	ghReleases    *github.ReleaseClient
	helmUpdater   *helmcharts.Updater
	argocdUpdater *argocd.Updater
	notifier      *notify.Notifier

	k8sClients   map[string]*kubernetes.StatusClient
	k8sClientsMu *sync.RWMutex

	// veleroResumeMu/veleroResumeStates track, per in-place restore, whether
	// Flux has actually been resumed after the restore reached a terminal
	// phase. Written by resumeAfterInPlaceRestore (the background watcher)
	// and by GetVeleroRestoreStatus (the request-driven poll) — see
	// velero.go's veleroResumeState for why a shared record matters here.
	veleroResumeMu     sync.Mutex
	veleroResumeStates map[string]*veleroResumeState

	// resumeWatchInterval is how often resumeAfterInPlaceRestore polls. Zero
	// means the production value (10s). Only tests set it, so they don't have to
	// wait ten seconds to observe whether the watcher was started at all —
	// which is the difference RestoreHooks.OwnsFollowUp is there to make.
	resumeWatchInterval time.Duration
}

// New builds a Service from its dependencies.
func New(d Deps) *Service {
	return &Service{
		cfg:           d.Cfg,
		parser:        d.Parser,
		generator:     d.Generator,
		gitlab:        d.GitLab,
		keycloak:      d.Keycloak,
		rancher:       d.Rancher,
		vault:         d.Vault,
		ghReleases:    d.GHReleases,
		helmUpdater:   d.HelmUpdater,
		argocdUpdater: d.ArgoCDUpdater,
		notifier:      d.Notifier,
		k8sClients:    d.K8sClients,
		k8sClientsMu:  d.K8sClientsMu,
	}
}

// k8sForEnv returns the StatusClient for the given environment, falling back
// to any available client (backward compatibility). Mirrors Handlers.k8sForEnv.
func (s *Service) k8sForEnv(env string) *kubernetes.StatusClient {
	s.k8sClientsMu.RLock()
	defer s.k8sClientsMu.RUnlock()

	if c, ok := s.k8sClients[env]; ok {
		return c
	}
	for _, c := range s.k8sClients {
		return c
	}
	return nil
}

// envOrDefault normalizes an empty environment to "preprod", the historical
// default used across the handlers.
func envOrDefault(env string) string {
	if env == "" {
		return "preprod"
	}
	return env
}
