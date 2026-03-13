# vCluster Manager - Instructions Claude

## Contexte

Application web Go pour gerer les vClusters Kubernetes. Elle interagit avec :
- **fluxprod** (repo GitOps) : genere et commit des fichiers YAML via GitLab API
- **FluxCD** : reconcilie les changements sur les clusters K8s
- **GitLab** : creation des repos app-manifests pour ArgoCD
- **Keycloak** : creation des clients OIDC pour ArgoCD

Le deploiement se fait via FluxCD depuis le repo GitOps (`clusters/{env}/vcluster-manager/`).

## Architecture

```
cmd/server/main.go                  # Entry point, routing, init clients
internal/
  config/config.go                   # Env vars parsing + persistent settings (data/settings.json)
  config/deleting.go                 # Etat persistant "suppression en cours" (data/deleting.json)
  auth/
    oidc.go                          # Keycloak OIDC middleware + NoopMiddleware (dev)
    csrf.go                          # CSRF middleware double-submit cookie (csrf_token + X-CSRF-Token)
    rate_limiter.go                  # Rate limiter par IP (token bucket, golang.org/x/time/rate)
  argocd/updater.go                  # Mise a jour version ArgoCD globale dans fluxprod (lib/tenant-template/argocd/base)
  audit/log.go                       # Audit log structure : qui + action + vcluster + env
  github/releases.go                 # Client GitHub API : derniere release vcluster + ArgoCD + versions K8s disponibles (cache 1h)
  helmcharts/updater.go              # Mise a jour du chart vcluster dans platform-helm-charts (GitLab projet 1891)
  gitops/
    parser.go                        # Lit les vclusters depuis le repo fluxprod (ListVClusters parallelise) ; interface fileProvider pour tests
    generator.go                     # Genere tous les YAML (port de create-vcluster.sh)
    gitlab.go                        # Client go-gitlab : commit, ListFiles, CreateAppManifestsRepo (cache memoire TTL 30s)
    argocd_assets.go                 # go:embed argocd.png avatar + GenerateAppManifestsREADME()
    assets/argocd.png                # Avatar ArgoCD pour les repos app-manifests
  keycloak/client.go                 # Client Keycloak Admin API (client_credentials, token cache auto-refresh)
  kubernetes/status.go               # client-go dynamique : HelmRelease + Kustomization + K8s version + apply manifest in vcluster + HasRancherAgents + GetNamespaceProtection/SetNamespaceProtection + ListVeleroBackups + GetBackupContentURL + CreateVeleroRestore + GetRestoreStatus
  metrics/                           # Middleware Prometheus + handler /metrics
  notify/webhook.go                  # Notifier webhook generique (Slack/Mattermost/RC compatible)
  rancher/client.go                  # Client Rancher API v3 : import/delete cluster, registration tokens
  models/vcluster.go                 # VCluster, QuotaConfig, CreateRequest, UpdateRequest, StatusInfo, ReleaseInfo
  vault/client.go                    # Client Vault : AppRole auth, setup Kubernetes auth backend par vcluster
  handlers/
    handlers.go                      # Init templates (par page: layout + page + partials), render(), sendNotification()
    dashboard.go                     # GET / — grouped by env, latest release banner
    vcluster.go                      # List, CreateForm, Create, Detail, DeleteConfirm, Delete
    settings.go                      # POST /vclusters/{name}/settings?env= — update values.yaml + RBAC (preprod/prod independants)
    api.go                           # HTMX fragments : StatusFragment, QuotaForm, UpdateChart, UpdateK8sVersion, UpdateArgoCDVersion, PairRancher, RancherStatus, ProtectionStatus, Enable/DisableProtection, VeleroBackupList, VeleroBackupContent, CreateVeleroRestore, VeleroRestoreStatus
  version/version.go                 # Version lue depuis le fichier VERSION a la compilation (go:embed)
web/
  templates/
    layout.html                      # {{define "layout"}} — Tailwind nav + {{template "content" .}} + CSRF hook HTMX
    cluster_config.html              # {{define "content"}} — page config complete (GitLab, Keycloak, OIDC, Helm, K8s, Rancher, Vault, Webhook)
    dashboard.html                   # {{define "content"}} — range .Groups + latest release banner
    vcluster_list.html               # Cards grid par env
    vcluster_create.html             # Formulaire hx-post
    vcluster_detail.html             # Detail + form edition inline (K8s version, ArgoCD version)
    vcluster_delete.html             # Confirmation hx-delete
    partials/
      status_badge.html              # HR + KS + Chart version + K8s version badges + quota usage
      quota_form.html                # Inline quota edit
      toast.html                     # Notifications
      rancher_status.html            # Toggle Rancher : Paired / Pairing / Unknown / ManuallyPaired / Cleaning / Off
      protection_status.html         # Toggle protection namespace : padlock on/off, admin seulement
      velero_backups.html            # Liste des backups Velero par vcluster (tableau + actions)
      velero_backup_content.html     # Contenu d'un backup (JSON formaté via DownloadRequest)
      velero_restore_status.html     # Statut d'une restauration Velero (polling HTMX)
      flux_summary.html              # Compteurs HelmReleases ready/total par env
  static/app.css                     # Animations
VERSION                              # Version courante (ex: 1.1.0), lue par internal/version/version.go
```

