# CRD `VCluster` — conception

> Document de conception pour l'étape 2 (opérateur). Applique le modèle C allégé tranché dans
> `docs/adr-001-source-de-verite.md` — je ne redébats pas la décision, je la détaille jusqu'au
> niveau où elle devient implémentable. Pas de code de contrôleur ici : le schéma
> (`api/v1alpha1/vcluster_types.go`), des exemples (`config/samples/`), et une ébauche de RGD kro
> (`config/kro/`).

## 1. Portée et hypothèse de départ

Le CR `VCluster` remplace l'arborescence de 8 à 17 fichiers générée aujourd'hui par
`internal/gitops/generator.go`. Il est commité dans fluxprod, appliqué par Flux, expansé côté
cluster par kro (pour la partie purement Kubernetes) et un contrôleur Go (pour le reste).

Hypothèse que je pose explicitement, parce que le reste du découpage en dépend : **chaque
environnement (preprod, prod) est un cluster hôte distinct, avec sa propre instance
d'opérateur**, au même titre que le générateur actuel distingue déjà `BaseDomainPreprod` et
`BaseDomainProd`. Si ce n'est pas le cas — si un seul opérateur doit un jour reconcilier des CR
appliqués sur plusieurs clusters à distance — le champ `env` devra revenir dans `spec` (voir
§2.1). À vérifier avant d'écrire le contrôleur.

## 2. Le schéma

### 2.1 Pourquoi `env` n'est pas dans `spec`

Le nom du vcluster est `metadata.name`, pas un champ `spec.name` : le CR *est* la ressource, pas
une requête de création. Pour la même raison, l'environnement n'est pas un champ `spec.env` : il
est déterminé par l'endroit où le CR est appliqué (`clusters/{env}/vclusters/{name}/vcluster.yaml`
dans fluxprod, réconcilié par l'instance d'opérateur qui tourne sur le cluster `{env}`). Ajouter
un champ `env` créerait une deuxième source de vérité pour la même information, avec un risque de
divergence entre le chemin Git et le champ — exactement le genre de duplication que le modèle C
essaie d'éliminer.

### 2.2 `spec` — table des champs

| Champ | Type | Justification |
|---|---|---|
| `type` | enum `vcluster \| capi` | ADR-001, suite point 1 : discriminant dès le premier schéma. `vcluster` seul actif ; `capi` réservé, refusé par la validation (voir §2.3). |
| `owner` | string, requis | **Champ nouveau, sans équivalent dans le code actuel.** Vient directement de la table « Contrôler la création — par la règle » de l'ADR : sans propriétaire, ni le budget de ressources ni la revue de branche protégée n'ont quelqu'un à qui parler. |
| `deletionProtection` | bool | ADR-001, suppression par la réversibilité point 1 : couper la protection est un commit séparé de celui qui supprime réellement le CR. |
| `argoCD.enabled` | bool | `CreateRequest.ArgoCD` / `VCluster.ArgoCD` |
| `argoCD.version` | string, optionnel | `CreateRequest.ArgoCDVersion` / `VCluster.ArgoCDVersion` — override par vcluster de la version globale |
| `argoCD.rbacGroups` | []string | `CreateRequest.RBACGroups` / `VCluster.RBACGroups` — groupes OIDC → `policy.csv` ArgoCD (`PolicyLines` dans le générateur) |
| `velero.enabled` | bool | `CreateRequest.VeleroEnabled` / `VeleroConfig.Enabled` |
| `velero.hour` | string `"HH:MM"` | `CreateRequest.VeleroHour` — converti en cron par le contrôleur (aujourd'hui `parseVeleroHour` + `VeleroSchedule` dans le générateur) |
| `velero.ttl` | string `"30j"` / `"12h"` / `"90m"` | Forme courte déjà acceptée par l'UI (`parseTTLText`/`ttlToText` dans `handlers.go`). **Choix délibéré** : aujourd'hui la forme *brute* Velero (`"720h0m0s"`) est ce qui atterrit dans `values.yaml` committé — donc dans le diff que la revue de branche protégée doit lire. Un CR lu par un humain doit porter la forme courte ; c'est le contrôleur qui fait la conversion vers Velero, pas l'inverse. |
| `quotas.enabled` | bool | Négation de `NoQuotas` — reformulé en positif pour que l'absence du bloc n'ait pas besoin d'un bool inversé pour rester sûre par défaut |
| `quotas.cpu/memory/storage` | string (quantité K8s) | `CreateRequest.CPU/Memory/Storage` / `QuotaConfig` |
| `k8sVersion` | string, optionnel | `UpdateRequest.K8sVersion` / `VCluster.K8sVersionConfig` — `controlPlane.distro.k8s.version` |
| `fluxCD.repoURL/branch/path` | string | `CreateRequest.FluxCDRepoURL/Branch/Path` / `FluxCDConfig` — bootstrap Flux imbriqué |

Champs volontairement absents parce que sans usage actuel identifiable : rien d'autre. Les champs
précalculés du générateur (`APIHost`, `Domain`, `WildcardSecret`, `ArgoCDClientID`, `ArgoCDURL`,
`TargetRevision`, `TLSSecret`, `PodSecurity`...) ne sont **pas** dans `spec` : ce sont des dérivés
de `name` + `env` + config globale de l'opérateur (domaines de base, politique ArgoCD par défaut),
recalculés à chaque reconcile plutôt que gelés dans le CR. Les mettre dans `spec` figerait dans
Git une valeur que seul l'opérateur doit produire, et romprait la promotion preprod→prod (qui
change `TargetRevision` et les domaines sans que rien dans le CR n'ait besoin de changer).

### 2.3 `type: capi` — ce qui est actif, ce qui est réservé

Actif aujourd'hui : uniquement `type: vcluster`. `type: capi` existe dans l'énumération Go
(`VClusterTypeCAPI`) pour que l'ajout futur de Cluster API (`docs/etude-cluster-api.md`) ne soit
pas un changement de schéma cassant. Tant que CAPI n'est pas implémenté, un CR avec
`type: capi` doit être rejeté par la validation — je ne l'ai pas codé ici (pas de webhook, pas de
CRD OpenAPI à ce stade), mais c'est un prérequis avant que quiconque ne pense pouvoir l'utiliser.
Aucun champ `spec.capi` n'existe encore : le jour où CAPI arrive, il prendra la forme d'un bloc
`spec.capi` optionnel de plus, pas d'une refonte de `VClusterSpec`.

