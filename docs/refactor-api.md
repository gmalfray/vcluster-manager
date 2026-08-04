# Refactor — extraction de l'API (couche service)

> **Statut (2026-08-04) : plan de l'increment 2, pas encore réalisé sur `main`.**
> `main` (v1.3.0) a déjà découpé les gros fichiers par domaine (`handlers/api_*.go`,
> `kubernetes/*.go`, `handlers.Deps`) mais garde la logique métier **inline** dans les
> handlers — il n'y a pas encore de couche `internal/service` ni de package `internal/api`.
> Ce document décrit la cible : sortir la logique dans `internal/service` (un fichier par
> domaine, unité testable isolée → travail parallèle propre) consommée par deux adaptateurs
> (web HTMX + REST JSON). La table de correspondance de routes ci-dessous a été établie
> contre une base antérieure ; les routes « avant » sont à re-dériver des handlers réels de
> `main` au moment du portage.

> Objectif long terme : transformer vcluster-manager en **UI → API → opérateur → FluxCD**.
> Cette étape extrait une couche service réutilisable hors du monolithe HTMX, sans changer
> le comportement, pour rendre l'API (et plus tard l'opérateur) possibles.

## Problème de départ

Le monolithe soude trois responsabilités dans chaque handler :

1. **Transport HTTP** (`r.FormValue`, `r.PathValue`)
2. **Logique métier** (validation, génération YAML, commit GitLab, Keycloak, Rancher, Vault…)
3. **Présentation** (`h.render`, `h.renderPartial`, `redirectWithFlash`)

Conséquences : les routes `/api/...` actuelles renvoient du **HTML** (fragments HTMX), pas du
JSON — il n'existe donc **aucune API réutilisable**. Et la logique n'est testable qu'à travers
un `*http.Request`.

## Cible : 3 couches

```
internal/service   ← logique métier pure : (DTO + Actor) → (struct + error). PAS de net/http.
internal/handlers  ← adaptateur web (HTMX) : parse form → service → rend HTML     [= l'actuel, aminci]
internal/api       ← adaptateur REST (JSON) : décode JSON → service → encode JSON  [nouveau]
```

Les deux adaptateurs partagent **la même** instance de `*service.Service` → aucune logique
dupliquée. Les erreurs du service sont des sentinelles typées (`ErrForbidden`,
`ErrK8sUnavailable`, …) que chaque adaptateur traduit dans son transport (web = toast + statut ;
REST = code HTTP + `{"error": …}`).

### Principes

- **Acteur en paramètre** : l'identité et le RBAC passent par `models.Actor` (username + isAdmin),
  construit par l'adaptateur depuis la requête. Le service ne dépend jamais de `*http.Request`.
  `audit.LogActor(username, …)` double `audit.Log(r, …)` pour cette raison.
- **RBAC dans le service** : le check admin vit dans la méthode service (`ErrForbidden`), les deux
  adaptateurs en héritent.
- **État éphémère dans le service** : `migrations`, `vaultStates`, `deleting.json`, reconcilers —
  c'est exactement ce que le futur **opérateur/contrôleur** absorbera (deleting → finalizer,
  vaultStates → boucle de réconciliation).

## Découpage cible du service (7 fichiers)

| Fichier | Domaine |
|---|---|
| `service/vcluster.go` | List, Get, Create, UpdateSettings, Delete, Kubeconfig, ProdMR |
| `service/status.go` | Status, Quotas, FluxSummary, ClusterHealth |
| `service/rancher.go` | RancherStatus, Pair, Unpair |
| `service/velero.go` | Backups, BackupContent, TriggerBackup, DeleteBackup, Restore, RestoreStatus |
| `service/protection.go` | GetProtection, SetProtection ✅ **fait (pilote)** |
| `service/apps.go` | ListApps, MigrateApp |
| `service/platform.go` | UpdateChart, UpdateK8sVersion, UpdateArgoCDVersion, Config, VeleroConfig |
| `service/vault.go` + `reconcilers.go` | état async Vault, goroutines de réconciliation |

## Correspondance des routes

- **Web (HTMX, inchangé)** : `GET /vclusters`, `POST /vclusters/new`, fragments `/api/...` (HTML).
- **REST (JSON, nouveau)** : sous **`/api/v1/...`** pour éviter toute ambiguïté avec les fragments.

Exemple (pilote protection) :

| Web (HTML) | REST (JSON) | Service |
|---|---|---|
| `GET /api/vclusters/{name}/protection-status` | `GET /api/v1/vclusters/{name}/protection` | `GetProtection` |
| `POST /api/vclusters/{name}/enable-protection` | `PUT /api/v1/vclusters/{name}/protection` `{"enabled":true}` | `SetProtection` |
| `POST /api/vclusters/{name}/disable-protection` | `PUT /api/v1/vclusters/{name}/protection` `{"enabled":false}` | `SetProtection` |