## Patterns importants

### Templates
- Chaque page est parsee individuellement avec layout + partials (pas de ParseGlob global)
- Le handler appelle `h.render(w, "dashboard.html", data)` qui execute `layout` -> `content`
- Les partials HTMX sont rendus via `h.renderPartial(w, "status_badge.html", data)`
- Les `{{define "content"}}` sont dans chaque page, PAS de `{{template "layout" .}}` en bas

### GitOps flow
- L'app ne modifie JAMAIS le cluster directement
- Toute modification = commit Git via GitLab API -> FluxCD reconcilie

### Generator
- Port exact de `create-vcluster.sh` (dans fluxprod)
- Genere 8 fichiers (sans ArgoCD) ou 14 fichiers (avec ArgoCD) par env
- Les patterns de nommage sont documentes dans fluxprod/CLAUDE.md
- **IMPORTANT** : Tout nouveau fichier tenant ajoute dans fluxprod (`clusters/{env}/vclusters/{name}/tenant/`) DOIT etre ajoute dans `generator.go` :
  1. `GenerateVCluster()` : ajouter le fichier dans la liste `files`
  2. `tenantKustomizationYAML()` : ajouter l'entree dans les ressources (argocd=true ET argocd=false si applicable)
  3. Creer la fonction generatrice correspondante
- **RECIPROQUE** : Toute modification de `generator.go` doit etre synchronisee avec les fichiers existants dans fluxprod (pour eviter que les regenerations ecrasent des configs manuelles)

