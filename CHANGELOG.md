# Changelog

Toutes les modifications notables sont documentées ici. Le format suit
[Keep a Changelog](https://keepachangelog.com/fr/1.1.0/), et la versioning suit
[Semantic Versioning](https://semver.org/lang/fr/).

## [Unreleased]

### Added
- **Opérateur — Events Kubernetes** : `Normal/Deleted` et `Warning/DeletedWithLeftovers`, émis
  au seul endroit où l'information **disparaît** — la conclusion de la suppression s'écrit dans
  le status d'un objet dont le finalizer part deux appels plus loin. Pendant la recette réelle,
  la seule façon d'apprendre que Keycloak et Vault n'avaient pas été nettoyés a été de fouiller
  les logs du pod. Rien n'est émis sur les états stables (budget refusé, protection illisible,
  étapes intermédiaires) : sur une boucle à 30 s, le bruit noierait le signal.
- **`status.podCount` est rempli**, sans jamais confondre « lecture ratée » et « aucun pod » —
  le piège est structurel, un `int` vaut 0 par défaut. Sur une lecture qui n'aboutit pas, la
  dernière valeur connue reste.
- **`ArgoCDReady` lit la Kustomization `argocd-<nom>`** en plus du client OIDC Keycloak, et
  raffine la condition avec ce que le cluster montre. Le volet dépôt GitLab reste hors de
  portée et le message le dit : `AppManifestsRepoExists` confond « dépôt absent » et « API en
  échec », le câbler ferait lire « ArgoCD est cassé » sur un hoquet.
- **Une `ValidatingAdmissionPolicy`** borne le `delete` cluster-wide de l'opérateur aux
  namespaces `vcluster-*` porteurs du label que `hostNamespace()` pose. Validation sur
  `oldObject` — sinon l'attaque se rejoue en deux temps via le SSA du provisionnement — et
  binding en `Warn`/`Audit` d'abord, la flotte vivante n'ayant pas encore le label. Une seconde
  VAP contraint `metadata.name` à la création d'un `VCluster`.
- **Règle CEL sur le nom du `VCluster`** (`^[a-z][a-z0-9-]{0,53}$`) : appliquée par l'API
  server, avec un message lisible, au lieu d'un `Accepted=False` posé après coup. Le 54 est la
  limite physique — `vcluster-` + le nom dans les 63 caractères d'un nom de namespace.
  `service.ValidName` gagne le même plafond, qui lui manquait.

### Fixed
- **Opérateur — le ClusterRole autorise la lecture des quotas de la cell.** Sans `list` sur les
  `resourcequotas`, l'étape budget échoue et **aucun `VCluster` n'est réconcilié** : ni
  provisionnement, ni status, ni finalizer. Trouvé à la première minute de la recette réelle, et
  invisible en test — **envtest n'applique pas le RBAC**, donc les 41 fichiers de test étaient
  verts sur un opérateur inopérant en production.
- **Opérateur — la borne de dix minutes fuyait d'un échec à l'autre.** `NamespaceRemoved=False`
  était écrite par l'attente *et* par le refus, et `SetStatusCondition` ne remet
  `LastTransitionTime` à zéro que si le *statut* change, pas la raison. Après un `forbidden`
  prolongé — le ClusterRole non redéployé — la borne était déjà dépassée : au premier tour
  réussi, le CR partait **sans observer**, en accusant un finalizer tiers. Reproduit sur cluster
  réel (CR lâché 17 s après la remise du droit, sur un namespace jamais disparu), puis fermé et
  revérifié sur le même scénario.
- **L'audit ne journalise plus une suppression par tour** sur un namespace déjà en Terminating :
  une vingtaine de lignes identiques noyaient la seule qui comptait.
- **« Inconnu n'est pas faux » tenu jusqu'à l'écran** : le fragment HTMX de protection rendait du
  vide sur une lecture ratée — le « Chargement... » était remplacé par rien, définitivement. Il
  affiche désormais « état non lisible » avec sa cause.

- **Opérateur — la suppression supprime enfin le namespace** (arbitrage N6). L'étape
  `Destroying` du finalizer retirait les finalizers Flux « pour que le namespace puisse être
  supprimé », puis annonçait « séquence de suppression terminée » : la suppression réelle était
  le prune Flux d'un commit que le finalizer n'écrit ni ne vérifie. Un CR pouvait donc disparaître
  en laissant derrière lui le namespace, ses pods et son volume. L'opérateur le supprime désormais
  lui-même — il en est propriétaire, il l'applique en Server-Side Apply — puis **constate** sa
  disparition avant de lâcher le CR. L'attente est bornée à 10 minutes ; au-delà, le CR est lâché
  et ce qui reste debout est nommé dans le message final. Nouvelle condition `NamespaceRemoved` ;
  `delete` sur `namespaces` ajouté au ClusterRole de l'opérateur (`deploy/base/operator-rbac.yaml`
  — **à redéployer**, sans quoi l'étape échoue en `forbidden`).
- **Opérateur — la protection du namespace est relue avant de détruire**, et pas seulement
  avant d'être levée. La séquence ne levait pas `protect-deletion` sur une lecture ratée —
  bon réflexe — puis détruisait quand même : le garde-fou était contourné par la panne qu'il
  aurait dû faire échouer. Inoffensif tant que la destruction passait par le prune Flux
  (qu'une annotation restée posée retient), mais plus depuis N6. La séquence s'arrête
  désormais sur `Ready=False/ProtectionUnknown` avant toute destruction, et
  `status.protectionEnabled` n'affirme plus une levée non constatée. `ProtectionState` gagne
  un champ `Detail` qui dit *pourquoi* la réponse est indisponible.
- **`GetNamespaceProtection` distingue « pas d'annotation » de « namespace illisible »** : elle
  rendait `false` dans les deux cas. Elle rend `(bool, error)`, et les trois sites d'appel
  traitent l'inconnu comme tel — en particulier la levée de l'annotation `protect-deletion` avant
  suppression, qui ne se fait plus que sur une lecture confirmée : un hoquet d'API ne débloque
  plus un namespace protégé.
- Le commentaire de `ProvisionFieldManager` affirmait que Flux n'écrit aucun des objets appliqués
  par l'opérateur. C'était faux pour le namespace, qui vient de `clusters/<cell>/base`. La
  propriété est tranchée explicitement : création et suppression à l'opérateur, réapplication à
  Flux tant que l'overlay du tenant est commité.

## [1.4.0] — 2026-08-05

### Added
- **Couche service `internal/service`** : la logique métier est extraite des handlers, un fichier
  par domaine (protection, rancher, dashboard, velero, vcluster, apps, settings, platform, status),
  testable en isolation. Les adaptateurs web (`internal/handlers`) ne font plus que parser, déléguer
  et rendre — préparation de l'API REST et de l'opérateur. Fondation `service.Deps`/`Service`,
  `models.Actor`, `audit.LogActor`, `service.ValidName`.
- Documentation de conception opérateur : `docs/adr-001-source-de-verite.md` (source de vérité = CR
  `VCluster` versionné), `docs/crd-vcluster.md` (schéma + découpage kro/controller-runtime),
  `docs/etude-cluster-api.md` (extension future CAPI), `docs/refactor-api.md`,
  `docs/recette-securite.md`.

### Fixed
- **Toggle ArgoCD** : activer/désactiver ArgoCD sur un vcluster avec FluxCD bootstrapé effaçait
  silencieusement la config Flux (`flux bootstrap --url=` vide). Les champs FluxCD sont désormais
  reportés lors de la régénération. Test de non-régression ajouté.
- **Restore Velero in-place** : garde-fous data-safety — pré-check de la phase du backup
  (`Completed`) avant toute suppression de PVC, attente réelle de la disparition du pod, reprise de
  FluxCD côté serveur (goroutine) indépendante du navigateur.

### Security
- **Contenu de backup Velero réservé aux admins** : il était lisible par tout utilisateur
  authentifié (un lecteur pouvait récupérer les `Secret` du tenant). Gate admin ajouté.
- **Validation anti-injection** des champs libres avant commit GitOps — dont une injection de
  commande shell via `FluxCDRepoURL`/`Branch`/`Path` dans `flux bootstrap`. Champs quantité/version/
  bucket/URL S3 validés ; YAML Velero produit par marshaling de struct.
- **Garde `IsDeleting`** sur `StatusFragment` : un `?deleting=true` ne peut plus déclencher le
  retrait de finalizers sur un nom arbitraire (état serveur-autoritaire requis).
- Validation cohérente du `name` (`UpdateSettings`, `CreateVeleroRestore`, `MigrateApp`).
- Dépendances : bumps `x/text`, `x/net`, `x/crypto`, `moby/spdystream`, `go-jose/v4` — `govulncheck`
  à 0 vulnérabilité atteignable.

### Changed
- Groupes admin OIDC par défaut neutralisés (`platform-admins`, `ops`), toujours configurables via
  `ADMIN_GROUPS`.

## [1.3.0] — 2026-04-29

### Added
- `Makefile` : cibles `build`, `test`, `test-short`, `vet`, `fmt`, `lint`,
  `lint-fix`, `coverage`, `tidy`, `check`, `clean`. Exporte
  `GOTMPDIR=$HOME/.cache/go-tmp` pour les workstations avec `/tmp` monté `noexec`.
- `.golangci.yml` (v2) : baseline de linters (errcheck, govet, staticcheck,
  ineffassign, unused, bodyclose, misspell, unconvert, gocritic, revive,
  copyloopvar). Toutes les exclusions temporaires levées en fin de release
  (`SA1019` après migration gitlab, `ST1000`/`ST1020` après passe doc.go).
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
