# AGENTS.md — vCluster Manager

Règles à destination des assistants IA (Claude Code, Copilot, Cursor, etc.).
Pour le contexte technique détaillé, voir [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md).
Pour le déploiement et la portabilité, voir `FORK.md`.

## Contexte en une phrase

Application web Go qui gère les vClusters Kubernetes via GitOps : elle commit dans
le repo fluxprod (GitLab API), FluxCD reconcilie, et l'app ne touche **jamais** au
cluster directement.

## Build & test

| Cible | Action |
|---|---|
| `make build` | Compile dans `./bin/vcluster-manager` |
| `make test` | `go test -race -count=1 ./...` |
| `make test-short` | Idem sans `-race` |
| `make vet` | `go vet ./...` |
| `make lint` | `golangci-lint run` |
| `make fmt` | `gofmt` + `goimports` |
| `make check` | `vet` + `test` + `lint` (à passer avant tout commit) |

**Préreqs** : Go ≥ 1.25, `golangci-lint` v2. Si `/tmp` est monté `noexec`, le
Makefile exporte `GOTMPDIR=$HOME/.cache/go-tmp` automatiquement.

## Règles non négociables

### 1. Accès à l'API d'un vcluster

Toute connexion à l'API K8s **à l'intérieur** d'un vcluster (pas au cluster host)
doit passer par l'un des trois helpers de `internal/kubernetes/status.go` :

| Helper | Fournit | Quand utiliser |
|---|---|---|
| `withVClusterPortForward(ctx, name, fn)` | `*rest.Config` | manifests bruts |
| `withVClusterDynClient(ctx, name, fn)` | `dynamic.Interface` | CRDs / ressources non typées |
| `withVClusterClientset(ctx, name, fn)` | `*kubernetes.Clientset` | Jobs, ServiceAccounts, Discovery |

**Interdit** : appeler `getInternalKubeconfig` / `GetKubeconfig` +
`clientcmd.RESTConfigFromKubeConfig` directement. `ApplyManifestToVClusterViaPortForward`
est un alias déprécié — ne pas l'utiliser dans du code neuf.

### 2. GitOps exclusif

L'application **ne modifie jamais** un cluster K8s directement. Toute mutation
d'état tenant = commit Git via `gitops.GitLabClient.Commit()`. Seules exceptions
autorisées (cluster host, hors tenant) : protection namespace (Server-Side Apply),
appairage Rancher (apply manifest).

### 3. Synchronisation generator ↔ fluxprod

- **Tout nouveau fichier** dans `clusters/{env}/vclusters/{name}/tenant/` doit
  être ajouté dans `internal/gitops/generator.go` :
  1. `GenerateVCluster()` → liste `files`
  2. `tenantKustomizationYAML()` → ressources (argocd=true ET argocd=false si applicable)
  3. Fonction génératrice dédiée
- **Réciproque** : avant toute modif de `generator.go`, vérifier la cohérence
  avec les fichiers existants dans fluxprod, sous peine d'écraser des configs
  manuelles à la prochaine régénération.

### 4. Pre-commit

Lancer `make check` avant tout commit. Si un linter remonte une issue dans du
code modifié, la traiter (ne pas l'ajouter à la liste d'exclusions sans
discussion).

## Conventions

- **Templates HTML** : chaque page est parsée individuellement avec
  `layout` + page + partials (pas de `ParseGlob` global). Le handler appelle
  `h.render(w, "page.html", data)` qui exécute `layout` → `content`. Les
  partials HTMX passent par `h.renderPartial(w, "partial.html", data)`.
  `{{define "content"}}` dans chaque page, **pas** de `{{template "layout" .}}` en bas.
- **Langue** : UI, commits et templates en français (orthographe accentuée
  depuis 2026 ; l'historique utilise du français non-accentué — ne pas
  retoucher massivement).
- **Commits** : convention conventionnelle (`feat:`, `fix:`, `chore:`,
  `docs:`, `style:`, `refactor:`). Co-author Claude quand pertinent.

## Logging

- `slog` est le logger par défaut (`cmd/server/main.go`, handler JSON sur
  `os.Stderr`). Niveau configurable via `LOG_LEVEL=debug|info|warn|error`.
- Le package `log` standard est bridgé via `slog.SetLogLoggerLevel` : tout
  `log.Printf` existant sort en JSON, mais sans fields structurés. Pour un
  nouveau call site, préférer `slog.Info/Warn/Error("message", "key", val)`.