### 2.4 `status` — vue d'ensemble

| Champ | Rôle |
|---|---|
| `observedGeneration` | Fraîcheur du status par rapport au dernier `spec` appliqué |
| `phase` | Résumé lisible : `Pending / Provisioning / Ready / Degraded / Suspended / Deleting / Failed` |
| `conditions` | Détail typé (voir §3.3) |
| `chartVersion`, `k8sVersion` | Repris tels quels de `StatusInfo` (version du chart HelmRelease, version K8s interne au vcluster) |
| `podCount`, `resourceUsage.*` | Repris de `StatusInfo` (usage quotas CPU/mem/storage + pourcentages) |
| `protectionEnabled` | État actuel de l'annotation `protect-deletion` sur le namespace hôte |
| `rancher.state`, `rancher.paired` | Les états déjà affichés par `rancher_status.html` (Paired/Pairing/Unknown/ManuallyPaired/Cleaning/Off) |
| `vault.status`, `vault.message` | Reprend `VaultSetupState` (waiting/configuring/done/error) tel quel |
| `lastBackup.*` | Dernière sauvegarde Velero connue (nom, phase, date de complétion) |
| `deletion.*` | État de la séquence de suppression — remplace `data/deleting.json` et les entrées `ListCleaning()`, voir §4.4 |

## 3. Le découpage kro / Go

C'est la frontière qui décide si le projet tient debout, donc je la donne explicite, pas
optimiste.

### 3.1 Ce que kro peut produire (pur Kubernetes, template CEL depuis le CR)

- `HelmRelease` du chart vcluster (namespace `vcluster-{name}`), avec les valeurs dérivées de
  `spec.quotas`, `spec.k8sVersion`, `spec.velero.*`
- `ResourceQuota` (quand `spec.quotas.enabled`)
- Les `Kustomization` tenant qui sont de la pure référence à des manifests statiques :
  `tenant/cert-manager`, `tenant/cert-manager-config`, `tenant/vault-webhook`
- `Kustomization` + `kustomization.yaml` ArgoCD (`tenant/argocd`, avec l'override d'image si
  `spec.argoCD.version` est renseigné) et les deux `ConfigMap` qui en dépendent
  (`argo-cd-cm.yaml`, `argocd-rbac-cm.yaml` — la policy RBAC générée depuis
  `spec.argoCD.rbacGroups`)