## Phasage (incrémental, adapté au solo)

- **Phase A — couche service, comportement identique.** Extraire la logique domaine par domaine ;
  les handlers délèguent. UI et URLs inchangées.
- **Phase B — adaptateur REST en parallèle.** `/api/v1` (JSON) sur le même service. Les fragments
  HTMX restent.
- **Phase C (plus tard) — l'UI consomme le REST.** Migration progressive des fragments vers `/api/v1`.

## Points de vigilance

1. **Cohabitation `/api`** : le JSON va sous `/api/v1`, les fragments HTMX gardent leurs URLs.
2. **CSRF** : l'API REST est consommée par le même navigateur → on garde le double-submit cookie
   (le middleware s'applique aussi à `/api/v1`). Pas de bascule vers des tokens tant que l'UI est le
   seul client.
3. **`requireAdmin`** : remplacé au fil de l'eau par le check service (`ErrForbidden`).
4. **Structure `Service`** : une struct unique (deps + état), méthodes réparties en fichiers par
   domaine. Scindable plus tard.

## État d'avancement (branche `refactor/extract-api`, non poussée)

### Socle
- `internal/models/actor.go` — `Actor{Username, IsAdmin}`.
- `internal/audit/audit.go` — `LogActor` sans dépendance à `*http.Request`.
- `internal/service/service.go` — `Service`, construit via struct `Deps` (toutes les deps :
  cfg/parser/generator/gitlab/keycloak/rancher/vault/helmUpdater/argocdUpdater/ghReleases/notifier +
  k8sClients partagés), `k8sForEnv`, `envOrDefault`, sentinelles `ErrForbidden`/`ErrK8sUnavailable`.
- `internal/api/{api,json}.go` — adaptateur REST monté sous `/api/v1`, `actorFrom`, `writeServiceError`.
- `internal/handlers/handlers.go` — champ `svc`, helpers `actor(r)` + `Service()`.

### Domaines extraits (9)
`protection` · `rancher` · `apps` · `platform` · `status` · `settings` · `dashboard` ·
**`vcluster` (cœur)** · **`velero`**.
Chacun : `internal/service/<domaine>.go` (logique + sentinelles + tests éventuels), handler HTMX
qui délègue, `internal/api/<domaine>.go` (endpoints `/api/v1` + `register…Routes` câblés dans `Routes()`).
Deux revues (code + sécu) par lot : aucune régression web, RBAC/concurrence/mapping d'erreurs fidèles.

#### Cœur `vcluster` (`service/vcluster.go`)
Liste, détail, création, suppression **et** les machines à états asynchrones — c'est la matière
directe du futur opérateur :

| Service | Web | REST |
|---|---|---|
| `ListVClusters` / `UsedVeleroSlots` | `GET /vclusters`, formulaire de création | `GET /api/v1/vclusters` |
| `GetVCluster` | `GET /vclusters/{name}` | `GET /api/v1/vclusters/{name}` |
| `Create` | `POST /vclusters/new` | `POST /api/v1/vclusters` (201) |
| `GetDeleteConfirm` | page de confirmation | `GET /api/v1/vclusters/{name}/delete-preview` |
| `Delete` | `DELETE /vclusters/{name}` | `DELETE /api/v1/vclusters/{name}` (202 si async) |
| `GetVaultState` / `RetryVaultSetup` | badge de statut | `GET`/`POST /api/v1/vclusters/{name}/vault[/retry]` |

- Suppression : garde Rancher (dépairage avant destruction), `performDeletion` (cleanup K8s,
  commits fluxprod ou MR prod, GitLab/Keycloak/Vault), `runCleanupAndDelete`.
- Réconciliation au démarrage : `StartReconcilers()` (setup Vault en attente + nettoyages Rancher
  interrompus par un restart) — la boucle que l'opérateur remplacera.
- L'état `vaultStates` vit désormais dans le service ; les handlers ne le lisent que pour le badge.
- Formatage laissé aux adaptateurs : le service renvoie le TTL Velero brut, la vue en fait « 30j ».

#### `velero` (`service/velero.go`)

| Service | Web | REST |
|---|---|---|
| `ListBackups` | fragment `velero_backups.html` | `GET /api/v1/vclusters/{name}/backups` |
| `TriggerBackup` | bouton « Backup maintenant » | `POST /api/v1/vclusters/{name}/backups` (202) |
| `GetBackupContent` (admin) | fragment contenu | `GET …/backups/{backup}/content` |
| `DeleteBackup` | action de la liste | `DELETE …/backups/{backup}` (204) |
| `CreateRestore` | formulaire de restauration | `POST …/restores` (202) |
| `GetRestoreStatus` | polling HTMX 3 s | `GET …/restores/{restore}` |

Les gardes de la restauration in-place (backup `Completed` avant destruction, reprise de Flux
côté serveur, attente du pod avant suppression du PVC) vivent avec l'opération qu'elles protègent.
`BackupContentError` porte l'étape en échec (`url`/`download`/`decompress`/`read`) : chaque
adaptateur reste précis sans partager de formatage.

### Reste dans les handlers (à extraire)
- `cluster_config` (mute la map `k8sClients` au runtime) et `ClusterHealth`.
- `api.go` : `DownloadKubeconfig`, `CreateAppManifestsRepo`, `CreateProdMR`.

### Corrigé au passage
- `rancherCleanupManifest` n'est plus dupliqué : une seule copie, dans le service.
- `runCleanupAndDelete` prenait une interface : un `*StatusClient` nil s'y cachait dans une valeur
  d'interface non-nil, donc le garde `k8s != nil` laissait passer un appel sur récepteur nil.
  Paramètre repassé au type concret.
- `UpdateVeleroConfig` remplaçait le générateur du handler alors que le service gardait l'ancien :
  la nouvelle rétention par défaut n'atteignait plus les `values.yaml` générés. Le domaine est passé
  dans le service (`gen()` / `setGenerator()` sous mutex — le swap se faisait sans verrou).
- REST : `writeServiceError` mappe les sentinelles domaine (404/400/409/502/503) et ne renvoie plus
  les erreurs internes brutes au client (500 générique + log).

### Dette / suivis notés par les revues
- **Fait** — tout `/api/v1` (lectures comprises) est admin-only. Porte unique : `API.handle(mux,
  pattern, h)` remplace les `mux.HandleFunc` directs dans les 9 fichiers de routes et enveloppe
  chaque handler avec `adminOnly` (`internal/api/api.go`), qui construit l'`Actor` via `actorFrom`
  et renvoie 403 `{"error": "forbidden: admin privilege required"}` si `!actor.IsAdmin`. La porte est
  dans l'adaptateur REST et pas dans le service : le service reste utilisé tel quel par les handlers
  web, où les lecteurs voient toujours les fragments HTMX (dashboard, status, quota, backups…) — seul
  le canal REST devient admin-only, ce n'est pas une règle métier.
- **Fait** — validation de `name`/`env` dans le service, défense en profondeur. Sentinelle
  `ErrInvalidEnv` à côté de `envOrDefault` (`service.go`), validateur `validEnv` (`""`/`preprod`/`prod`) ;
  `validName` réutilise `nameRegex` existant (`vcluster.go`). Appliqués à toutes les méthodes listées
  qui reçoivent `name`/`env` depuis un path/query externe (vcluster, status, protection, rancher,
  velero, apps, settings). Le risque réel confirmé en creusant le code : `env` non filtré atteint des
  chemins fluxprod (`clusters/{env}/vclusters/...` dans `gitops.Parser`) et des chemins Vault
  (`kubernetes-vcluster-{name}-{env}`) — un `env` forgé du type `../../etc` y est un vecteur de
  traversal. `name` était déjà protégé par `nameRegex` (pas de `..` ni de `/` dans le charset accepté).
  Pour `CreateRestore`/`MigrateApp`, `targetName` est aussi validé quand non vide : il devient
  directement un nom de namespace K8s (`vcluster-{targetName}`) ou un chemin GitLab, même surface de
  risque que `name`.
  Pour les méthodes qui ne retournaient pas d'erreur avant (`GetProtection`, `GetRancherStatus`,
  `GetVaultState`, `UsedVeleroSlots`, `MigrationTargets`) : signature inchangée. Ces méthodes ne
  touchent jamais un chemin fichier ou un client externe avec l'entrée invalide — c'est soit une clé
  de map en mémoire (`GetVaultState`), soit un retour déjà vide/désactivé identique au cas
  « client non configuré » existant (`GetProtection`, `GetRancherStatus`). Elles retombent sur cette
  même valeur zéro au lieu d'ajouter un `error` que tous les appelants (web + REST) auraient dû
  apprendre à gérer pour un cas qui, en usage normal, ne se produit jamais (l'UI n'envoie jamais autre
  chose que `""`/`preprod`/`prod` et des noms déjà validés à la création).

- Les champs `Handlers` des clients d'intégration ont disparu (parser, keycloak, ghReleases,
  helm/argocd updaters) : tout passe par le service. Restent `rancher`/`vault`, lus par la page de
  config pour dire s'ils sont configurés.

### Sécu déjà corrigée
Bumps go-jose/x/net/spdystream/x/crypto (vulns govulncheck), contenu backup Velero réservé admin,
token Rancher retiré des logs.

### Prochains domaines suggérés
Le reste est du résidu : `cluster_config`/`ClusterHealth` (qui mute la map `k8sClients`) et les
trois handlers d'`api.go`. Le vrai chantier suivant est l'étape 2 — concevoir la CRD `VCluster` et
transformer `Create`/`Delete`/`StartReconcilers` en reconcile + finalizers (voir
`docs/etude-cluster-api.md` pour le CRD multi-backend `vcluster | capi`).