### Versions
- Le StatusFragment (HTMX) affiche la version du chart vcluster (`status.history[0].chartVersion` du HelmRelease) et la version K8s (via kubeconfig secret `vc-vcluster-{name}-ext` + Discovery API)
- Le dashboard affiche un bandeau avec la derniere release vcluster upstream (GitHub API `loft-sh/vcluster`, cache 1h) + version actuelle du chart sur preprod + lien vers les release notes GitHub
- Si la version du chart est inferieure a la derniere release, un bouton "Mettre a jour" bumpe la version dans `Chart.yaml` (version + appVersion + dependency) de platform-helm-charts : commit sur preprod + MR preprod→master pour la prod
- Un bandeau dedie permet de modifier la version K8s par defaut du chart (`vcluster.controlPlane.distro.k8s.image.tag` dans `charts/vcluster/values.yaml`) : commit sur preprod + MR preprod→master pour la prod. Les versions disponibles sont recuperees depuis le registre GHCR `ghcr.io/loft-sh/kubernetes` (les images reellement utilisees par vcluster, derniere patch par version mineure, cache 1h), avec possibilite de saisir une version custom
- Preprod et prod sont independants (branches differentes dans platform-helm-charts). La mise a jour prod passe toujours par une MR
- Detection des MR en attente : avant de creer une nouvelle MR, le systeme verifie s'il en existe deja une ouverte (prefixes `update-chart-`, `update-k8s-`, `update-argocd-`). Si oui, le bouton d'action est remplace par un lien vers la MR existante
- La version du chart n'est pas epinglee dans fluxprod : elle vient du GitRepository `platform-helm-charts` (reconcileStrategy: Revision)
- La version K8s est configurable via `controlPlane.distro.k8s.version` dans values.yaml (champ dans le formulaire settings)
- Le StatusFragment affiche aussi la consommation des ResourceQuota K8s (`status.used` vs `status.hard`) pour CPU, Memory et Storage. Les badges sont colores selon le taux d'utilisation : vert (<70%), jaune (70-90%), rouge (>90%). Les vclusters avec `NoQuotas` n'affichent pas de badges quota. La lecture utilise `k8s.io/apimachinery/pkg/api/resource` pour parser les Quantity K8s (millicores, Gi, Ti, etc.)
- **ArgoCD version management** : deux niveaux de gestion
  - **Global** : la version ArgoCD est epinglee dans `lib/tenant-template/argocd/base/kustomization.yaml` via `images: - name: quay.io/argoproj/argocd, newTag: vX.Y.Z`. Le dashboard affiche un bandeau orange avec la derniere release ArgoCD (GitHub API `argoproj/argo-cd`, cache 1h), les versions preprod/prod, et un bouton "Mettre a jour" (admin). La mise a jour commit sur preprod + cree une MR vers master. Le updater est dans `internal/argocd/updater.go` et utilise le GitLab client fluxprod (pas helmcharts)
  - **Unitaire** : chaque vcluster avec ArgoCD peut overrider la version via `images:` dans son overlay `clusters/{env}/vclusters/{name}/tenant/argocd/kustomization.yaml`. Si vide, la version globale s'applique. Le champ est editable dans la page detail (settings). Le generator ajoute la section `images:` uniquement si `ArgoCDVersion != ""`
  - Le parser lit la version override depuis l'overlay kustomization (methode `parseArgoCDVersion`)
- Le badge K8s dans status_badge.html indique visuellement quand une mise a jour est en cours : les templates HTMX passent `?configVersion=` (version configuree dans values.yaml) a StatusFragment, qui compare avec la version K8s reelle du cluster. Si elles different, le badge passe en jaune avec `v1.32 → v1.33`. Quand FluxCD reconcilie et que les versions correspondent, le badge repasse en indigo

### Cache GitLab + Parallelisation
- `GitLabClient` embarque un cache memoire (map + sync.RWMutex) avec TTL 30s
- Cle de cache : `"{method}:{branch}:{path}"` — ex: `"get:preprod:clusters/preprod/vclusters/demos/values.yaml"`
- `GetFile` et `ListFiles` sont caches automatiquement (tous les callers en beneficient)
- `Commit()` invalide tout le cache apres un commit reussi
- `InvalidateCache()` est publique pour usage externe si besoin
- `ListVClusters` parse les 6 vclusters en parallele (goroutines + sync.WaitGroup)
- Le cache est thread-safe : les goroutines paralleles peuvent lire/ecrire concurremment

### Velero backup toggle
- Le backup Velero est activable/desactivable par vcluster (champ `VeleroEnabled` dans `CreateRequest` et `UpdateRequest`)
- Quand desactive, `values.yaml` genere `veleroBackup: enabled: false` sans schedule
- Quand active, le schedule complet est genere (comme avant)
- La checkbox est cochee par defaut a la creation
- En edition, la section heure est masquee si le backup est desactive

### Suppression avec etat visuel
- Quand un vcluster est supprime, il reste visible dans l'UI avec un badge rouge "Suppression en cours"
- Un fichier `data/deleting.json` persiste les vclusters en cours de suppression (survit au restart)
- Le StatusFragment HTMX poll toutes les 30s avec `?deleting=true` pour verifier si le HelmRelease K8s existe encore
- Quand le HelmRelease disparait, l'entree est nettoyee et le dashboard rafraichi via `HX-Redirect: /`
- Pour la prod deployee : une MR de suppression est creee, le badge affiche un lien vers la MR
- Auto-nettoyage des entrees > 24h dans `deleting.json`
- Les vclusters prod "pending" (pas encore sur master) sont supprimes directement sans AddDeleting (pas de HR K8s)