- Convention erreurs : `slog.Error("operation X failed", "err", err)` (clé
  `err` standardisée).
- Pas encore de propagation `context.Context` (`slog.InfoContext`) : à
  introduire en même temps que le tracing OpenTelemetry (item TODO).

## Points de vigilance

- **Fichiers monstres** : `internal/service/velero.go` (38 Ko) et
  `internal/service/vcluster.go` (29 Ko) sont les deux plus gros du dépôt. Éviter
  d'y ajouter sans nécessité ; un découpage est dans le backlog. (Les chiffres
  précédents — `status.go` ~1400 LOC, `api.go` ~1300 LOC — étaient périmés :
  `status.go` fait ~480 lignes aujourd'hui.)
- **Cache GitLab** : TTL 30s. `gl.Commit()` invalide tout. Si tu modifies l'état
  GitLab par un autre chemin, appeler `gl.InvalidateCache()` ou les lecteurs
  resteront sur du contenu périmé jusqu'à 30s.
- **CSRF** : header `X-CSRF-Token` injecté automatiquement sur les requêtes HTMX
  par le hook dans `layout.html`. Toute nouvelle page **non-HTMX** doit ajouter
  un `<input type="hidden" name="_csrf" value="{{.CSRFToken}}">` manuellement.
- **RBAC** : l'autorité est `if !actor.IsAdmin { return ErrForbidden }`, en
  **première instruction de la méthode de service** — 21 sites, jamais dans
  l'adaptateur. `requireAdmin(w, r)` existe encore côté handlers mais ne couvre
  plus que 4 routes ; il ne fait pas foi. Les templates masquent les boutons
  d'écriture avec `{{if .User.IsAdmin}}` : côté confort, pas côté sécurité.
- **Audit** : `audit.LogActor(actor.Username, action, name, env, extra...)`
  **après** que la mutation a réussi, jamais avant. `audit.Log(r, …)` est la forme
  transport-couplée, dépréciée — il en reste 3 appels dans
  `internal/handlers/api_chart.go`, à migrer.
- **Fail-closed délibéré, à ne pas « simplifier »** : sans plafond de ressources
  configuré, la création avec quotas est **refusée** (`internal/config/config.go`) ;
  un groupe RBAC ArgoCD non conforme est **refusé** et non assaini
  (`internal/service/vcluster_validation.go`) — un groupe discrètement écarté est
  un droit d'accès silencieusement cassé.
- **⚠️ `internal/gitops/branches.go`** : `preprod` y est un nom de **branche Git**,
  pas d'environnement. « Paramétrer proprement » ces constantes par environnement
  enverrait les changements prod sur la branche que Flux prod surveille.
- **Pas de Go local sur cette machine.** Aucune cible du Makefile n'est
  exécutable en l'état : tout passe par Docker. Voir le hook
  `.claude/hooks/go-fmt-vet.sh` pour l'invocation exacte et les caches partagés.

## L'opérateur

La brique opérateur (`internal/controller`, `api/v1alpha1`, `cmd/operator`) suit
trois règles non négociables, apprises à la dure :

- **Reprise par observation, jamais par registre écrit.** Ne jamais relire un
  champ de status qu'on a écrit soi-même pour savoir où on en est : redemander au
  monde. Le finalizer a explicitement refusé le `status.deletion.stage` que le
  design demandait, parce que sur quatre étapes persistées une seule discriminait
  — et elle se trompait dans le cas qu'elle devait couvrir.
- **Inconnu n'est pas faux.** `True` = lu et bon, `False` = lu et pas prêt,
  `Unknown` = pas lu. Un tiers injoignable ne vaut jamais « cassé », et une
  absence de client ne vaut jamais « rien à faire ».
- **Vérification par mutation.** Un test qui ne tue aucun mutant ne compte pas.
  Piège vérifié : une mutation qui rend un import ou une variable inutilisés **ne
  compile pas**, et `go test` échoue alors pour la mauvaise raison — vérifier que
  chaque mutant passe `go vet` avant de le compter tué.

État détaillé et ce qui reste : [`docs/etat-brique-operateur.md`](docs/etat-brique-operateur.md).

## Pour aller plus loin

- [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) — patterns détaillés (versions,
  cache, Velero, Rancher, protection namespace, etc.)
- [`CHANGELOG.md`](CHANGELOG.md) — historique des releases
- [`TODO.md`](TODO.md) — backlog et items à retirer quand résolus
- `FORK.md` — variables d'environnement et guide de portabilité (gitignored,
  contient des détails opérationnels)
