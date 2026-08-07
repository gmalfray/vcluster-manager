# Équipe d'agents & workflow de dev — vcluster-manager

> Local uniquement (`.claude/` n'est pas versionné → rien n'est poussé sur le repo public).
> Se charge automatiquement quand une session Claude Code est ouverte dans ce repo.

## L'équipe

| Agent | Domaine | Zone |
|------|---------|------|
| **backend-go** | Couche service/API/handlers, DTOs, clients d'intégration, tests Go | `internal/{service,api,handlers,models,keycloak,rancher,vault,gitops,…}` |
| **operator-k8s** | CRD, kro RGD / controller-runtime, réconciliation, finalizers | futur `internal/controller`, `api/`, `config/crd` |
| **gitops-flux** | FluxCD, kustomize, `deploy/`, templates fluxprod, CI | `deploy/`, `internal/gitops/templates`, `Jenkinsfile` |
| **design-ux** | Design **en amont** via Claude Design (`/design`, `/design-sync`), maquettes, charte → hand-off | maquettes / specs (pas de code) |
| **frontend-htmx** | Templates HTMX/Tailwind, fragments, migration UI→REST, implémentation des maquettes | `web/templates`, `web/static` |
| **simplificateur** | Challenge les choix techniques **avant** d'implémenter : sur-ingénierie, abstractions prématurées, usines à gaz | docs de design / architecture |
| **code-reviewer** | Revue qualité/correction/simplification (avant merge) | diff / PR |
| **security-auditor** | Failles, secrets, RBAC K8s, dépendances (avant release) | diff / repo |
| **qa-recette** | Stratégie de test, tests Go manquants, plans de recette preprod, verdict Go/No-Go | `*_test.go`, plans de recette |

**Délégation** : déléguer spontanément à l'agent du domaine, parallélisable par domaine. Un agent fait le travail lui-même (pas de cascade vers un agent du même type).

## Workflow de dev

1. **Partir à jour** : `git fetch` + se baser sur la bonne branche (preprod par défaut).
2. **Une branche par lot** : `feat/…`, `fix/…`, `refactor/…`. Jamais de dev direct sur `master`/`preprod`.
3. **Déléguer** au bon agent selon le domaine (table ci-dessus). Plusieurs domaines = flux parallèles.
4. **Build + tests systématiques** (pas de `go` local — image officielle) :
   ```bash
   docker run --rm -v "$PWD":/app -w /app -e GOCACHE=/tmp/gocache -e GOFLAGS=-mod=mod \
     golang:1.25-alpine sh -c 'go build ./... && go vet ./... && go test ./...'
   ```
5. **Porte de revue avant merge** : `code-reviewer` (qualité) puis, si authz/endpoint/reconcile/release touché, `security-auditor`. Corriger les findings avant de merger.
   → En amont, sur un chantier structurant (opérateur, CRD, refactor), passer par **`simplificateur`** *avant* d'écrire le code : il est là pour qu'on en écrive moins, pas pour relire ce qui existe.
6. **Recette** : `qa-recette` après la revue — tests automatisés manquants, plan de recette, verdict Go/No-Go. Les opérations destructives (suppression, restore Velero in-place, dépairage Rancher) se recettent sur un vcluster jetable en preprod, jamais en prod.
7. **Commit** : messages en **français**, **sans** `Co-Authored-By`.
8. **Déploiement** : via GitOps/FluxCD (`gitops-flux`). Prod = client réel → sauvegarde + vérification, jamais de modif prod à chaud non validée.

## Règles projet transverses (rappel)

- **GitOps absolu** : l'app ne modifie jamais le cluster directement ; tout passe par commit Git → FluxCD (sauf lectures status client-go et opérations intra-vcluster via `withVCluster*`).
- **API interne d'un vcluster** : uniquement via `withVClusterPortForward/DynClient/Clientset` (`internal/kubernetes/status.go`).
- **Générateur ↔ fluxprod** : garder `generator.go` synchronisé avec les templates/fichiers fluxprod.
- **Secrets** : jamais en clair, jamais loggés, jamais en URL. `secret*` gitignorés ; Vault / secrets Flux sinon.

## Roadmap (le fil directeur)

- **Étape 1 — extraction couche service + API REST** *(faite pour l'essentiel)*. Voir `docs/refactor-api.md`. 9 domaines extraits, cœur `vcluster` et `velero` compris. Ne restent dans les handlers que `cluster_config`/`ClusterHealth` et trois handlers d'`api.go` (kubeconfig, repo app-manifests, MR prod).
- **Étape 2 — POC kro** sur un vcluster preprod (valider le graphe + la maturité).
- **Étape 3 — opérateur** : source de vérité **tranchée = C allégé** (CR `VCluster` versionné dans le repo GitOps, appliqué par Flux, expansé côté cluster ; MR permanente supprimée). Voir `docs/adr-001-source-de-verite.md`. Reste : CRD `VCluster` (avec `type: vcluster | capi` dès le départ), reconcile, finalizers — le finalizer conditionne le retrait de la MR sur le chemin de suppression.

## Écriture
Commentaires, messages (toasts/logs/erreurs) et messages de commit en ton naturel : invoque le skill `humanizer` (ou applique ses règles). Vaut pour toute l'équipe.
