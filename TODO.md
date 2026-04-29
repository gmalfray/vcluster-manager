# TODO

Backlog des évolutions à venir. Les items terminés sont archivés dans
[`CHANGELOG.md`](CHANGELOG.md).

## Améliorations Go (issu de l'audit skills)

- [x] ~~**Migration `log` → `slog` (phase 1)** : init JSON handler dans
      `main.go`, bridge du package `log` standard via
      `slog.SetLogLoggerLevel`, conversion enrichie de `cmd/server/main.go`
      et `internal/audit/audit.go`.~~
- [x] ~~**Migration `log` → `slog` (phase 2)** : enrichir avec des fields
      structurés (`slog.Error("foo", "err", err)`) les ~150 call sites
      restants dans `internal/handlers/*` (98), `internal/kubernetes/*` (10),
      `internal/gitops/*` (8), `internal/rancher/`, `internal/vault/`, etc.~~
      Phase 3 (corrélation `slog.*Context(ctx, ...)`) à planifier séparément.
- [x] ~~**Cache GitLab maison → `samber/hot`** : `internal/gitops/gitlab.go`
      embarque un cache `map+sync.RWMutex+TTL 30s` (~40 LOC). `samber/hot`
      apporte LRU/TinyLFU, métriques Prometheus, et purge des entrées expirées
      (le maison ne purge jamais : croissance mémoire non bornée).~~
- [x] ~~**`errgroup` au lieu de `WaitGroup`** dans `parser.ListVClusters` : pas
      de propagation d'erreur, pas d'annulation si un parse échoue.~~
- [x] ~~**`withRetry` cancellable** : `time.Sleep` bloquant dans
      `gitops/gitlab.go:80` retarde le shutdown jusqu'à 17s. Ajouter `ctx` et
      `select { case <-ctx.Done(): ...; case <-time.After(delay): }`.~~
- [x] ~~**`notify.Send` avec contexte** : `n.client.Post(...)` → utiliser
      `http.NewRequestWithContext(ctx, ...)`. Permet d'annuler un webhook
      bloqué quand l'utilisateur ferme l'onglet.~~
- [x] ~~**Constructeur `handlers.New` à 12 args** : remplacer par struct config
      ou functional options.~~ Struct `handlers.Deps`.
- [x] ~~**Découpe `internal/handlers/api.go`** (1275 LOC) : `api_velero.go`,
      `api_rancher.go`, `api_protection.go`, `api_chart.go`, `api_apps.go`.~~
- [ ] **Découpe `internal/kubernetes/status.go`** (1413 LOC) : `status.go`,
      `vcluster_access.go`, `velero.go`, `rancher.go`, `protection.go`.
- [x] ~~**`atoi` avec `fmt.Sscanf`** dans `gitlab.go` → `strconv.Atoi` (plus
      rapide, erreur explicite).~~ Étendu à `gitops/generator.go`,
      `github/releases.go` et `config/config.go`.
- [ ] **`/metrics` derrière le rate limiter ?** Aujourd'hui sur le mux global
      sans middleware. À décider : assumé (Prom scrape interne) vs DoS-vector.
- [ ] **`doc.go` par package** : permettrait de réactiver `ST1000/ST1020` au
      lint. Une passe mécanique.
- [ ] **Tests manquants** : `internal/gitops/gitlab.go` (`withRetry` testable
      avec `httptest`), `internal/notify/webhook.go`, `internal/auth/oidc.go`,
      `internal/rancher/client.go`.
- [x] ~~**Errcheck cleanup** : ~21 erreurs réelles non check via `go fmt` dans
      handlers et clients. Soit fix soit `//nolint:errcheck` motivé.~~

## Portabilité Git provider

- [ ] **Support GitHub** (ou tout autre provider Git) : actuellement couplé à
      l'API GitLab via `github.com/xanzy/go-gitlab`. Nécessiterait une
      interface `GitProvider` abstraite + réimplémentation complète de
      `internal/gitops/gitlab.go` (commits multi-fichiers atomiques, MR→PR,
      deploy keys, création de repos dans une org). Voir analyse dans
      `FORK.md`. Grosse feature, à planifier séparément.
- [ ] **Migration `xanzy/go-gitlab` → `gitlab.com/gitlab-org/api/client-go`** :
      le module `xanzy` est archivé depuis 2024. La migration permet aussi de
      retirer l'exclusion `SA1019` au lint.

## UX / Internationalisation

- [ ] **Support multilingue (i18n)** : l'interface est actuellement en
      français mixte avec quelques termes anglais ; prévoir FR/EN minimum via
      un mécanisme de traduction (fichiers de messages, Accept-Language, ou
      cookie de préférence).

## À retirer quand résolu

- [ ] Workaround Pod exclusion ArgoCD
      (`fluxprod/lib/tenant-template/argocd/base/configmap-argocd-cm.yaml`) à
      retirer quand le bug est corrigé upstream (ArgoCD 3.3.3+).
