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

### Refactoring
- **`handlers.New` à 12 args positionnels → struct `handlers.Deps`** :
  le call site dans `cmd/server/main.go` passe maintenant un struct
  champ-par-champ, donc auto-documenté et résilient à l'ajout/retrait
  de dépendances (réordonner ou ajouter un argument ne nécessite plus
  de toucher l'appelant). Pas de changement de comportement.

### Lint
- **`make check` propre** : 0 warning. Les 15 issues résiduelles
  (copyloopvar, gocritic, staticcheck SA9003, unused, misspell) sont
  traitées :
  - `cmd/server/main.go` refactorisé en `main()` + `run() error` : les
    quatre `os.Exit(1)` qui shuntaient `defer stop()` (et plus loin
    `tunnels.Close()`, `gl.Close()`, `helmGL.Close()`) deviennent des
    `return fmt.Errorf(...)`, donc tous les defer s'exécutent avant
    sortie.
  - Suppression des copies de variable de boucle (`env := env`,
    `entry := entry`) inutiles depuis Go 1.22.
  - `else { if … }` → `else if`, `host = host + ":22"` → `host += ":22"`.
  - `if err := …; err != nil { /* comment-only */ }` → `_ = …` avec
    commentaire explicatif (best-effort documenté au call site).
  - Suppression du type mort `valuesFile` dans `gitops/parser.go` (le
    parser utilise `map[string]interface{}`) et des méthodes Rancher
    inutilisées (`getManifestURL`, `getManifestURLFromEndpoint` +
    `registrationTokenListResponse`).
  - `.golangci.yml` : whitelist de mots français (`manifestes`,
    `exemple`, `correspondant`) pour `misspell` ; nettoyage des
    `disabled-checks` gocritic redondants.

### Errcheck cleanup
- 21 retours d'erreur ignorés signalés par `errcheck` traités à la
  source : `tmpl.Execute`, `json.Unmarshal/Decode`, `w.Write`,
  `buf.WriteTo`, `gz.Close`, et les `Close()` des connexions tunnel SSH.
  Les erreurs récupérables remontent (auth OIDC retourne `authenticated:
  false` si le payload JWT est mal formé ; le polling Rancher continue
  son retry au lieu d'utiliser un cluster vide). Les chemins de cleanup
  où l'erreur n'est pas actionnable utilisent `_ = closer.Close()`
  pour expliciter l'intention.
- **`atoi` maison → `strconv.Atoi`** dans `internal/gitops/gitlab.go`,
  `internal/gitops/generator.go`, `internal/github/releases.go` et
  `internal/config/config.go`. La conversion du `argocdGroupID` propage
  désormais une erreur si la valeur n'est pas numérique au lieu d'utiliser
  silencieusement `0`.

### Caching
- **Cache GitLab maison → `samber/hot`** : `internal/gitops/gitlab.go`
  remplace son `map+sync.RWMutex+TTL 30s` non borné par
  `hot.HotCache[string, string]` (W-TinyLFU, capacité 1024 entrées,
  TTL 30 s, janitor de purge en arrière-plan). Avantages :
  - **Mémoire bornée** : l'ancien cache ne purgeait jamais les entrées
    expirées (uniquement vérifiées au lookup), donc un serveur de longue
    durée découvrant de nouveaux vclusters/fichiers grandissait sans
    limite. Capacité dure désormais ~5 MB.
  - **Métriques Prometheus** : `hot_hit_total`, `hot_miss_total`,
    `hot_eviction_total`, `hot_size_bytes`, `hot_length`, etc., labellés
    par `name=<projectID>` pour distinguer les caches fluxprod et helm
    charts.
  - **Algo W-TinyLFU** scan-resistant, plus robuste qu'un LRU naïf.
- `GitLabClient.Close()` arrête le janitor du cache ; appelé depuis le
  shutdown de `cmd/server/main.go` pour ne pas laisser tourner les
  goroutines au-delà du process.

### Style
- Sweep `gofmt` sur l'ensemble du tree (alignement de structs, regroupement
  d'imports). Aucun changement sémantique.

### Context propagation
- **`context.Context` propagé sur la chaîne GitOps** :
  `gitops.GitLabClient.{ListFiles,GetFile,Commit}`,
  `gitops.Parser.{ListVClusters,ParseVCluster,Exists,UsedVeleroSlots,
  ListVClusterNamesOnBranch}`, `helmcharts.Updater.{GetCurrentChartVersion,
  GetDefaultK8sVersion,UpdateChart,UpdateK8sVersion}` et
  `argocd.Updater.{GetGlobalVersion,UpdateGlobalVersion}` prennent
  désormais un `ctx` en premier argument. Les handlers HTTP propagent
  `r.Context()` ; les chaînes background (vault reconciler, suppression
  asynchrone après pairing Rancher) utilisent `context.Background()`
  explicitement.
- **`withRetry` annulable** dans `gitops/gitlab.go` : le `time.Sleep`
  bloquant entre tentatives (jusqu'à 17 s cumulés) est remplacé par un
  `select { <-ctx.Done() / <-time.After(delay) }` qui débloque le
  graceful shutdown si le serveur reçoit un SIGTERM pendant un retry.
  Les requêtes GitLab elles-mêmes utilisent `gitlab.WithContext(ctx)`
  pour annuler les appels HTTP en vol.
- **`errgroup` dans `parser.ListVClusters`** : remplace
  `sync.WaitGroup`. L'annulation du contexte (onglet fermé) interrompt
  les parses en cours au lieu de continuer en pure perte. Les échecs
  par-vcluster restent non fatals (warning + skip), seul `ctx.Err()`
  remonte.
- **`notify.Notifier.Send(ctx, text)`** utilise
  `http.NewRequestWithContext` au lieu de `client.Post` ; le webhook
  honore désormais le `ctx` du caller en plus du timeout 10 s du
  client. Les deux call sites actuels (`go h.sendNotification(...)`)
  sont déjà détachés et passent `context.Background()` — la mécanique
  est en place pour des futurs callers synchrones.

### Logging
- **Phase 1 de la migration `log` → `slog`** : initialisation d'un handler
  JSON par défaut dans `cmd/server/main.go`, niveau configurable via
  `LOG_LEVEL` (`debug|info|warn|error`, défaut `info`). Les call sites
  existants `log.Printf/Println` flow désormais à travers `slog` via
  `slog.SetLogLoggerLevel` — sortie JSON immédiate sans refactor des 181
  occurrences éparpillées dans 20 fichiers.
- `cmd/server/main.go` (32 calls) et `internal/audit/audit.go` (audit log
  structuré) sont migrés avec des fields enrichis (`"err"`, `"env"`,
  `"vcluster"`, `"action"`, etc.). L'enrichissement progressif des autres
  fichiers est listé dans `TODO.md`.
- **Phase 2 de la migration `log` → `slog`** : conversion des ~140 call
  sites restants dans `internal/auth/`, `internal/config/`,
  `internal/gitops/`, `internal/handlers/`, `internal/kubernetes/`,
  `internal/rancher/`, `internal/vault/`. Chaque appel utilise désormais
  des attributs key/value structurés (`"vcluster"`, `"env"`, `"err"`,
  `"branch"`, `"cluster_id"`, …) plutôt que du formatage `%s/%v`, ce qui
  rend la sortie JSON exploitable par un agrégateur (Loki, ELK). Les
  messages bruyants (port-forward, manifests appliqués, polling Rancher)
  passent au niveau `Debug`. `cmd/server/main.go` conserve l'import
  `"log"` pour le bridge `slog.SetLogLoggerLevel`.

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