### Vclusters prod editables (pre-merge)
- Les vclusters prod qui existent sur preprod mais pas encore sur master (= PendingMR) sont cliquables et editables
- Les modifications vont directement sur la branche preprod (commit direct, pas de MR)
- Le toggle ArgoCD on/off est possible : supprime tous les fichiers et regenere via `GenerateVCluster()`
- La page detail affiche un bandeau jaune "Non deploye" avec instructions pour deployer
- La suppression d'un pending se fait directement sur preprod (pas de MR, pas d'AddDeleting)

### RBAC (roles admin / lecteur)
- Deux roles : **admin** (lecture + ecriture) et **lecteur** (consultation uniquement)
- Groupes OIDC admin : `exploit`, `it` (definis dans `auth.adminGroups`)
- Le login local (`admin`) est toujours admin (issuer `vcluster-manager-local`)
- `auth.IsAdmin(r)` extrait les groupes du JWT cookie sans verification (comme `UserFromRequest`)
- `handlers.requireAdmin(w, r)` : renvoie 403 + toast si pas admin
- Operations protegees : CreateForm, Create, DeleteConfirm, Delete, UpdateSettings, UpdateChart, UpdateK8sVersion, UpdateArgoCDVersion, PairRancher, UnpairRancher
- Templates : les boutons/formulaires d'ecriture sont masques avec `{{if .User.isAdmin}}`
- Pages en lecture libre : dashboard, liste, detail (sans edition), status

### Protection CSRF
- Pattern double-submit cookie : cookie `csrf_token` (SameSite=Strict, HttpOnly=false) + header `X-CSRF-Token`
- Middleware `auth.CSRFMiddleware` applique a toutes les routes protegees (apres auth, avant handlers)
- GET/HEAD/OPTIONS : cree le cookie si absent (token hex 64 chars = 32 bytes cryptographiquement aleatoires)
- POST/PUT/DELETE/PATCH : verifie que le header `X-CSRF-Token` (HTMX) ou le champ `_csrf` (form classique) correspond au cookie → 403 sinon
- HTMX : le hook `htmx:configRequest` dans `layout.html` injecte automatiquement le header sur chaque requete
- `csrf.go` + `csrf_test.go` (12 tests)

### Rate limiting
- `auth.NewRateLimiter(rate.Limit(20), 50)` — 20 req/s, burst 50 par IP
- Middleware applique avant auth (protege aussi les endpoints non authentifies)

### Audit log
- `audit.Log(r, action, name, env, extra...)` — loggue qui (user JWT) fait quoi sur quel vcluster
- Actions tracees : create, delete, update-settings, update-chart, update-k8s-version, update-argocd-version, pair-rancher, unpair-rancher

