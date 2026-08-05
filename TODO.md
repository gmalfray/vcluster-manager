# TODO

Backlog des évolutions à venir. Les items terminés sont archivés dans
[`CHANGELOG.md`](CHANGELOG.md).

## Correctifs recette 1.4.0 → 1.4.1

> Issus de la recette fonctionnelle réelle. Détail + preuves : [`docs/recette-1.4-findings.md`](docs/recette-1.4-findings.md).

- [ ] 🔴 **Restore Velero — RBAC SA** : `deploy/base/rbac.yaml` — `patch` sur `helmreleases`/`statefulsets`/`deployments` dans les ns tenant (suspend/scale/resume échouent en `forbidden`).
- [ ] 🟠 **Restore Velero — topologie** : `internal/kubernetes/velero.go` — cibler l'etcd StatefulSet `vcluster-<name>-etcd` + PVC `data-vcluster-<name>-etcd-0` (control-plane = Deployment, pas StatefulSet).
- [ ] 🟠 **Restore Velero — remontée d'échec** : `handlers/api_velero.go` + `velero_restore_status.html` — ne pas afficher « terminé / Flux repris » quand les étapes ont échoué.
- [ ] 🟠 **UI — bouton « Contenu » backup** : `velero_backups.html` l.68 — envelopper dans `{{if $.User.isAdmin}}` (comme « Restaurer »).
- [ ] 🟠 **Session expirée** : renvoyer `HX-Redirect: /auth/login` sur 401/403 des endpoints HTMX (au lieu d'injecter le login dans un fragment).
- [ ] 🟠 **Flash de contenu vide** : skeleton (`hx-indicator`) ou opacité + swap différé sur la navigation HTMX.
- [ ] 🟡 **Vault webhook** : refonte du template `examples/gitops-repo/lib/tenant-template/vault-webhook` (OCIRepository `v1beta2`, `chartRef`, création du ns `vault-system`) — incompatible avec Flux < 2.6.
- [ ] **UX — modale de confirmation générique** : remplacer les `window.confirm()` (backup) ; unifier Supprimer / Backup / Restaurer / désactivation protection.
- [ ] **UX — toggle protection** : off = gris neutre, on = couleur positive (plus de rouge sur « Inactive »).
- [ ] **UX — colonne STATUS dashboard** : dé-empiler FLUX/VAULT/QUOTAS ; badge de synthèse agrégé + détail au survol pour tenir à l'échelle (8-10 vclusters).
- [ ] **UX — tokens de statut** en variables CSS (`--status-ready/pending/inactive/error`) ; états vides dédiés ; contrastes AA des libellés gris.
- [ ] **Branding — charte Rebuild IT** : appliquer la charte éditeur (indigo `#4F46E5` / orange `#FF7A45` / mint `#22C3A6`, Space Grotesk + Inter, logo 4 blocs — `rebuild-it/branding/brand.md`) à l'app ; à mener avec les tokens de statut et le thème Keycloak.
- [ ] **UX — thème Keycloak** custom (logo/couleurs/fond sombre, charte Rebuild IT) pour la page SSO.
- [ ] **A11y** : focus visible en thème sombre, `aria-label` sur icônes seules, `aria-live`/`role="alert"` sur les toasts.

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
- [x] ~~**Découpe `internal/kubernetes/status.go`** (1413 LOC) : `status.go`,
      `vcluster_access.go`, `velero.go`, `rancher.go`, `protection.go`.~~
- [x] ~~**`atoi` avec `fmt.Sscanf`** dans `gitlab.go` → `strconv.Atoi` (plus
      rapide, erreur explicite).~~ Étendu à `gitops/generator.go`,
      `github/releases.go` et `config/config.go`.
- [x] ~~**`/metrics` derrière le rate limiter ?** Aujourd'hui sur le mux global
      sans middleware. À décider : assumé (Prom scrape interne) vs DoS-vector.~~
      Rate-limiter dédié (5 req/s, burst 10) sans auth.
- [x] ~~**`doc.go` par package** : permettrait de réactiver `ST1000/ST1020` au
      lint. Une passe mécanique.~~ ST1000 activé ; ST1020 (exported symbols)
      reste désactivé — une passe godoc complète est un effort séparé.
- [x] ~~**Tests manquants** : `internal/gitops/gitlab.go` (`withRetry` testable
      avec `httptest`), `internal/notify/webhook.go`, `internal/auth/oidc.go`,
      `internal/rancher/client.go`.~~
- [x] ~~**Errcheck cleanup** : ~21 erreurs réelles non check via `go fmt` dans
      handlers et clients. Soit fix soit `//nolint:errcheck` motivé.~~

## Portabilité Git provider

- [ ] **Support GitHub** (ou tout autre provider Git) : actuellement couplé à
      l'API GitLab via `github.com/xanzy/go-gitlab`. Nécessiterait une
      interface `GitProvider` abstraite + réimplémentation complète de
      `internal/gitops/gitlab.go` (commits multi-fichiers atomiques, MR→PR,
      deploy keys, création de repos dans une org). Voir analyse dans
      `FORK.md`. Grosse feature, à planifier séparément.
- [x] ~~**Migration `xanzy/go-gitlab` → `gitlab.com/gitlab-org/api/client-go`** :
      le module `xanzy` est archivé depuis 2024. La migration permet aussi de
      retirer l'exclusion `SA1019` au lint.~~

## UX / Internationalisation

- [ ] **Support multilingue (i18n)** : l'interface est actuellement en
      français mixte avec quelques termes anglais ; prévoir FR/EN minimum via
      un mécanisme de traduction (fichiers de messages, Accept-Language, ou
      cookie de préférence).

## À retirer quand résolu

- [ ] Workaround Pod exclusion ArgoCD
      (`fluxprod/lib/tenant-template/argocd/base/configmap-argocd-cm.yaml`) à
      retirer quand le bug est corrigé upstream (ArgoCD 3.3.3+).