- `Kustomization` `tenant/navlink` (lien de navigation, actif seulement si ArgoCD)
- `Kustomization` `tenant/flux-bootstrap` (si `spec.fluxCD` est renseigné) : `GitRepository` +
  `Kustomization` pointant vers `spec.fluxCD.repoURL/branch/path`

Tout ça, c'est du gabarit : nom de ressource, labels, valeurs Helm — aucun appel réseau vers un
système externe à Kubernetes.

### 3.2 Ce qui reste en Go (contrôleur, appelant `internal/service`)

- **Écriture du CR lui-même dans Git.** Attention à la boucle : c'est `service.Create` qui
  sérialise le CR et le commite dans fluxprod via le client GitLab — ni kro ni le contrôleur
  n'écrivent dans Git au moment de la création. `generator.go` ne produit plus 8 à 17 fichiers,
  il produit un objet `VCluster` unique, marshalé en YAML.
- **Keycloak** : création/suppression des clients OIDC ArgoCD (`keycloak.CreateArgoCDClients` /
  `DeleteArgoCDClients`) — API HTTP externe, aucune ressource Kubernetes.
- **GitLab** : création/suppression du repo `app-manifests` (`gitlab.CreateAppManifestsRepo` /
  `DeleteProject`) — API HTTP externe.
- **Rancher** : import (`ImportCluster`), suppression (`DeleteCluster`), et le déploiement +
  attente du job `rancher-cleanup` *à l'intérieur* du vcluster via port-forward
  (`ApplyManifestToVClusterViaPortForward` + `WaitForJobComplete`). Le déploiement du Job est bien
  une ressource Kubernetes, mais son déclenchement est séquencé avec un appel API Rancher externe
  et une attente active — kro ne fait pas de séquencement conditionnel, juste de l'expansion de
  graphe.
- **Vault** : configuration du backend d'authentification Kubernetes
  (`vault.SetupVClusterAuth`), qui attend que `vault-webhook` soit prêt puis génère un token
  reviewer *depuis l'intérieur* du vcluster (`CreateVaultReviewerToken`, port-forward) — API Vault
  externe + lecture intra-vcluster.
- **Lecture de statut intra-vcluster** : version K8s réelle (`getK8sVersion`, discovery API à
  travers le port-forward), liste des apps ArgoCD (`ListVClusterArgoApps`). Toujours via les
  helpers `withVCluster*` de `internal/kubernetes/status.go`, jamais de kubeconfig brut — cette
  règle ne change pas avec l'opérateur.
- **Budget de ressources par cluster hôte** : calcul et décision d'admission ou de refus (§5).
- **Séquence de suppression** : dépairage Rancher, déclenchement + attente de la sauvegarde
  Velero, retrait de la protection namespace, destruction — c'est un enchaînement d'appels
  externes avec des conditions d'attente, pas un graphe statique (§4.4).
- **Le toggle de protection namespace** (`SetNamespaceProtection`) reste en Go parce qu'il est
  imbriqué dans la séquence de suppression au bon moment, même si le geste lui-même n'est qu'un
  patch d'annotation.

### 3.3 Conditions typées proposées

`Ready`, `ResourcesProvisioned` (le graphe kro est appliqué et sain), `BudgetOK` /
`BudgetExceeded`, `ArgoCDReady` (Keycloak + GitLab + Kustomization ArgoCD tous ok, si
`spec.argoCD.enabled`), `VaultConfigured`, `RancherPaired`, `DeletionProtected` (miroir informatif
de `spec.deletionProtection`, pour voir l'état sans lire le spec), `BackupCompleted` (pendant une
suppression). `Ready` est l'agrégat que Flux peut utiliser comme health check de Kustomization —
c'est ce qui permet à un échec de rester visible dans `flux get kustomizations` sans avoir besoin
d'un webhook d'admission (§5).

Events proposés (au sens Kubernetes `Event`, pas juste des logs) : `BudgetExceeded`,
`DeletionProtectionBlocked` (tentative de suppression alors que `deletionProtection` est toujours
`true`), `GracePeriodEntered`, `GracePeriodCancelled`, `BackupFailed`, `RancherUnpairFailed`. Ce
sont les points où un échec silencieux serait le plus coûteux (ADR-001, risques : « l'opérateur
devient un point de passage unique »).

## 4. Cycle de vie

### 4.1 Reconcile normal

