# vCluster Manager

[![Build](https://github.com/gmalfray/vcluster-manager/actions/workflows/build.yaml/badge.svg)](https://github.com/gmalfray/vcluster-manager/actions/workflows/build.yaml)
[![Latest release](https://img.shields.io/github/v/release/gmalfray/vcluster-manager)](https://github.com/gmalfray/vcluster-manager/releases/latest)
[![Image](https://img.shields.io/badge/ghcr.io-vcluster--manager-blue?logo=docker)](https://github.com/gmalfray/vcluster-manager/pkgs/container/vcluster-manager)

Application web de gestion des vClusters Kubernetes. Interface centralisee pour creer, configurer et superviser les vclusters deployes via FluxCD.

## Fonctionnalites

- **Dashboard** : vue d'ensemble de tous les vclusters avec status temps reel via HTMX
- **Creation** : formulaire avec generation automatique des fichiers YAML, commit GitOps via GitLab API
- **Configuration** : modification des quotas CPU/mem/storage, activation/desactivation backup Velero (heure + retention), groupes RBAC ArgoCD, version K8s
- **Suppression** : suppression GitOps avec etat visuel "Suppression en cours" (persistant, polling K8s), option de supprimer le repo GitLab et les clients Keycloak
- **Status temps reel** : polling HTMX des HelmRelease et Flux Kustomization via client-go, consommation ResourceQuota (CPU/mem/stockage)
- **Versions** : affichage de la version du chart vcluster deploye, de la version K8s interne, et de la derniere release upstream (GitHub API)
- **Mise a jour du chart** : bouton pour mettre a jour le chart vcluster dans platform-helm-charts (commit preprod + MR vers master), avec detection des MR en attente
- **Version K8s globale** : modification de la version K8s par defaut du chart vcluster, avec liste des versions disponibles (commit preprod + MR vers master)
- **Version ArgoCD** : gestion globale de la version ArgoCD (template de base) et override par vcluster
- **ArgoCD** : creation automatique du repo app-manifests et des clients OIDC Keycloak
- **Migration d'apps ArgoCD** : deplacement d'applications ArgoCD d'un vcluster a un autre via l'interface
- **Vclusters prod editables** : les vclusters prod (deployes ou non) sont editables independamment de la preprod. Les modifications en prod deployee passent par MR automatique, les pending sont commitees directement
- **Rancher** : appairage/desappairage vcluster↔Rancher via port-forward, detection des appairages manuels, 6 etats visuels (Paired, Pairing, Unknown, ManuallyPaired, Cleaning, Off)
- **Protection namespace** : toggle par vcluster pour activer/desactiver l'annotation `protect-deletion: "true"` sur le namespace host. Protege contre les suppressions accidentelles (`kubectl delete ns`). Desactivee automatiquement avant une suppression GitOps. Persiste aux reconciliations FluxCD (Server-Side Apply).
- **Vault** : configuration automatique des backends Kubernetes auth par vcluster (AppRole ou token statique)
- **Backups Velero** : liste des backups par vcluster (filtre par namespace, tri par date), visualisation du contenu (DownloadRequest → presigned URL), restauration in-place ou vers un autre namespace (cross-vcluster), suivi de progression HTMX persistant au rechargement de page, declenchement de backup manuel (admin)
- **Retention Velero** : configurable globalement (page `/config`) et par vcluster (settings). Saisie en format court (`30j`, `12h`, `90m`), converti en duration Go (`720h0m0s`). Mode GitOps exclusivement : toute modification = commit dans `values.yaml` fluxprod.
- **Configuration Velero** (page `/config`, admin) : URL S3, buckets preprod/prod, TTL par defaut — modifiables a chaud (sans redemarrage), persistes et commites dans fluxprod.
- **CSRF** : protection double-submit cookie sur toutes les routes POST/PUT/DELETE (cookie `csrf_token` + header `X-CSRF-Token` injete automatiquement par HTMX)
- **Rate limiting** : 20 req/s, burst 50 par IP sur toutes les routes
- **Audit log** : trace des operations d'ecriture (creation, suppression, mise a jour, appairage Rancher, restauration Velero)
- **Metriques Prometheus** : endpoint `GET /metrics` (non authentifie, scraping par Prometheus)
- **Notifications webhook** : notification Slack/Mattermost/Rocket.Chat a la suppression d'un vcluster (debut et confirmation)

## Deploiement

### Prerequis

- Un cluster Kubernetes avec FluxCD
- Un namespace `vcluster-manager`
- Acces a un GitLab avec le repo fluxprod et les repos ArgoCD
- Un Keycloak pour l'authentification (OIDC + gestion des clients ArgoCD)

### 1. Creer le namespace

```bash
kubectl create namespace vcluster-manager
```

### 2. Configurer Keycloak

Deux clients Keycloak sont necessaires : un pour l'authentification des utilisateurs (OIDC) et un compte de service pour la gestion des clients ArgoCD.

#### Client OIDC (authentification utilisateurs)

Ce client permet aux utilisateurs de se connecter a l'application via Keycloak (SSO).

Dans l'admin Keycloak, realm cible :

1. **Clients > Create client**
   - Client ID : `vcluster-manager` (prod) ou `vcluster-manager-preprod` (preprod)
   - Client Protocol : `openid-connect`
2. **Settings**
   - Access Type : `confidential`
   - Valid Redirect URIs : `https://<URL_APP>/auth/callback`
   - Web Origins : `https://<URL_APP>`
3. **Credentials**
   - Copier le **Client Secret** (sera utilise comme `OIDC_CLIENT_SECRET`)
4. **Mappers > Create**
   - Mapper Type : `Group Membership`
   - Name : `groups`
   - Token Claim Name : `groups`
   - Full group path : `OFF`
   - Add to ID token : `ON`
   - Add to access token : `ON`

Les groupes definis dans `ADMIN_GROUPS` (defaut : `platform-admins,ops`) donnent le role admin dans l'application.

#### Compte de service Keycloak (gestion des clients ArgoCD)

Ce compte permet a l'application de creer/supprimer les clients OIDC ArgoCD via l'API Admin Keycloak, en utilisant le flow `client_credentials`.

Dans l'admin Keycloak, realm cible :

1. **Clients > Create client**
   - Client ID : `vcluster-manager-service` (ou un nom au choix)
   - Client Protocol : `openid-connect`
2. **Settings**
   - Access Type : `confidential`
   - Service Accounts Enabled : `ON`
   - Standard Flow Enabled : `OFF` (pas besoin de login interactif)
   - Direct Access Grants Enabled : `OFF`
3. **Credentials**
   - Copier le **Client Secret** (sera utilise comme `KEYCLOAK_CLIENT_SECRET`)
4. **Service Account Roles > Client Roles > realm-management**
   - Ajouter les roles :
     - `manage-clients` (creer/modifier/supprimer des clients)
     - `view-clients` (lister les clients existants)

### 3. Generer les secrets Kubernetes

Le script interactif genere les deux secrets necessaires :

```bash
# Generer et appliquer
./scripts/generate-secret.sh --env preprod | kubectl apply -f -

# Ou generer dans un fichier pour revue
./scripts/generate-secret.sh --env prod > secrets-prod.yaml
kubectl apply -f secrets-prod.yaml

# Verifier sans generer
./scripts/generate-secret.sh --env preprod --dry-run
```

Deux secrets sont crees :
- **`vcluster-manager-secrets`** : configuration (GitLab, Keycloak, OIDC)
- **`vcluster-manager-auth`** : authentification locale (mot de passe admin genere, JWT)

### 4. Configurer Vault KV (prerequis ArgoCD)

Pour les vclusters ArgoCD, vault-webhook injecte automatiquement les cles SSH dans les Secrets ArgoCD.
Les cles doivent etre stockees dans Vault KV avant la creation du premier vcluster ArgoCD, et la policy
`cert-manager` doit couvrir ces chemins :

```bash
# Cle SSH pour l'acces root ArgoCD (repos app-manifests-{name})
vault kv put <VAULT_KV_ARGOCD_ROOTAPPS> \
  sshPrivateKey="$(cat argocd_root_deploy_key)"

# Cle SSH pour les repos applicatifs declares dans les repos app-manifests
vault kv put <VAULT_KV_ARGOCD_REPO> \
  sshPrivateKey="$(cat argocd_repo_deploy_key)"
```

Ajouter dans la policy Vault de l'application :
```hcl
path "<VAULT_KV_ARGOCD_ROOTAPPS>" {
  capabilities = ["read"]
}
```

### 5. Deployer via FluxCD

Les manifests de deploiement sont dans `deploy/base/` avec des overlays dans `deploy/overlays/{preprod,prod}/`.

Les references FluxCD se trouvent dans le repo fluxprod sous `clusters/{env}/vcluster-manager/` :
- **preprod** : pointe vers la branche `preprod`
- **prod** : pointe vers la branche `master`

### 6. Operations courantes

Retrouver le mot de passe admin :

```bash
kubectl get secret vcluster-manager-auth -n vcluster-manager \
  -o jsonpath='{.data.ADMIN_PASSWORD}' | base64 -d
```

Regenerer uniquement le mot de passe admin (sans toucher a la config) :

```bash
kubectl create secret generic vcluster-manager-auth -n vcluster-manager \
  --from-literal=ADMIN_PASSWORD="$(openssl rand -hex 12)" \
  --from-literal=JWT_SECRET="$(openssl rand -base64 32)" \
  --dry-run=client -o yaml | kubectl apply -f -
```

## Configuration

### Variables d'environnement

| Variable | Requis | Description | Defaut |
|----------|--------|-------------|--------|
| `LISTEN_ADDR` | | Adresse d'ecoute | `:8080` |
| `DATA_DIR` | | Repertoire de donnees persistantes (backend `file`) | `data` |
| `TEMPLATE_DIR` | | Chemin des templates HTML | `web/templates` |
| `STATE_BACKEND` | | Backend de persistence : `file` ou `configmap` | `file` |
| `K8S_NAMESPACE` | | Namespace K8s pour le backend `configmap` | auto-detecte |
| **GitLab** | | | |
| `GITLAB_URL` | | URL de l'instance GitLab | `https://gitlab.example.com` |
| `GITLAB_TOKEN` | **oui** | Token GitLab (scope `api`) — utiliser un **Group Access Token** avec le rôle Maintainer sur le groupe contenant fluxprod, platform-helm-charts et le groupe ArgoCD | - |
| `GITLAB_PROJECT_ID` | | ID du projet fluxprod | - |
| `GITLAB_ARGOCD_GROUP_ID` | | ID du groupe pour les repos ArgoCD | - |
| `GITLAB_HELM_PROJECT_ID` | | ID du projet platform-helm-charts | - |
| `GITLAB_SSH_URL` | | URL SSH GitLab | `ssh://git@gitlab.example.com:22` |
| `GITLAB_ARGOCD_PATH` | | Namespace GitLab des repos ArgoCD | `ops/argocd` |
| `FLUXCD_DEPLOY_KEY_ID` | | ID de la deploy key FluxCD dans GitLab | `0` |
| **Keycloak (service account)** | | | |
| `KEYCLOAK_URL` | | URL Keycloak | - |
| `KEYCLOAK_REALM` | | Realm Keycloak | `master` |
| `KEYCLOAK_CLIENT_ID` | | Client ID du compte de service | - |
| `KEYCLOAK_CLIENT_SECRET` | | Client Secret du compte de service | - |
| **OIDC (auth utilisateurs)** | | | |
| `OIDC_CLIENT_ID` | | Client ID OIDC | - |
| `OIDC_CLIENT_SECRET` | | Client Secret OIDC | - |
| `OIDC_REDIRECT_URL` | | URL de callback OIDC | - |
| **Auth locale** | | | |
| `ADMIN_PASSWORD` | | Mot de passe du compte local admin | - |
| `JWT_SECRET` | | Secret pour signer les tokens JWT | - |
| `ADMIN_GROUPS` | | Groupes OIDC avec acces admin (virgule-separes) | `platform-admins,ops` |
| **Kubernetes** | | | |
| `KUBECONFIG_PREPROD` | | Chemin kubeconfig cluster preprod | - |
| `KUBECONFIG_PROD` | | Chemin kubeconfig cluster prod | - |
| `SSH_TUNNEL_PREPROD` | | Tunnel SSH preprod (`user@bastion:22`) | - |
| `SSH_TUNNEL_PROD` | | Tunnel SSH prod | - |
| `SSH_KEY_PATH` | | Cle SSH pour les tunnels | `/root/.ssh/id_rsa` |
| `CLUSTER_LABEL_PREPROD` | | Nom d'affichage cluster preprod | `Preprod` |
| `CLUSTER_LABEL_PROD` | | Nom d'affichage cluster prod | `Prod` |
| **Domaines et TLS** | | | |
| `BASE_DOMAIN_PREPROD` | | Domaine de base preprod | `preprod.example.com` |
| `BASE_DOMAIN_PROD` | | Domaine de base prod | `example.com` |
| `TLS_SECRET_PREPROD` | | Secret TLS wildcard ingress preprod | `wildcard-preprod-example-com-tls` |
| `TLS_SECRET_PROD` | | Secret TLS wildcard ingress prod | `wildcard-example-com-tls` |
| **vCluster defaults** | | | |
| `DEFAULT_CPU` | | Quota CPU par defaut | `8` |
| `DEFAULT_MEMORY` | | Quota memoire par defaut | `32Gi` |
| `DEFAULT_STORAGE` | | Quota stockage par defaut | `500Gi` |
| `DEFAULT_RBAC_GROUP` | | Groupe RBAC OIDC par defaut pour les vclusters | `developers` |
| `VELERO_TIMEZONE` | | Timezone pour les cron Velero | `Europe/Paris` |
| `VELERO_DEFAULT_TTL` | | Retention par defaut des backups Velero (format Go duration) | `720h0m0s` |
| `VELERO_NAMESPACE` | | Namespace ou Velero est installe | `velero-system` |
| `VELERO_S3_URL` | | URL du serveur S3 pour les backups Velero | - |
| `VELERO_BUCKET_PREPROD` | | Bucket S3 pour les backups Velero preprod | - |
| `VELERO_BUCKET_PROD` | | Bucket S3 pour les backups Velero prod | - |
| `VCLUSTER_POD_SECURITY` | | Pod security standard (`privileged`, `baseline`, `restricted`) | `privileged` |
| `ARGOCD_DEFAULT_POLICY` | | Politique RBAC ArgoCD par defaut | `role:readonly` |
| **GitOps repo** | | | |
| `FLUXPROD_CLUSTERS_PATH` | | Dossier racine des clusters dans le repo GitOps | `clusters` |
| `FLUXPROD_ARGOCD_KUST_PATH` | | Chemin du kustomization ArgoCD global | `lib/tenant-template/argocd/base/kustomization.yaml` |
| `HELM_CHARTS_VCLUSTER_PATH` | | Chemin du chart vcluster dans platform-helm-charts | `charts/vcluster` |
| **Vault KV** (optionnel) | | | |
| `VAULT_KV_ARGOCD_ROOTAPPS` | | Chemin Vault KV credentials ArgoCD root apps | `secret/data/vcluster/argocd/rootapps` |
| `VAULT_KV_ARGOCD_REPO` | | Chemin Vault KV credentials repo ArgoCD | `secret/data/vcluster/argocd/repo` |
| **Rancher** (optionnel) | | | |
| `RANCHER_URL` | | URL de l'instance Rancher | - |
| `RANCHER_TOKEN` | | Bearer token API Rancher | - |
| `RANCHER_ENABLED_PREPROD` | | Activer l'appairage Rancher en preprod | `false` |
| `RANCHER_ENABLED_PROD` | | Activer l'appairage Rancher en prod | `false` |
| **Vault** (optionnel) | | | |
| `VAULT_ADDR` | | URL de l'instance Vault | - |
| `VAULT_ROLE_ID` | | AppRole role_id (prefere a VAULT_TOKEN) | - |
| `VAULT_SECRET_ID` | | AppRole secret_id | - |
| `VAULT_TOKEN` | | Token statique (fallback, deconseille) | - |
| **Notifications** (optionnel) | | | |
| `WEBHOOK_URL` | | URL webhook Slack/Mattermost/Rocket.Chat | - |

> **Note :** `WEBHOOK_URL` contient un token dans l'URL — utiliser le Secret Kubernetes, pas le ConfigMap.

Les kubeconfigs, tunnels SSH, labels de clusters et configuration Velero (TTL, S3 URL, buckets) sont aussi configurables depuis l'interface web (page `/config`, admin uniquement). Ces valeurs sont initialisees depuis les variables d'env au demarrage, puis modifiables a chaud et persistees dans le state backend (`DATA_DIR/settings.json` ou ConfigMap). Les modifications Velero (buckets, S3 URL) declenchent aussi un commit dans fluxprod.

## Roles et autorisations

| Role | Acces |
|------|-------|
| **admin** | Lecture + ecriture (creation, modification, suppression de vclusters) |
| **lecteur** | Consultation uniquement (dashboard, liste, detail, status) |

Le role admin est attribue aux utilisateurs appartenant aux groupes OIDC definis dans `ADMIN_GROUPS` (defaut : `platform-admins,ops`). Le compte local `admin` est toujours administrateur.

## Securite

| Mecanisme | Detail |
|-----------|--------|
| **CSRF** | Double-submit cookie (`csrf_token` SameSite=Strict + header `X-CSRF-Token`). Le hook HTMX `htmx:configRequest` injecte automatiquement le header sur chaque requete. |
| **Rate limiting** | 20 req/s, burst 50 par IP. Applique avant l'authentification. |
| **Auth OIDC** | Keycloak (ou tout provider OIDC), groupes admin configurables via `ADMIN_GROUPS`. |
| **Auth locale** | Mot de passe + JWT (fallback dev). |
| **Secrets** | Tokens GitLab/Rancher/Vault dans `vcluster-manager-secrets`. `WEBHOOK_URL` dans le meme secret (URL avec token). |

## Developpement

### Build

```bash
go build -o vcluster-manager ./cmd/server
```

### Tests

```bash
go test ./...
```

Les tests couvrent : generateur GitOps (`generator_test.go`, 25 tests), parser (`parser_test.go`, 17 tests), handlers (`handlers_test.go`, 17 tests), CSRF (`csrf_test.go`, 12 tests).

### Docker

```bash
docker build -t vcluster-manager .
docker run -e GITLAB_TOKEN=glpat-xxx -e GITLAB_PROJECT_ID=123 vcluster-manager
```

## Structure

```
cmd/server/main.go            # Point d'entree, routing, init clients
internal/
  config/                     # Env vars + settings persistants (data/settings.json, data/deleting.json)
  auth/
    oidc.go                   # Middleware Keycloak OIDC + auth locale JWT
    csrf.go                   # Protection CSRF double-submit cookie
    rate_limiter.go           # Rate limiting par IP (token bucket)
  audit/log.go                # Audit log structure (qui/action/quoi/quand)
  argocd/updater.go           # Mise a jour version ArgoCD globale dans fluxprod
  github/releases.go          # Client GitHub API (releases vcluster, ArgoCD, versions K8s)
  gitops/
    parser.go                 # Lecture des vclusters depuis fluxprod (parallelise, interface fileProvider)
    generator.go              # Generation des fichiers YAML GitOps
    templates/                # Templates YAML embarques (go:embed)
    gitlab.go                 # Client GitLab API (cache TTL 30s)
  helmcharts/updater.go       # Mise a jour chart vcluster (platform-helm-charts)
  keycloak/client.go          # Client Keycloak Admin API (client_credentials, token cache)
  kubernetes/status.go        # client-go : HelmRelease/Kustomization + versions + quotas + Rancher agents
  metrics/                    # Middleware Prometheus + handler /metrics
  models/vcluster.go          # Structs Go (VCluster, CreateRequest, UpdateRequest, StatusInfo...)
  notify/webhook.go           # Notifier webhook generique (Slack/Mattermost/RC)
  rancher/client.go           # Client Rancher API v3 (import/delete cluster, registration tokens)
  vault/client.go             # Client Vault (AppRole auth, Kubernetes auth backends)
  version/version.go          # Version lue depuis VERSION (go:embed)
  handlers/                   # Handlers HTTP (dashboard, CRUD, API, config)
web/
  templates/                  # Templates HTML (layout, pages, partials HTMX)
  static/app.css              # Styles custom
deploy/
  base/                       # Manifests Kubernetes (Deployment, Service, PVC, RBAC, ConfigMap)
  overlays/{preprod,prod}/    # Kustomize overlays par environnement
scripts/
  generate-secret.sh          # Generateur interactif de secrets Kubernetes
VERSION                       # Version courante de l'application (ex: 1.1.0)
```

## Templates de génération GitOps

Lors de la création d'un vcluster, l'application génère et commit entre 9 et 17 fichiers YAML dans fluxprod (`clusters/{env}/vclusters/{name}/`). Ces fichiers sont produits à partir de **templates editables** situés dans :

```
internal/gitops/templates/
  kustomization.yaml.tmpl              # kustomization racine du vcluster
  values.yaml.tmpl                     # values.yaml (quotas, Velero, K8s version...)
  tenant_flux.yaml.tmpl               # Flux Kustomization pour le répertoire tenant/
  tenant/
    kustomization.yaml.tmpl            # kustomization du répertoire tenant/
    cert-manager_kustomization.yaml.tmpl
    cert-manager-config_kustomization.yaml.tmpl
    vault-webhook_kustomization.yaml.tmpl
    cert-manager/kustomization.yaml.tmpl
    vault-webhook/kustomization.yaml.tmpl
    argocd_kustomization.yaml.tmpl     # (uniquement si ArgoCD activé)
    argocd/
      kustomization.yaml.tmpl          # overlay kustomize ArgoCD
      argo-cd-cm.yaml.tmpl             # ConfigMap OIDC ArgoCD
      argocd-rbac-cm.yaml.tmpl         # ConfigMap RBAC ArgoCD
    navlink_kustomization.yaml.tmpl    # (uniquement si ArgoCD activé)
    navlink/kustomization.yaml.tmpl
    flux-bootstrap_kustomization.yaml.tmpl  # (uniquement si FluxCD activé)
    flux-bootstrap/kustomization.yaml.tmpl
```

Les templates utilisent la syntaxe Go [`text/template`](https://pkg.go.dev/text/template). Ils sont embarqués dans le binaire via `//go:embed` à la compilation.

### Variables disponibles dans les templates

Les variables sont calculées par `buildData()` dans `generator.go` à partir des données du formulaire et de la configuration (`GeneratorConfig`). Elles se divisent en deux catégories :

**Variables dynamiques** — calculées à partir du nom et de l'environnement du vcluster :

| Variable | Description | Exemple |
|----------|-------------|---------|
| `{{.Name}}` | Nom du vcluster | `demos` |
| `{{.Env}}` | Environnement | `preprod` ou `prod` |
| `{{.APIHost}}` | Hostname API (depuis `BASE_DOMAIN_*`) | `demos.api.preprod.example.com` |
| `{{.Domain}}` | Domaine wildcard (depuis `BASE_DOMAIN_*`) | `demos.preprod.example.com` |
| `{{.WildcardSecret}}` | Secret TLS cert-manager (dérivé du domaine) | `wildcard-demos-preprod-example-com-tls` |
| `{{.ArgoCDClientID}}` | Client ID Keycloak OIDC ArgoCD | `argocd-k8s-demos-preprod` |
| `{{.ArgoCDURL}}` | URL ArgoCD avec slash final | `https://argocd.demos.preprod.example.com/` |
| `{{.ArgoCDHost}}` | Hostname ArgoCD | `argocd.demos.preprod.example.com` |
| `{{.TargetRevision}}` | Branche app-manifests | `preprod` ou `master` |
| `{{.EnvLabel}}` | Label d'affichage de l'env | `preprod` ou `prod` |
| `{{.TLSSecret}}` | Secret TLS ingress ArgoCD (depuis `TLS_SECRET_*`) | `wildcard-preprod-example-com-tls` |
| `{{.PolicyLines}}` | Lignes RBAC pré-formatées (depuis les groupes du formulaire) | `    g, ops, role:admin\n` |

**Variables de configuration** — viennent directement des variables d'environnement :

| Variable | Variable d'env source | Description | Exemple |
|----------|-----------------------|-------------|---------|
| `{{.OIDCIssuer}}` | `KEYCLOAK_URL` + `KEYCLOAK_REALM` | Issuer OIDC pour ArgoCD ConfigMap | `https://keycloak.example.com/auth/realms/myrealm` |
| `{{.GitLabSSHBase}}` | `GITLAB_SSH_URL` + `GITLAB_ARGOCD_PATH` | Base SSH pour les URLs app-manifests | `ssh://git@gitlab.example.com:22226/ops/argocd` |
| `{{.DefaultPolicy}}` | `ARGOCD_DEFAULT_POLICY` | Politique RBAC ArgoCD par défaut | `role:readonly` |
| `{{.PodSecurity}}` | `VCLUSTER_POD_SECURITY` | Pod security standard vcluster | `privileged` |

**Variables de formulaire** — saisies par l'utilisateur à la création/modification :

| Variable | Description | Exemple |
|----------|-------------|---------|
| `{{.VeleroEnabled}}` | Backup Velero activé | `true` / `false` |
| `{{.VeleroSchedule}}` | Expression cron Velero (timezone depuis `VELERO_TIMEZONE`) | `CRON_TZ=Europe/Paris 0 2 * * *` |
| `{{.VeleroTTL}}` | Retention du backup (depuis `velero_ttl` ou `VELERO_DEFAULT_TTL`) | `720h0m0s` |
| `{{.NoQuotas}}` | Pas de ResourceQuota | `true` / `false` |
| `{{.CPU}}` | Quota CPU (défaut : `DEFAULT_CPU`) | `8` |
| `{{.Memory}}` | Quota mémoire (défaut : `DEFAULT_MEMORY`) | `32Gi` |
| `{{.Storage}}` | Quota stockage (défaut : `DEFAULT_STORAGE`) | `500Gi` |
| `{{.K8sVersion}}` | Version K8s forcée | `v1.32.1` (vide = chart par défaut) |
| `{{.ArgoCD}}` | ArgoCD activé | `true` / `false` |
| `{{.ArgoCDVersion}}` | Override version ArgoCD | `v2.13.0` (vide = version globale) |
| `{{.FluxCD}}` | FluxCD activé | `true` / `false` |
| `{{.FluxCDRepoURL}}` | URL repo FluxCD | `ssh://git@...` |
| `{{.FluxCDBranch}}` | Branche FluxCD | `master` |
| `{{.FluxCDPath}}` | Chemin FluxCD dans le repo | `clusters/pra2` |

### Modifier un template existant

1. Editer le fichier `.yaml.tmpl` concerné dans `internal/gitops/templates/`
2. Recompiler : `go build ./...`
3. Tester avec `go vet ./...`
4. La modification s'applique à tous les vclusters créés ou mis à jour ensuite

> Les vclusters existants dans fluxprod **ne sont pas modifiés automatiquement** : seules les nouvelles créations ou mises à jour (settings) utiliseront le nouveau template.

### Ajouter une nouvelle variable configurable dans les templates

Si la valeur doit venir d'une variable d'environnement :

1. Ajouter le champ dans `Config` (`internal/config/config.go`) avec `getEnv("MA_VAR", "défaut")`
2. Ajouter le champ dans `GeneratorConfig` (`internal/gitops/generator.go`)
3. Passer la valeur dans `handlers.go` où `NewGenerator(gitops.GeneratorConfig{...})` est appelé
4. Ajouter le champ dans `TemplateData` et l'assigner dans `buildData()`
5. Utiliser `{{.MaVariable}}` dans le template `.yaml.tmpl`
6. Documenter la variable dans le tableau ci-dessus et dans le tableau des variables d'environnement

### Ajouter un nouveau fichier généré

1. Créer le fichier `.yaml.tmpl` dans `internal/gitops/templates/` (sous-dossier si besoin)
2. Dans `generator.go` :
   - Ajouter le fichier dans la liste `files` de `GenerateVCluster()`
   - Si c'est un fichier tenant, l'ajouter dans `tenant/kustomization.yaml.tmpl`
3. Synchroniser avec les vclusters existants dans fluxprod si nécessaire (voir [`AGENTS.md`](AGENTS.md) §3 et [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md#generator))

## CI/CD

Build et push de l'image Docker :

```bash
docker build -t ghcr.io/gmalfray/vcluster-manager:latest .
docker push ghcr.io/gmalfray/vcluster-manager:latest
```

Branche `master` → tag `latest`.

## Branches

- `master` : production
- `preprod` : pre-production
