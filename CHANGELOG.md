# Changelog

Toutes les modifications notables sont documentées ici. Le format suit
[Keep a Changelog](https://keepachangelog.com/fr/1.1.0/), et la versioning suit
[Semantic Versioning](https://semver.org/lang/fr/).

## [Unreleased]

### Added
- `Makefile` : cibles `build`, `test`, `test-short`, `vet`, `fmt`, `lint`,
  `lint-fix`, `coverage`, `tidy`, `check`, `clean`. Exporte
  `GOTMPDIR=$HOME/.cache/go-tmp` pour les workstations avec `/tmp` monté `noexec`.
- `.golangci.yml` (v2) : baseline de linters (errcheck, govet, staticcheck,
  ineffassign, unused, bodyclose, misspell, unconvert, gocritic, revive,
  copyloopvar). Exclusions ciblées : `SA1019` (deprecation `xanzy/go-gitlab`,
  migration séparée) et `ST1000/ST1020` (passe `doc.go` à venir).
- Documentation séparée : `AGENTS.md` (règles agent), `docs/ARCHITECTURE.md`
  (patterns détaillés), `CHANGELOG.md`, `TODO.md`. `CLAUDE.md` devient un shim
  pointant vers ces fichiers.

### Changed
- **Graceful shutdown** dans `cmd/server/main.go` : `signal.NotifyContext`
  (SIGINT/SIGTERM) + `srv.Shutdown(30s)` pour drainer les requêtes en vol
  avant exit. Les SSH tunnels sont fermés au shutdown (étaient `_ = tunnel`).
- **Timeouts HTTP** sur le serveur : `ReadHeaderTimeout=10s` (mitigation
  CWE-400 Slowloris), `ReadTimeout=30s`, `WriteTimeout=60s`,
  `IdleTimeout=120s`.

### Fixed
- `gitlab.CreateAppManifestsRepo` : les erreurs des appels post-création
  (avatar, README, branche preprod, protection des branches, deploy key)
  étaient silencieusement ignorées, laissant le repo à moitié configuré
  sans signal au caller. Elles sont maintenant loggées et agrégées via
  `errors.Join`, retournées à côté du `projectID` (best-effort : le repo
  reste récupérable manuellement).

### Style
- Sweep `gofmt` sur l'ensemble du tree (alignement de structs, regroupement
  d'imports). Aucun changement sémantique.

## [1.2.0] — Initial public release

Première release publique. Voir le commit `065b9ec` pour la liste complète des
fonctionnalités embarquées.

## [1.1.0]

### Added
- Numéro de version : fichier `VERSION` + `internal/version/version.go`
  (go:embed), affiché dans la nav.
- Rate limiting : `auth.NewRateLimiter` (20 req/s, burst 50) sur toutes les
  routes.
- Protection CSRF : double-submit cookie `csrf_token` + header `X-CSRF-Token`.
- Audit log : `audit.Log(r, action, name, env)` sur toutes les opérations
  d'écriture.
- Métriques Prometheus : middleware `metrics.Middleware` + handler `GET /metrics`.
- Notification webhook : `internal/notify/webhook.go` + variable `WEBHOOK_URL`.
- Tests unitaires generator : 25 tests dans `internal/gitops/generator_test.go`.
- Tests unitaires parser : 17 tests dans `internal/gitops/parser_test.go` (via
  interface `fileProvider`).
- Tests unitaires handlers : 17 tests dans `internal/handlers/handlers_test.go`.
- Tests CSRF : 12 tests dans `internal/auth/csrf_test.go`.
- Détection appairage Rancher manuel : `k8s.HasRancherAgents()` + états UI
  Unknown / ManuallyPaired.
- Fork portability : valeurs hardcodées remplacées par env vars avec defaults
  backward-compat (`ADMIN_GROUPS`, `DEFAULT_RBAC_GROUP`,
  `FLUXPROD_CLUSTERS_PATH`, `FLUXPROD_ARGOCD_KUST_PATH`,
  `HELM_CHARTS_VCLUSTER_PATH`, `VAULT_KV_ARGOCD_ROOTAPPS`,
  `VAULT_KV_ARGOCD_REPO`).
- Backend de persistence configurable : `STATE_BACKEND=file` (défaut) ou
  `STATE_BACKEND=configmap` (ConfigMap K8s `vcluster-manager-state`, survit au
  rescheduling sans PVC). Interface `stateBackend` dans
  `internal/config/backend.go`, implémentations `fileBackend` et
  `configmapBackend`. RBAC Role namespaced dans `deploy/base/rbac.yaml`.
- Retries GitLab API : `withRetry()` dans `internal/gitops/gitlab.go` (3
  tentatives, backoff 2s/5s/10s, uniquement sur 5xx/429/erreurs réseau).
  Métriques `gitlab_api_errors_total` et `gitlab_api_retries_total`.