1. Lire le CR, valider `spec.type == "vcluster"` (sinon condition `Failed`, pas de destin pour
   `capi` aujourd'hui).
2. Vérifier le budget de ressources (§5) — si dépassé, poser `BudgetExceeded`, phase `Failed`,
   ne rien provisionner, `Requeue` périodique (le budget peut se libérer si un autre vcluster est
   supprimé entretemps).
3. Appliquer/mettre à jour l'instance kro (le graphe §3.1).
4. Lancer/vérifier les étapes Go (§3.2) : Keycloak, GitLab repo, Vault — chacune indépendante,
   chacune sa condition.
5. Lire le statut intra-vcluster (version K8s, apps ArgoCD) et le statut Rancher, écrire
   `status`.
6. Agréger en `phase` et condition `Ready`.

Idempotent par construction : chaque étape compare l'état désiré (dérivé du CR) à l'état observé
avant d'agir, comme le fait déjà `service.Create` aujourd'hui pour les warnings non bloquants
(GitLab/Keycloak/Vault peuvent échouer sans empêcher la déclaration du vcluster).

### 4.2 Grace period (« corbeille ») — pourquoi ce n'est pas `deletionTimestamp`

Point important que je détaille parce que l'ADR le mentionne sans trancher le mécanisme : une
fois que Kubernetes a posé `metadata.deletionTimestamp` sur un objet, cette décision est
**irréversible** — impossible de l'annuler, même avec un finalizer qui bloque encore l'objet. Le
« délai de grâce, annulable d'ici là » de l'ADR ne peut donc pas être modélisé avec
`deletionTimestamp` + finalizer seul : il doit se passer *avant*.

Séquence proposée, en trois gestes Git distincts :

1. **Commit 1** — `spec.deletionProtection: false`. Le CR existe toujours, rien ne bouge encore.
2. **Commit 2** — le CR passe en intention de suppression (`spec.suspend: true`, ou un champ
   équivalent — à trancher avec le contrôleur, pas dans ce document de schéma). Le contrôleur
   réagit : suspend Flux pour les ressources tenant (`SetFluxSuspend`), scale le vcluster à zéro
   réplica (`ScaleVClusterStatefulSet`), pose `status.deletion.gracePeriodEndsAt = now + N`,
   phase `Suspended`. **Réversible** : un `git revert` de ce commit repasse `spec.suspend` à
   `false`, le contrôleur resuspend Flux dans l'autre sens et remonte les réplicas — rien n'a été
   perdu puisque rien n'a encore été détruit.
3. **Commit 3**, après expiration du délai — suppression du fichier CR dans fluxprod. C'est *ce*
   commit qui déclenche le `deletionTimestamp` réel via le prune Flux, et donc le finalizer
   (§4.4). Le geste qui écrit ce commit peut être le même service Go qui commite déjà les
   suppressions aujourd'hui (`performDeletion`), déclenché par le contrôleur une fois
   `gracePeriodEndsAt` dépassé sans annulation — cohérent avec la règle « l'opérateur commit dans
   fluxprod ou crée des ressources Flux ».

Ce découpage explique pourquoi l'ADR range la « réversibilité » (points 1 et 3 de sa liste) et le
« finalizer qui orchestre » (point 4) dans deux paragraphes séparés : ce sont deux mécanismes
différents, l'un basé sur `spec` (réversible), l'autre sur `deletionTimestamp` (ne l'est plus).

### 4.3 Contrôle de garde : `deletionProtection` encore actif à la suppression réelle

Si le commit 3 arrive alors que `spec.deletionProtection` est resté `true` (quelqu'un a sauté le
commit 1, ou l'a réappliqué entretemps), le finalizer doit refuser de continuer : poser
`DeletionProtectionBlocked`, rester en `Deleting` sans avancer au-delà, et attendre soit un commit
qui recrée le CR (annule la suppression côté Git — possible tant que le finalizer n'a pas encore
libéré l'objet), soit un commit explicite qui lève la protection pendant que l'objet est déjà en
`Terminating`. Ce garde-fou est ce qui rend le retrait de la MR acceptable : la protection est
vérifiée au moment le plus tardif possible, pas seulement à la lecture du diff.

### 4.4 Le finalizer — séquence et survie au redémarrage

Une fois `deletionTimestamp` posé (commit 3, ou suppression manuelle malgré la protection —
bloquée par §4.3), le finalizer exécute, dans l'ordre :

1. **`RancherUnpairing`** — si `status.rancher.paired`, dépairage (`rancher.DeleteCluster`), puis
   déploiement + attente du job `rancher-cleanup` *dans* le vcluster (c'est pour ça que cette
   étape doit précéder la destruction : le cleanup tourne à l'intérieur du vcluster qu'on est en
   train de supprimer).
2. **`BackupPending`** — déclenchement d'une sauvegarde Velero et attente de la phase
   `Completed`. Si Velero n'est pas configuré ou que la sauvegarde échoue,
   `spec` doit porter un champ d'override explicite (pas encore dans le schéma ci-dessus — à
   ajouter au moment du contrôleur, par exemple `status.deletion.backupOverride`, déjà présent
   dans le type Go, actionné par un geste humain distinct) avant que le finalizer n'accepte de
   passer à l'étape suivante.
3. **`ProtectionRemoval`** — si `status.protectionEnabled`, retrait de l'annotation
   `protect-deletion`.
4. **`Destroying`** — retrait des finalizers K8s internes si nécessaire, laisser Kubernetes
   effectivement supprimer l'objet (les ressources kro et les leurs, via leurs
   `ownerReferences`, disparaissent en cascade).

**Survie au redémarrage** : chaque étape écrit `status.deletion.stage` avant de passer à la
suivante. Au redémarrage, le contrôleur relit le CR (toujours en `Terminating` dans etcd tant que
le finalizer n'a pas terminé), regarde `status.deletion.stage`, et reprend exactement où il s'est
arrêté — pas de retraitement d'une étape déjà faite (un dépairage Rancher déjà effectué ne
redéclenche pas un deuxième `DeleteCluster`, le contrôleur vérifie l'état réel avant d'agir, comme
`runCleanupAndDelete` le fait déjà aujourd'hui de façon manuelle). C'est très exactement ce que
`data/deleting.json` et `cfg.ListCleaning()`/`AddCleaning()`/`RemoveCleaning()` font à la main
aujourd'hui via `StartReconcilers()` — le CR et son `status` remplacent ce fichier d'état ad hoc,
avec un avantage : l'état vit sur l'objet lui-même, pas dans un fichier ou une ConfigMap à tenir
synchronisée séparément, donc pas de risque de désynchronisation entre « ce que Kubernetes sait »
et « ce que le fichier d'état dit ».

## 5. Budget de ressources par cluster hôte

### 5.1 Où le calculer — je tranche pour le reconcile, pas le webhook, pour la v1

Deux options :

- **Webhook d'admission** (`ValidatingWebhookConfiguration`) : bloque avant même que l'objet soit
  persisté. Propre, mais ajoute un service HTTP avec certificat TLS à maintenir, et une panne du
  webhook bloque *toute* admission de CR `VCluster* `— y compris des mises à jour sans rapport
  avec le budget.
- **Contrôleur au moment du reconcile** : le CR est admis normalement, le contrôleur pose
  `BudgetExceeded` et ne provisionne rien. Si la `Kustomization` Flux qui applique le CR a un
  health check sur la condition `Ready`, l'échec reste visible dans `flux get kustomizations`
  exactement comme un échec d'admission le serait — sans l'infrastructure webhook.

Je retiens **le reconcile** pour la v1 : c'est la même quantité de garde-fou visible côté Flux,
sans certificat à gérer ni service à garder disponible pour que la création fonctionne. Un
webhook reste une amélioration valable plus tard (retour plus rapide, rien n'est même créé) si le
volume de créations le justifie — je ne le ferme pas, je le retarde.

### 5.2 Sur quelle source

Pas de capacité « déclarée » qui existe déjà quelque part : je propose un **plafond statique par
cluster hôte**, configuré comme variable d'environnement de l'opérateur (dans le même style que
`DEFAULT_RBAC_GROUP` aujourd'hui — ex. `RESOURCE_BUDGET_CPU`, `RESOURCE_BUDGET_MEMORY`,
`RESOURCE_BUDGET_STORAGE`), ajusté à la main par l'exploitation quand l'infra grandit. Je n'ai pas
retenu la somme des `Allocatable` des nœuds : le cluster hôte porte aussi d'autres charges que des
vclusters, et calculer depuis les nœuds reviendrait à vouloir remplir le cluster plutôt qu'à
garder une marge délibérée. Le calcul lui-même : somme des `ResourceQuota.status.hard` de tous
les namespaces `vcluster-*` du cluster hôte (le contrôleur le fait déjà pour l'usage affiché dans
`status.resourceUsage`), comparée au plafond configuré.

### 5.3 Absence de plafond configuré — refuser, pas laisser passer

Si le plafond n'est pas configuré, le contrôleur **refuse** la création (`BudgetExceeded` avec un
message explicite « pas de budget configuré »), il ne laisse pas passer avec un simple
avertissement. Ce contrôle remplace une revue humaine qui, par défaut, penchait plutôt vers la
prudence ; le laisser passer silencieusement en absence de configuration viderait le contrôle de
son sens au moment précis où il devrait le plus s'appliquer (un opérateur mal configuré). Le
compromis : une mauvaise configuration se voit tout de suite (plus aucune création ne passe), donc
se corrige vite — plutôt qu'un trou de sécurité qui ne se voit jamais.

## 6. Migration des vclusters existants

Le point dur nommé dans l'ADR : tout ce qui a été ajusté à la main dans fluxprod et que
`generator.go` ignore sera perdu à la première réconciliation par le CR. Procédure d'inventaire,
prérequise à toute migration — aucun code à écrire ici, juste la marche à suivre :

1. Pour chacun des vclusters existants (les deux environnements), reconstruire un
   `CreateRequest`/`UpdateRequest` à partir de ce que `parser.ParseVCluster` lit aujourd'hui, puis
   regénérer les fichiers attendus avec `generator.GenerateVCluster` / les fonctions `Update*`.
2. Comparer, fichier par fichier, ce rendu avec le contenu réel dans fluxprod
   (`gitlab.ListFiles` + `GetFile` sur chaque chemin). Produire un rapport de diff par vcluster.
3. Classer chaque diff : dérive sans intérêt (espace, ordre, valeur par défaut qui a changé côté
   générateur depuis) vs. ajustement manuel intentionnel (quelqu'un a changé une valeur à la main
   pour une raison qui ne passe pas par le formulaire).
4. Pour chaque ajustement intentionnel : soit le futur schéma `VClusterSpec` a un champ qui le
   couvre (rien à perdre), soit il n'en a pas — dans ce cas, décision explicite avant de migrer ce
   vcluster précis : étendre le schéma pour le couvrir, ou accepter consciemment la perte (avec
   qui a décidé, noté quelque part, pas juste oublié dans un commit de migration).
5. Migrer un vcluster à la fois, en commençant par celui qui a le diff le plus vide : un commit
   qui remplace l'arborescence de fichiers par le CR équivalent, puis vérification que Flux/kro
   reproduisent le même état (pas de redémarrage du pod vcluster, pas de diff sur le
   `HelmRelease`) avant de passer au suivant. Avec six vclusters au total, ce rythme reste
   praticable sans automatisation supplémentaire.
6. Le code de génération de l'ancienne arborescence (`generator.go` dans sa forme actuelle) ne
   disparaît qu'une fois les six migrés.

## 7. Inconnues à lever par le POC kro

Sur un vcluster preprod réel, pas des généralités :

1. **Ownership et conflits de champ manager** : quand kro applique le `HelmRelease` généré depuis
   le CR, et que Flux réconcilie aussi ce même objet (health check, drift detection), qui gagne
   sur un champ que les deux touchent ? Reproduire le même schéma de field-manager propre que
   `SetNamespaceProtection` utilise déjà (Server-Side Apply avec un manager dédié) pour éviter que
   FluxCD écrase ce que kro vient de poser, ou l'inverse.
2. **Prune quand un champ disparaît du CR** : si `spec.argoCD` passe de renseigné à `nil`, les
   ressources ArgoCD déjà matérialisées (Kustomization, ConfigMaps) sont-elles supprimées par kro,
   ou faut-il un mécanisme de garbage collection explicite (`ownerReferences`) en plus ?
3. **Ce que le CEL de kro peut exprimer** : le générateur actuel a de la génération conditionnelle
   (fichiers ArgoCD seulement si `spec.argoCD.enabled`, fichiers FluxCD seulement si
   `spec.fluxCD` renseigné). Vérifier que ça se fait proprement en CEL/template kro, pas en
   bricolant des ressources vides qu'il faudrait ensuite filtrer.
4. **Ce que le `status` de kro donne réellement** : une condition `Ready` agrégée directement
   utilisable, ou faut-il que le contrôleur Go recalcule sa propre agrégation à partir de l'état
   brut de chaque ressource enfant ?
5. **Stabilité inter-versions de kro** : le CRD `ResourceGraphDefinition` lui-même a-t-il eu des
   changements cassants récents ? À vérifier avant d'y accrocher la source de vérité de six
   vclusters en prod.

Si le POC échoue sur un de ces points au-delà de ce qui est réparable, la décision de source de
vérité (le CR versionné) tient quand même — c'est uniquement l'expansion qui repasserait sur du
`controller-runtime` classique (ADR-001, risques).