### Notifications webhook
- `internal/notify/webhook.go` — `Notifier` : POST JSON `{"text": "..."}` vers `WEBHOOK_URL`
- Compatible Slack incoming webhooks, Mattermost, Rocket.Chat, tout endpoint HTTP JSON
- Timeout 10s, erreurs non bloquantes (loggues, n'interrompent pas le traitement)
- Variable : `WEBHOOK_URL` (optionnel — si absent, aucune notification)
- **Deux evenements notifies** :
  1. Quand le commit GitOps de suppression preprod est pose → *"Suppression du vcluster NAME (preprod) en cours..."*
  2. Quand le HelmRelease K8s disparait (suppression confirmee) → *"vcluster NAME (env) supprime avec succes."*
- `h.sendNotification(text)` dans `handlers.go` : helper qui appelle le notifier en goroutine (non bloquant)

### Variables d'environnement configurables
- `ADMIN_GROUPS` : groupes OIDC admin (virgule-separes), defaut `platform-admins,ops` — configure via `auth.SetAdminGroups()` apres `config.Load()`
- `DEFAULT_RBAC_GROUP` : groupe RBAC par defaut pour les nouveaux vclusters, defaut `it`
- `FLUXPROD_CLUSTERS_PATH` : dossier racine des clusters dans le repo GitOps, defaut `clusters`
- `FLUXPROD_ARGOCD_KUST_PATH` : chemin du kustomization ArgoCD global, defaut `lib/tenant-template/argocd/base/kustomization.yaml`
- `HELM_CHARTS_VCLUSTER_PATH` : chemin du chart vcluster dans platform-helm-charts, defaut `charts/vcluster`
- `VAULT_KV_ARGOCD_ROOTAPPS` : chemin Vault KV pour les credentials ArgoCD root apps, defaut `secret/data/vcluster/argocd/rootapps`
- `VAULT_KV_ARGOCD_REPO` : chemin Vault KV pour les credentials repo ArgoCD, defaut `secret/data/vcluster/argocd/repo`
- Toutes ces variables ont des defaults backward-compatible
- Consulter `FORK.md` pour les details de portabilite et de configuration

### Accès à l'API d'un vcluster depuis vcluster-manager

**RÈGLE ABSOLUE** : toute fonction qui doit se connecter à l'API K8s *à l'intérieur* d'un vcluster (pas au cluster host) doit utiliser l'un des trois helpers de `internal/kubernetes/status.go`. Ne jamais appeler `getInternalKubeconfig` / `GetKubeconfig` + `clientcmd.RESTConfigFromKubeConfig` directement.

| Helper | Fournit | Quand utiliser |
|---|---|---|
| `withVClusterPortForward(ctx, name, fn(*rest.Config))` | `*rest.Config` pointant sur le vcluster via port-forward | appliquer des manifests, opérations nécessitant la config brute |
| `withVClusterDynClient(ctx, name, fn(dynamic.Interface))` | `dynamic.Interface` vers le vcluster | lister/get/delete des CRDs ou ressources non typées |
| `withVClusterClientset(ctx, name, fn(*k8sclient.Clientset))` | `*k8sclient.Clientset` vers le vcluster | opérations typées (Jobs, ServiceAccounts, Discovery...) |

**Logique interne** : si `StatusClient.configSource == "in-cluster"` (vcluster-manager tourne sur le même cluster) → kubeconfig interne direct. Sinon → port-forward SPDY sur le pod vcluster (port 8443), kubeconfig interne avec URL réécrite en `https://127.0.0.1:{localPort}`.

**Fonctions qui respectent déjà ce pattern** : `ApplyManifestToVCluster`, `WaitForJobComplete`, `CreateVaultReviewerToken`, `DeleteNamespaceInVCluster`, `NamespaceExistsInVCluster`, `ListVClusterArgoApps`.

`ApplyManifestToVClusterViaPortForward` est un alias déprécié de `ApplyManifestToVCluster` — ne pas l'utiliser dans de nouveau code.

### Rancher (appairage/desappairage)
- Client optionnel : configure via `RANCHER_URL` + `RANCHER_TOKEN` (Bearer token API v3)
- **Activation par env** : `RANCHER_ENABLED_PREPROD=true` / `RANCHER_ENABLED_PROD=true` pour activer l'appairage par environnement
- **UI** : toggle switch dans le header de la page detail (toggle horizontal avec icones, vert + checkmark pour ON, rouge + X pour OFF), admin only, non-pending uniquement
- **Appairage** : POST `/api/vclusters/{name}/pair-rancher?env={env}` (admin only)
  1. Double verification avant lancement :
     - `rancher.FindClusterByName(name)` → erreur si deja present dans Rancher (evite double-pairing)
     - `k8s.HasRancherAgents(ctx, name)` → erreur si agents detectes via pods K8s (appairage manuel)
  2. Affiche immediatement "Appairage en cours..." (disabled toggle)
  3. Goroutine asynchrone :
     - `rancher.ImportCluster(name)` → cree un cluster importe `vcluster-{name}` dans Rancher
     - Attend le registration token (retry loop, tokens crees de maniere asynchrone par Rancher)
     - Telecharge le manifest d'enregistrement depuis `manifestUrl`
     - `k8s.ApplyManifestToVClusterViaPortForward()` → applique le manifest dans le vcluster via port-forward
  4. HTMX polling (3s) sur `/api/vclusters/{name}/rancher-status` pour rafraichir l'etat (toggle se reactive quand paired=true)
- **Detection appairage manuel** : `k8s.HasRancherAgents(ctx, name)` liste les pods du namespace `vcluster-{name}` sur le host cluster avec le label `vcluster.loft.sh/namespace=cattle-system` (pods synces par vcluster depuis l'interieur). Detecte les vclusters pairas manuellement avec un nom Rancher different de la convention `vcluster-{name}`.
- **Etats UI** (template `rancher_status.html`) :
  - `Paired` : toggle vert actif
  - `Pairing` : toggle desactive + "Appairage en cours..." (poll 3s)
  - `Unknown` : toggle gris desactive + ⚠️ + "Inconnu" (API Rancher inaccessible, poll 10s)
  - `ManuallyPaired` : toggle ambre desactive + ⚠️ + "Manuel" (agents detectes mais pas trouve par nom)
  - `Cleaning` : badge "Nettoyage en cours" (poll 5s)
  - Off : toggle rouge
- **Desappairage** : POST `/api/vclusters/{name}/unpair-rancher?env={env}` (admin only)
  1. `rancher.FindClusterByName(name)` → supprime de Rancher si trouve (sinon log + continue)
  2. Deploy `rancher-cleanup` job dans le vcluster via port-forward (goroutine)
  3. Retourne immediatement le toggle en mode "Cleaning" (poll HTMX)
- **Suppression vcluster avec Rancher** : si le vcluster est encore appaire au moment de la suppression, le desappairage est execute automatiquement avant la suppression GitOps
- Page config : section lecture seule avec URL, etat du token, et checkboxes d'activation par env (lecture seule, configurees via env vars)
- **Port-forward pour cross-cluster** : `ApplyManifestToVClusterViaPortForward` cree un `kubectl port-forward` temporaire vers le service vcluster sur le cluster distant, modifie le kubeconfig pour utiliser `localhost:18443`, applique le manifest, puis ferme le port-forward. Permet d'acceder aux vclusters prod depuis vcluster-manager sur preprod/exploit.
- `ApplyManifestToVCluster` utilise le discovery API pour resoudre les GVR dynamiquement (multi-doc YAML)

### Protection namespace
- Toggle par vcluster pour ajouter/supprimer l'annotation `protect-deletion: "true"` sur le namespace `vcluster-{name}` du cluster host
- Protege contre les suppressions accidentelles (`kubectl delete ns`) via un ValidatingWebhookConfiguration deploye sur les clusters
- Persistance garantie : vcluster-manager utilise un field manager propre (Server-Side Apply) → FluxCD ne possede pas ce champ et ne l'ecrase pas lors des reconciliations
- UI : toggle dans le header de la page detail (comme Rancher), visible meme pour les lecteurs (lecture seule), admin pour toggle
- **Integration suppression** : si la protection est active quand une suppression GitOps est lancee, elle est desactivee automatiquement (SetNamespaceProtection false) avant le commit fluxprod
- Page delete : bandeau amber si protection active au moment ou l'admin confirme (informatif, la desactivation est auto)
- Routes : `GET /api/vclusters/{name}/protection-status`, `POST enable-protection`, `POST disable-protection`
- RBAC K8s : verbe `update` sur `namespaces` dans `deploy/base/rbac.yaml`

### Backups Velero
- **Lister** : `GET /api/vclusters/{name}/velero/backups?env=` → liste les `Backup` Velero (CRD velero.io/v1) dont `spec.includedNamespaces` contient `vcluster-{name}`, filtre cote Go
- **Contenu** : `GET /api/vclusters/{name}/velero/backups/{backup}/content?env=` → cree un `DownloadRequest` (kind: BackupResourceList), poll jusqu'a phase `Processed`, recupere le `downloadURL` (presigned S3), proxifie le JSON formaté. DownloadRequest supprime apres usage (defer best-effort).
- **Restaurer** : `POST /api/vclusters/{name}/velero/restore?env=&backup=&target=` → cree un objet `Restore` avec `spec.includedNamespaces: [vcluster-{name}]` et optionnellement `spec.namespaceMapping` pour cross-vcluster. Retourne immediatement le partial `velero_restore_status.html` avec polling HTMX toutes les 3s.
- **Statut restore** : `GET /api/vclusters/{name}/velero/restore/{restore}/status?env=` → retourne la phase du Restore. Le polling s'arrete quand phase est `Completed`, `Failed` ou `PartiallyFailed`.
- **RBAC K8s** : `deploy/base/rbac.yaml` contient `velero.io` resources : backups, restores, downloadrequests, schedules avec verbes get/list/create/delete.
- **GVRs** dans `status.go` : `veleroBackupGVR`, `veleroDownloadRequestGVR`, `veleroRestoreGVR` (group: `velero.io`, version: `v1`)
- Affichage dans la page detail uniquement si `VCluster.Velero.Enabled && !Pending`

### Retention Velero (TTL)
- **Format de saisie** : `30j` (jours), `12h` (heures), `90m` (minutes) — `parseTTLText()` dans `handlers.go`
- **Conversion Velero** : `30j` → `720h0m0s`, `12h` → `12h0m0s`, `90m` → `0h90m0s` (format Go duration attendu par Velero)
- **Affichage inversé** : `ttlToText()` reconvertit `720h0m0s` → `30j`, `36h` reste `36h` (pas divisible par 24)
- **Globale** : configurable sur `/config` (champ `velero_ttl`), stockee dans `Config.VeleroDefaultTTL`, persistee dans le state backend, utilisee comme valeur par defaut lors de la generation du `values.yaml`
- **Par vcluster** : champ `velero_ttl` dans les settings de la page detail → ecrit `veleroBackup.ttl` dans `values.yaml` via `GenerateUpdatedValues()` (GitOps)
- **Mode GitOps exclusivement** : toute modification = commit dans `clusters/{env}/vclusters/{name}/values.yaml` dans fluxprod

### Configuration Velero (`/config`)
- Section editable (admin) dans la page `/config` : TTL par defaut, URL S3, bucket preprod, bucket prod
- `POST /config/velero` → `UpdateVeleroConfig` handler dans `handlers.go`
- Apres sauvegarde : regenere le generateur avec le nouveau TTL + commit `clusters/{env}/velero/values.yaml` dans fluxprod (branche preprod) pour les deux envs dont le bucket est defini
- Contenu genere (`generateVeleroValuesYAML`) : `configuration.backupStorageLocation` avec bucket, s3Url, s3ForcePathStyle, checksumAlgorithm
- Determine `create` ou `update` en appelant `GetFile` avant le commit (evite erreur 400 GitLab)
- Les variables `VELERO_S3_URL`, `VELERO_BUCKET_PREPROD`, `VELERO_BUCKET_PROD` initialisent les valeurs au demarrage ; modifiables ensuite via l'UI sans redemarrage

### Page Configuration (`/config`)
- Vue d'ensemble de toutes les integrations : GitLab, Keycloak, OIDC, Auth locale, Helm Charts, Clusters K8s, Velero
- Les secrets (tokens, passwords) ne sont JAMAIS affiches — seulement des booleens `Configured` et `******`
- Sections editables : Clusters K8s (upload kubeconfig) + Velero (TTL, S3 URL, buckets)
- Section aide avec toutes les variables d'environnement

## Dependances Go

- `github.com/xanzy/go-gitlab` v0.115 — API a changee :
  - `File.Content` est base64, pas de `Decode()` — utiliser `base64.StdEncoding.DecodeString`
  - `FileAction()` prend `FileActionValue`, pas `string`
  - `DeleteProject()` requiert `*DeleteProjectOptions{}`
  - `EnableDeployKey()` (pas `EnableProjectDeployKey`)
- `k8s.io/client-go` + `k8s.io/apimachinery` — dynamic client pour CRDs Flux + ResourceQuota usage
- `github.com/coreos/go-oidc/v3` + `golang.org/x/oauth2` — auth OIDC
- `gopkg.in/yaml.v3` — parsing values.yaml

## Deploiement

- Image Docker : `ghcr.io/gmalfray/vcluster-manager` (build via GitHub Actions)
- FluxCD : manifests dans `deploy/` (base + overlays)
- Namespace K8s : `vcluster-manager`
- ServiceAccount avec RBAC pour lire HelmReleases, Kustomizations, Secrets, ResourceQuotas
- Voir `FORK.md` pour le guide de déploiement complet

## Branches

- `master` : deploye en prod via FluxCD (branche master de fluxprod)
- `preprod` : deploye en preprod via FluxCD (branche preprod de fluxprod)

## Regles

- Tester avec `go build ./...` et `go vet ./...` avant de committer
- **Avant toute modification des configs tenant dans fluxprod** (`tenant/kustomization.yaml`, ajout de `*_kustomization.yaml`) : verifier `internal/gitops/generator.go` et synchroniser si necessaire
- **Avant toute modification de `generator.go`** : verifier que les fichiers existants dans fluxprod sont coherents et les mettre a jour si necessaire

## TODO

### Resilience

### Portabilite Git provider
- [ ] Support GitHub (ou tout autre provider Git) : actuellement couple a l'API GitLab via `github.com/xanzy/go-gitlab`. Necessiterait une interface `GitProvider` abstraite + reimplementation complete de `internal/gitops/gitlab.go` (commits multi-fichiers atomiques, MR→PR, deploy keys, creation de repos dans une org). Voir analyse dans `FORK.md`. Grosse feature, a planifier separement.


### UX / Internationalisation
- [ ] Support multilingue (i18n) — l'interface est actuellement en francais mixte avec quelques termes anglais ; prevoir FR/EN minimum via un mecanisme de traduction (fichiers de messages, Accept-Language, ou cookie de preference)

### A retirer quand resolu
- [ ] Retirer le workaround Pod exclusion ArgoCD (`fluxprod/lib/tenant-template/argocd/base/configmap-argocd-cm.yaml`) quand le bug est corrige upstream (ArgoCD 3.3.3+)

### Fait (v1.1.0)
- [x] Numero de version : fichier `VERSION` + `internal/version/version.go` (go:embed), affiche dans la nav
- [x] Rate limiting : `auth.NewRateLimiter` (20 req/s, burst 50) sur toutes les routes
- [x] Protection CSRF : double-submit cookie `csrf_token` + header `X-CSRF-Token` (voir `### Protection CSRF`)
- [x] Audit log : `audit.Log(r, action, name, env)` sur toutes les operations d'ecriture
- [x] Metriques Prometheus : middleware `metrics.Middleware` + handler `GET /metrics`
- [x] Notification webhook : `internal/notify/webhook.go` + variable `WEBHOOK_URL` (voir `### Notifications webhook`)
- [x] Tests unitaires generator : 25 tests dans `internal/gitops/generator_test.go`
- [x] Tests unitaires parser : 17 tests dans `internal/gitops/parser_test.go` (via interface `fileProvider`)
- [x] Tests unitaires handlers : 17 tests dans `internal/handlers/handlers_test.go`
- [x] Tests CSRF : 12 tests dans `internal/auth/csrf_test.go`
- [x] Detection appairage Rancher manuel : `k8s.HasRancherAgents()` + etats UI Unknown / ManuallyPaired
- [x] Fork portability : toutes les valeurs hardcodees remplacees par env vars avec defaults backward-compat (`ADMIN_GROUPS`, `DEFAULT_RBAC_GROUP`, `FLUXPROD_CLUSTERS_PATH`, `FLUXPROD_ARGOCD_KUST_PATH`, `HELM_CHARTS_VCLUSTER_PATH`, `VAULT_KV_ARGOCD_ROOTAPPS`, `VAULT_KV_ARGOCD_REPO`)
- [x] Backend de persistence configurable : `STATE_BACKEND=file` (defaut) ou `STATE_BACKEND=configmap` (ConfigMap K8s `vcluster-manager-state`, survit au rescheduling sans PVC). Interface `stateBackend` dans `internal/config/backend.go`, implementations `fileBackend` et `configmapBackend`. RBAC Role namespaced dans `deploy/base/rbac.yaml`.
- [x] Retries GitLab API : `withRetry()` dans `internal/gitops/gitlab.go` (3 tentatives, backoff 2s/5s/10s, uniquement sur 5xx/429/erreurs reseau). Metriques `gitlab_api_errors_total` et `gitlab_api_retries_total`.

## Repo GitOps associe

Le repo GitOps (votre "fluxprod") contient :
- Les manifests FluxCD pour deployer vcluster-manager
- Les configurations des vclusters par environnement (`clusters/{env}/vclusters/{name}/`)
- Voir `FORK.md` §3 pour la structure attendue
