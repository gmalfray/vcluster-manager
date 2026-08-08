# Audit — les deux `cluster-admin` du cluster de recette

> Écrit le 2026-08-08 sur le cluster de recette (`vcluster-mgr`, k3s v1.32.4+k3s1).
> Audit en **lecture seule** : aucune écriture cluster, aucun objet créé, aucun
> mot de passe testé. Ce fichier est le seul écrit.
>
> Convention reprise de `etat-brique-operateur.md` : **inconnu n'est pas faux**.
> Chaque constat porte son statut — **Prouvé** (une commande l'a montré, elle est
> citée), **Déduit** (une lecture de config le rend certain sans exécution),
> **Plausible** (hypothèse à vérifier, et ce qui manque pour trancher).

## Sommaire des verdicts

| # | Sujet | Verdict |
|---|---|---|
| 1 | `velero-system/velero-server` est cluster-admin | **Acceptable en soi, inacceptable dans ce contexte.** Le droit large de Velero n'est pas le problème principal : ce qu'il garde l'est déjà sans lui. |
| 2 | `User:user-psmxl` est cluster-admin | **Attendu, pas un résidu.** C'est le compte `admin` de bootstrap de Rancher, créé par le chart. Le risque n'est pas le binding, c'est son exposition. |

Trois constats sont apparus en chemin et pèsent plus lourd que les deux sujets.
Ils sont en §3, avec le même niveau de preuve.

---

## 1. `velero-system/velero-server` est cluster-admin

### 1.1 Le constat

**Prouvé.** Deux droits, pas un :

```
kubectl get clusterrolebinding velero-server -o yaml
  → roleRef: ClusterRole/cluster-admin, subject ServiceAccount velero-system/velero-server

kubectl -n velero-system get role velero-server -o yaml
  → rules: apiGroups [*] / resources [*] / verbs [*]
```

Le Role namespacé est redondant avec le cluster-admin. C'est le défaut du chart
`velero` 8.2.0.

Le même ServiceAccount porte **deux** charges de travail, ce qui compte pour la
suite :

- le Deployment `velero` (v1.15.1, `--uploader-type=kopia`) ;
- le DaemonSet `node-agent`, qui tourne **`runAsUser: 0`** et monte
  **`hostPath: /var/lib/kubelet/pods`**.

```
kubectl -n velero-system get ds node-agent -o json | jq '.spec.template.spec.securityContext, [.spec.template.spec.volumes[]|select(.hostPath)]'
  → {"runAsUser": 0}
  → [{"hostPath":{"path":"/var/lib/kubelet/pods"},"name":"host-pods"}]
```

### 1.2 Ce qu'une compromission emporte, chiffré sur ce produit

Un backup de tenant contient les secrets du namespace hôte. Ce n'est pas une
supposition, les journaux de Velero le nomment item par item.

**Prouvé** — pour le backup `manual-recette-restore-a-1786205874675` :

```
kubectl -n velero-system logs deploy/velero --since=6h \
  | grep manual-recette-restore-a-1786205874675 | grep 'Backing up item' | grep resource=secrets
```

Sept secrets, dont :

| Secret sauvegardé | Ce qu'il donne |
|---|---|
| `vc-vcluster-recette-restore-a` | le **kubeconfig admin du vcluster** : `certificate-authority`, `client-certificate`, `client-key`, `token`, `config` |
| `vc-vcluster-…-ext` / `-int` | le même, avec les URL externe et interne |
| `vcluster-…-certs` (29 clés) | l'autorité de certification du vcluster — de quoi **forger** des identités dedans |
| `wildcard-preprod-rebuild-it-fr-tls` | la **clé privée du certificat wildcard** `*.preprod.rebuild-it.fr`, propagée dans chaque namespace tenant par `reflector` |
| `vault-webhook-webhook-tls-…` | le TLS du webhook Vault du tenant |

Donc, en volume, aujourd'hui :

- **4 tenants vivants** (`vcluster-demo`, `recette-cdv-1`, `recette-restore-a`,
  `recette-restore-b`) ;
- **9 objets `Backup` dans le bucket, couvrant 7 namespaces tenants distincts** —
  dont six (`recette-n6-a`, `n6-b`, `n6-c`, `n6-d`, `n6-d2`, `n6-d3`) **n'existent
  plus dans le cluster**. Le TTL par défaut posé par l'app est `720h0m0s`
  (`internal/kubernetes/velero.go:340`), soit 30 jours : *le bucket contient les
  clés de tenants supprimés pendant un mois après leur suppression*. La surface
  du bucket est plus large que celle du cluster vivant, et elle ne rétrécit pas
  quand on supprime un tenant.

Lire le bucket, c'est donc obtenir l'accès **admin** à chaque vcluster
sauvegardé, sans jamais toucher au cluster hôte. Pour la promesse d'isolation du
produit, c'est le pire résultat possible : la restauration du tenant A est aussi
la clé du tenant B.

### 1.3 Le chemin d'exfiltration ne passe pas par le pod Velero

C'est le point qui déplace la question. Compromettre `velero-server` est un
chemin ; il en existe un plus court, qui n'a besoin d'aucun droit RBAC.

**Prouvé** — les identifiants S3 de Velero sont le compte **root** de MinIO, et
ce sont les identifiants par défaut du produit :

```
# comparaison faite sans afficher les valeurs
secret velero-system/velero (clé cloud).aws_access_key_id     == minio.rootUser      → OUI
secret velero-system/velero (clé cloud).aws_secret_access_key == minio.rootPassword  → OUI
longueurs : 10 et 10
```

Et la source, versionnée en clair dans le dépôt d'infra —
`terraform/02-platform/velero.tf`, commit `6753411` :

```hcl
set { name = "rootUser"     value = "minioadmin" }
set { name = "rootPassword" value = "minioadmin" }
…
aws_access_key_id = minioadmin
aws_secret_access_key = minioadmin
```

`minioadmin/minioadmin` est le couple par défaut de MinIO. Il est en dur dans un
fichier suivi par git (`git ls-files terraform/02-platform/velero.tf` → suivi ;
`git log -S minioadmin` → introduit par `6753411`). Le `.gitignore` de l'infra
protège correctement `*.tfstate` et `*.tfvars`, mais ce secret-ci n'est pas
passé par une variable : il est écrit dans le `.tf`.

Reste à savoir qui peut joindre MinIO. **Prouvé** : `minio` est un Service
`ClusterIP` sur `9000`, et il n'y a **aucune NetworkPolicy** dans
`velero-system` ni dans aucun namespace `vcluster-*` :

```
kubectl get netpol -A
  → 4 policies au total, toutes dans flux-system et cattle-fleet-local-system
```

**Déduit** (de la config, sans l'exécuter) : la configuration vcluster déployée
porte `policies.networkPolicy.enabled: false` (lu dans le secret
`vc-config-vcluster-recette-restore-a`). Un pod lancé par un tenant dans son
vcluster est synchronisé en pod réel dans le namespace hôte `vcluster-<nom>` —
on le voit directement :

```
kubectl -n vcluster-recette-restore-a get pods
  → vault-webhook-…-x-vault-system-x-vcluster-…   (workload tenant, syncé sur l'hôte)
```

Ce pod a donc un accès réseau libre à `minio.velero-system.svc:9000`, et le mot
de passe est public. **Un tenant lit les sauvegardes de tous les autres tenants
sans aucun droit sur l'API Kubernetes de l'hôte.**

C'est ce qui rend le sujet « velero est cluster-admin » secondaire : le trésor
que ce droit est censé garder est déjà ouvert par une autre porte.

### 1.4 Le confused deputy : `create restores` vaut cluster-admin

Deuxième effet, celui-là interne au produit. Velero exécute une restauration
**avec ses propres droits**, sans notion de tenant. Donc tout sujet capable de
créer un objet `Restore` dans `velero-system` obtient l'équivalent de
cluster-admin par délégation.

**Prouvé** — les deux ServiceAccounts du produit l'ont, sans borne de namespace :

```
kubectl auth can-i create restores.velero.io -n velero-system \
  --as=system:serviceaccount:vcluster-manager:vcluster-manager           → yes
kubectl auth can-i create backups.velero.io  -n velero-system \
  --as=system:serviceaccount:vcluster-manager:vcluster-manager           → yes
kubectl auth can-i create restores.velero.io -n velero-system \
  --as=system:serviceaccount:vcluster-manager:vcluster-manager-operator  → yes
```

Le `spec` d'un `Restore` accepte un `namespaceMapping` arbitraire — l'app s'en
sert légitimement pour les restaurations croisées
(`CreateVeleroRestore`, `internal/kubernetes/velero.go:293-328`). Le même champ,
détourné, restaure le backup du tenant A dans un namespace choisi par
l'attaquant. Rien dans le RBAC ne restreint la valeur d'un champ de spec.

Le ClusterRole de l'app a par ailleurs plusieurs droits qui **paraissent** bornés
et ne le sont pas — la famille exacte du `list secrets` trouvé sur
`ingress-nginx` :

| Règle (`deploy/base/rbac.yaml`) | Ce que ça a l'air d'être | Ce que c'est |
|---|---|---|
| `secrets: [get]` | « juste get, pas list » | `get` **sur tous les namespaces**, et les noms sont déterministes : `vc-vcluster-<nom>`. Prouvé : `can-i get secret/vc-vcluster-recette-restore-a -n vcluster-recette-restore-a --as=…:vcluster-manager` → **yes** ; `can-i get secrets -n kube-system` → **yes** |
| `persistentvolumeclaims: [get,list,delete]` | « pour la restauration in-place » | `delete` sur **tout PVC du cluster**. Prouvé : `can-i delete pvc -n velero-system` → **yes**, soit le volume de MinIO lui-même |
| `namespaces: [get,list,update]` | « pour l'annotation de protection » | `update` sur tout namespace, labels d'admission compris |
| opérateur : `namespaces: […,delete]` | « pour le finalizer » | `delete` sur **tout namespace**. Prouvé : `can-i delete namespaces --as=…:vcluster-manager-operator` → **yes** |

Le code borne ces appels (le nom du PVC est dérivé, cf. `DeleteVClusterPVC`), le
RBAC non. C'est la même asymétrie que celle qui a motivé la garde de placement :
la règle vit dans le code, alors que l'API server pourrait la tenir.

Note d'outillage confirmée sur ce cluster (kubectl v1.35) :

```
kubectl auth can-i create pods/portforward           --as=…:vcluster-manager-operator → no   (faux)
kubectl auth can-i create pods --subresource=portforward --as=…:vcluster-manager-operator → yes  (vrai)
```

La forme avec slash ment. Toute recette qui vérifie un droit de sous-ressource
avec la forme `res/sub` valide un « non » qui n'existe pas.

### 1.5 La garde de placement tient-elle face au RBAC de Velero ?

`internal/controller/namespace_guard.go` rend les objets auto-bornés : un
`VClusterVeleroOps` nommé `X` ne peut vivre que dans `vcluster-X`, et
`service.ValidName` interdit le nom qui retomberait sur `vcluster-manager`.
La règle est bonne, et le commentaire du fichier décrit exactement le risque
(« un backup de `vcluster-manager` expédie le token GitLab, le secret Keycloak
et `JWT_SECRET` vers le bucket »).

**Prouvé, sur le contenu réel** — c'est bien ce qu'il y a dans ce namespace :

```
kubectl -n vcluster-manager get secret vcluster-manager-secrets -o json | jq -r '.data|keys[]'
  → FLUXCD_DEPLOY_KEY_ID, GITLAB_ARGOCD_GROUP_ID, GITLAB_HELM_PROJECT_ID,
    GITLAB_PROJECT_ID, GITLAB_TOKEN, RANCHER_TOKEN, SESSION_SECRET,
    VAULT_ROLE_ID, VAULT_SECRET_ID
kubectl -n vcluster-manager get secret vcluster-manager-auth -o json | jq -r '.data|keys[]'
  → ADMIN_PASSWORD, JWT_SECRET, KEYCLOAK_CLIENT_SECRET, OIDC_CLIENT_SECRET
```

La réponse à la question posée est donc en deux temps :

- **Pour un tenant : la garde tient, et elle n'est même pas le rempart utile.**
  Un tenant n'a aucun droit sur l'API de l'hôte, il ne peut pas déposer de
  marqueur. Ce qu'il a, c'est le chemin réseau du §1.3, qui ne passe pas du tout
  par le marqueur.
- **Pour le porteur d'un des deux ServiceAccounts du produit : la garde est
  contournable, parce qu'elle n'est pas sur le chemin.** La garde filtre le
  marqueur ; le RBAC autorise l'appel **direct** à `backups.velero.io` /
  `restores.velero.io`, qui ne lit aucun marqueur. Un attaquant qui obtient
  l'exécution dans le pod de l'app ne dépose pas de marqueur nommé `manager` —
  il crée un `Backup` avec `includedNamespaces: [vcluster-manager]`, puis lit le
  résultat dans MinIO. **Statut : déduit du RBAC et du code, non exécuté** (créer
  un Backup serait une écriture).

Autrement dit, la garde ferme la porte que le design avait identifiée, et le RBAC
laisse la fenêtre à côté ouverte.

### 1.6 Est-ce réductible en pratique ?

Ce que ce déploiement utilise réellement, **prouvé** par la spec et par le
`terraform/02-platform/velero.tf` :

- un seul plugin : `velero-plugin-for-aws:v1.11.0` ;
- `snapshotsEnabled: false`, `volumeSnapshotLocation: []` — pas de CSI, pas de
  `VolumeSnapshot` ;
- `deployNodeAgent: true`, uploader **kopia**, et l'app pose systématiquement
  `defaultVolumesToFsBackup: true` + `snapshotVolumes: false`
  (`internal/kubernetes/velero.go:351-355`) ;
- `excludedResources: [events, leases, pods, replicasets.apps]` ;
- `includedNamespaces` toujours réduit à **un** namespace `vcluster-<nom>` —
  aucun backup `*`, aucun `Schedule` (`kubectl -n velero-system get schedules`
  → vide) ;
- un seul BSL, provider `aws`, `s3Url: http://minio.velero-system.svc:9000`.

Un ClusterRole taillé sur cet usage est donc **techniquement descriptible** :
lecture large (`get/list/watch` sur `*`) pour la phase backup, écriture large
mais bornée aux namespaces `vcluster-*` pour la phase restore, plus le groupe
`velero.io` et les `pods/exec` dont kopia a besoin.

Mais c'est là que le compromis se joue, et ma réponse est **non, pas en l'état** :

1. **La lecture ne se réduit pas.** Sauvegarder un namespace, c'est lire *toutes*
   les ressources qui s'y trouvent, y compris les types que Velero découvre au
   moment de l'exécution. Un tenant qui installe une CRD dans son vcluster ne
   change rien côté hôte (les CRD du vcluster vivent dans son etcd, pas sur
   l'hôte) — mais l'hôte, lui, en gagne : `vault-webhook` a déjà déposé des
   objets syncés dans `vcluster-recette-restore-a`. La liste des ressources à
   lire n'est pas fermée. Concrètement, `get/list/watch` sur `*` reste
   nécessaire, et **`list` sur `*` inclut `list secrets` cluster-wide** — soit
   exactement le droit dont on cherchait à se débarrasser. La réduction gagne
   sur l'écriture, pas sur la lecture.
2. **`node-agent` annule le bénéfice restant.** Il partage le SA et monte
   `/var/lib/kubelet/pods` en root : ce répertoire contient les volumes projetés
   de **tous** les pods du nœud, donc les jetons de tous les ServiceAccounts et
   tous les secrets montés. Réduire le ClusterRole sans traiter ce hostPath, c'est
   fermer la porte principale en laissant l'échelle contre la fenêtre. **Prouvé**
   par la spec du DaemonSet ; l'exploitation elle-même n'a pas été tentée.
3. **Le coût d'une restauration cassée est asymétrique.** Un droit manquant se
   révèle au moment où on restaure, c'est-à-dire au moment où quelque chose est
   déjà cassé — et le chemin de restauration de ce produit **supprime le PVC**
   avant d'appeler Velero (`DeleteVClusterPVC`, `ScaleVClusterWorkloads` à 0).
   Une restauration refusée pour cause de RBAC après la suppression du PVC laisse
   le tenant **sans données et sans sauvegarde restaurée**. `GetVClusterPVCState`
   est justement là pour détecter ce « point de non-retour ». Un RBAC réduit
   déplace le risque de « quelqu'un peut tout lire » vers « on peut perdre un
   tenant », et le second n'est pas meilleur.

**Verdict §1.** Le cluster-admin de Velero est **justifié par sa fonction et
acceptable en soi** ; le réduire aujourd'hui coûterait cher et ne fermerait rien,
parce que les deux vrais chemins (MinIO en `minioadmin`, `create restores` chez
l'app) ne passent pas par lui. Ce qui n'est pas acceptable, c'est **ce que ce
droit garde** : un bucket ouvert et des sauvegardes en clair.

### 1.7 Remédiations, avec leur coût

Par ordre de rapport gain/coût, pas par ordre de gravité.

**R1 — Identifiants MinIO propres, et un compte S3 dédié à Velero.**
*Gain* : ferme le chemin du §1.3, celui qui n'exige aucun droit.
*Coût* : un `random_password` dans `velero.tf`, un utilisateur MinIO non-root
avec une policy limitée au bucket `velero`, et une réinstallation du chart MinIO
(le rootUser ne se change pas à chaud). Le secret Velero doit être régénéré au
même moment, sinon Velero perd le bucket.
*Ce que ça casse* : le mot de passe actuel est en clair dans git — il faut le
considérer comme brûlé et **purger l'historique** ou accepter qu'il reste lisible
dans `6753411`. La rotation invalide toute config MinIO manuelle (client `mc`,
console).
*Risque résiduel* : nul si fait proprement. C'est le premier geste.

**R2 — Une NetworkPolicy de refus par défaut sur `velero-system`.**
*Gain* : même si les identifiants fuitent à nouveau, un pod tenant ne joint plus
MinIO. C'est la défense en profondeur qui rend R1 durable.
*Coût* : une policy ingress qui n'autorise que `velero-system` lui-même. Faible.
*Ce que ça casse* : le node-agent et Velero doivent rester autorisés (même
namespace, donc couvert) ; si un jour la console MinIO est exposée par un
Ingress, il faudra ouvrir depuis `ingress-nginx`. Attention aussi au fait que
k3s+flannel applique bien les NetworkPolicy — à vérifier avant de compter dessus,
c'est le genre de contrôle qui échoue en silence.

**R3 — Chiffrer les sauvegardes au repos.**
*Gain* : la lecture du bucket ne suffit plus. C'est la seule mesure qui protège
les backups des tenants **déjà supprimés** encore présents 30 jours.
*Coût* : le chiffrement côté serveur MinIO (SSE-KMS) demande un KMS ; Vault est
déjà là, mais c'est un chantier, pas un réglage. Alternative moins chère :
raccourcir le TTL par défaut de `720h` à quelque chose de justifié par un besoin
réel, ce qui réduit la fenêtre sans rien chiffrer.
*Ce que ça casse* : un TTL raccourci supprime des points de restauration —
décision produit, pas décision sécurité. Le chiffrement, lui, complique la
restauration de secours si la clé est perdue : une clé de chiffrement stockée
uniquement dans le cluster qu'on restaure est un piège.

**R4 — Séparer le ServiceAccount du `node-agent` de celui de `velero`.**
*Gain* : le node-agent n'a pas besoin de cluster-admin ; il lui faut lire les
`PodVolumeBackup`/`PodVolumeRestore` et les pods. Sa surface (root + hostPath)
cesse d'emporter aussi l'API.
*Coût* : le chart 8.2.0 ne l'expose pas proprement — il faut soit un patch
kustomize sur le DaemonSet, soit un fork des values. À revalider à chaque montée
de chart, sous peine de retour silencieux au défaut.
*Ce que ça casse* : si un droit manque, le fs-backup échoue **silencieusement en
`PartiallyFailed`** — statut déjà observé sur `temoin-avec-pods-restore-a`, ce
qui montre que ce mode d'échec passe inaperçu ici. À ne faire qu'avec une recette
backup+restore complète derrière.

**R5 — Borner le RBAC de l'app et de l'opérateur sur `velero.io` et les PVC.**
*Gain* : casse le confused deputy du §1.4. `create backups/restores` limité au
namespace `velero-system` est déjà le cas ; ce qui manque, c'est de ne pas
pouvoir viser n'importe quel namespace dans le `spec`.
*Coût* : le RBAC **ne sait pas** filtrer sur la valeur d'un champ de spec. Il
faut un webhook d'admission (ou une ValidatingAdmissionPolicy CEL, disponible en
1.32) qui refuse un `Backup`/`Restore` dont `includedNamespaces` sort de
`vcluster-*` et dont le `namespaceMapping` change de tenant. C'est le vrai coût :
un composant d'admission de plus à écrire, tester et maintenir.
*Ce que ça casse* : une policy trop stricte bloque les restaurations croisées
légitimes (`targetNS != sourceNS` est un cas supporté du produit). À écrire en
`audit` avant `enforce`, sinon on découvre la règle trop serrée pendant un
incident.
*Note* : `persistentvolumeclaims: delete` cluster-wide se borne, lui, sans
webhook — l'app ne supprime que des PVC dans `vcluster-*`. Le RBAC ne sait pas
filtrer par préfixe de namespace non plus, mais un Role namespacé par tenant,
posé par l'opérateur au provisionnement, le ferait. Coût : un objet de plus par
tenant, et un chemin de migration pour les tenants existants.

**R6 — Une sonde RBAC pour l'infra, sur le modèle de `rbac_probe_test.go`.**
Le dispositif applicatif (`internal/controller/rbac_probe_test.go`) fait tourner
le vrai code derrière le vrai ClusterRole commité, via un proxy qui impersonne le
ServiceAccount — il existe parce qu'un `list resourcequotas` manquant était parti
en production avec tous les tests au vert. Son propre commentaire dit ce qu'il ne
couvre pas, et la dernière ligne est celle qui compte ici : « l'écart entre le
ClusterRole COMMITÉ et celui qui est réellement DÉPLOYÉ ».
L'infra n'a **aucun équivalent** : rien ne vérifie qu'un composant de plateforme
a exactement les droits attendus. Un test d'infra utile ne serait pas un test de
code mais un **test d'assertions négatives** :

```
# forme attendue, à exécuter en recette après apply
kubectl auth can-i list secrets -A --as=system:serviceaccount:ingress-nginx:ingress-nginx   # doit être: no
kubectl auth can-i get  secrets -A --as=system:serviceaccount:vcluster-manager:vcluster-manager  # à statuer
kubectl auth can-i create pods --subresource=portforward --as=…                             # forme correcte obligatoire
```

*Coût* : un script et une liste d'assertions à tenir à jour — c'est-à-dire
exactement le « registre écrit à la main » que la doctrine du dépôt déconseille.
La version honnête l'assume : ces assertions décrivent une **frontière**, pas un
état d'avancement, et une frontière se tient à la main.
*Ce que ça casse* : rien, sauf du temps de recette. Le piège à éviter est le
faux vert du `res/sub` documenté au §1.4.

---

## 2. `User:user-psmxl` est cluster-admin

### 2.1 D'où il vient — établi factuellement

**Prouvé.** Ce n'est pas un compte nominatif : c'est le compte `admin` de
bootstrap de Rancher.

```
kubectl get users.management.cattle.io user-psmxl -o json
  → username:    "admin"
    displayName: "Default Admin"
    labels:      authz.management.cattle.io/bootstrapping: "admin-user"
                 cattle.io/last-login: "1786187144"
    principalIds: ["local://user-psmxl"]
    creationTimestamp: 2026-08-08T10:57:02Z
```

La chaîne complète, dans l'ordre où elle a été posée :

| Objet | Créé à | Ce qui l'a créé |
|---|---|---|
| `users.management.cattle.io/user-psmxl` | 10:57:02 | contrôleur Rancher au bootstrap |
| `globalrolebindings/globalrolebinding-fzhkw` (user-psmxl → GlobalRole `admin`) | 10:57:03 | idem |
| `clusterrolebindings/globaladmin-user-psmxl` → `cluster-admin` | 10:57:10 | **dérivé** du GlobalRoleBinding : l'objet porte `authz.cluster.cattle.io/globalrolebinding: globalrolebinding-fzhkw` |

Le CRB n'est donc pas un objet indépendant : c'est la projection Kubernetes du
GlobalRole `admin` de Rancher. Le supprimer ne servirait à rien, le contrôleur
Rancher le recrée.

Origine côté Terraform, **prouvée** dans
`~/GIT/github/vcluster-manager-infra/terraform/02-platform/rancher.tf` :
`helm_release.rancher` (chart stable 2.14.3) avec
`set_sensitive { name = "bootstrapPassword" … }` alimenté par
`random_password.rancher_bootstrap` (24 caractères, `special = false`) quand
`var.rancher_bootstrap_password` est vide — ce qui est le cas ici :
`grep -c rancher_bootstrap_password terraform/terraform.tfvars` → **0**.

**Réponse aux trois questions posées :**

- **D'où il vient** : du chart Rancher, déclenché par Terraform. Pas créé à la
  main, pas créé par une ressource Terraform dédiée.
- **À quoi il correspond** : le compte `admin` de Rancher, `Default Admin`.
- **Attendu ou résidu** : **attendu**. C'est le compte sans lequel on ne peut pas
  se connecter à Rancher la première fois. Et il **sert** : la même journée,
  `cattle.io/last-login` = 2026-08-08 13:05:44 CEST, et un token de session UI
  `token-ww9mz` (`ttl=57600000`, soit 16 h) a été utilisé à 16:24:10.

Aucune donnée personnelle n'a été lue ni n'est reprise ici : `displayName` est
`Default Admin`, une valeur produit, pas une identité.

### 2.2 Le vrai risque n'est pas le binding, c'est son exposition

**Prouvé.** Rancher est publié sur Internet :

```
kubectl get ingress -A          → cattle-system/rancher, host rancher.rebuild-it.fr
kubectl get settings.management.cattle.io server-url  → https://rancher.rebuild-it.fr
```

Donc : un compte **cluster-admin du cluster hôte**, authentifié par un simple mot
de passe, sur un formulaire ouvert au monde. Ce compte est aussi celui qui, dans
Rancher, ouvre l'accès aux clusters appairés — soit tous les vclusters du
produit. Pas de MFA constaté ; le provider est `local` (`authProvider: local` sur
le token de session), pas Keycloak, alors que Keycloak est déployé à côté.

`first-login` vaut `false`, donc l'écran de premier login a été franchi.

**Ce que je ne peux pas trancher, et ce qui manque pour le faire.**
`first-login: false` ne prouve **pas** que le mot de passe a changé : Rancher
bascule ce réglage dès le premier login réussi, et l'écran de changement peut
être passé en conservant le mot de passe courant. Le mot de passe généré est par
ailleurs en clair dans `terraform/02-platform/terraform.tfstate` (output
`rancher_bootstrap_password`, 24 caractères, `sensitive: true` mais la valeur y
est), fichier **non versionné** (`.gitignore` couvre `*.tfstate`) mais en
permissions `-rw-rw-r--` sur le poste. Trancher demanderait soit de tester
l'authentification — je ne le fais pas, c'est offensif et hors mandat — soit une
confirmation directe de la personne qui a fait le premier login. **Statut :
indéterminé, et je ne le devine pas.**

### 2.3 Remédiations, avec leur coût

**R7 — Confirmer que le mot de passe bootstrap n'est plus valide.**
*Gain* : lève la seule inconnue. Si le mot de passe est resté celui du tfstate,
la compromission du poste ou du fichier vaut cluster-admin.
*Coût* : une minute et une réponse humaine. C'est la première chose à faire.
*Ce que ça casse* : rien.

**R8 — Brancher Rancher sur Keycloak (OIDC) et réduire `admin` à un compte de
secours.**
*Gain* : l'accès cluster-admin passe par l'IdP déjà déployé, avec ses règles.
Le compte local devient un « break-glass » qu'on n'utilise plus au quotidien.
*Coût* : configurer le provider dans Rancher, mapper les groupes, et **garder**
le compte local actif — un IdP en panne sans compte de secours enferme dehors.
*Ce que ça casse* : le mapping de groupes se trompe facilement de sens, et un
mauvais mapping retire les droits à tout le monde d'un coup. À faire avec une
session admin locale ouverte en parallèle.

**R9 — Restreindre l'accès réseau à l'Ingress Rancher.**
*Gain* : un formulaire de login cluster-admin ne devrait pas être joignable
depuis n'importe quelle IP. `nginx.ingress.kubernetes.io/whitelist-source-range`
suffit.
*Coût* : très faible, mais opérationnellement contraignant si les IP sources ne
sont pas stables.
*Ce que ça casse* : un accès depuis une IP non listée, y compris le vôtre en
déplacement. Bien vérifier que le rétablissement ne passe pas *par Rancher*.

**R10 — Ne pas toucher au CRB `globaladmin-user-psmxl`.**
Il est dérivé du GlobalRoleBinding et sera recréé. Le supprimer donne l'illusion
d'avoir agi et casse Rancher jusqu'à la prochaine réconciliation.
*Coût* : zéro, c'est une non-action. Elle est ici pour être écrite noir sur
blanc, parce que c'est le geste réflexe quand on lit la liste des cluster-admin.

**Verdict §2.** Le binding est **attendu et légitime**, il n'y a pas de résidu à
nettoyer. Le point à traiter est ailleurs : un compte cluster-admin à mot de
passe unique, exposé publiquement, dont on ne sait pas si le mot de passe initial
a été changé.

---

## 3. Trois constats trouvés en chemin, qui pèsent plus lourd

Ils ne faisaient pas partie du mandat, mais les ignorer rendrait les deux
verdicts ci-dessus trompeurs : ils décrivent des chemins plus courts vers le même
résultat.

### 3.1 Les tenants peuvent lancer des pods privilégiés sur l'hôte — Critique

**Prouvé** (par la configuration réellement déployée, lue dans le secret
`vc-config-vcluster-recette-restore-a`) :

```
policies.podSecurityStandard: privileged
policies.networkPolicy.enabled: false
```

Et côté hôte, **aucun** label Pod Security Admission :

```
kubectl get ns -o json | jq '… pod-security.kubernetes.io/enforce …'
  → vcluster-demo, vcluster-manager, vcluster-recette-*, velero-system : tous "-"
```

Aucun webhook d'admission ne comble le trou (`validatingwebhookconfigurations` :
capi, cert-manager, ingress-nginx, rancher — aucun ne filtre les pods).

**Déduit, non exécuté** (créer un pod serait une écriture) : un tenant crée dans
son vcluster un pod `privileged: true` avec un `hostPath: /`. vcluster le
synchronise dans `vcluster-<nom>` sur l'hôte, sans le filtrer. Le nœud est
unique : c'est root sur le nœud, donc sur le control-plane k3s, donc sur tous les
tenants et sur MinIO.

Si ce chemin est réel, il rend les §1 et §2 largement académiques : l'isolation
entre tenants — la promesse centrale du produit — ne repose sur rien d'appliqué.
Ce qui manque pour trancher définitivement : lancer le pod dans un vcluster de
recette jetable et regarder ce qui arrive dans le namespace hôte. C'est un test
d'écriture, à faire dans un chantier dédié, pas ici.

### 3.2 L'API server des vclusters est joignable en HTTP clair depuis Internet — Haut

**Prouvé, par une requête sortante non authentifiée :**

```
curl -o /dev/null -w '%{http_code}' http://recette-restore-a.api.preprod.rebuild-it.fr/version
  → 200

openssl s_client -connect recette-restore-a.api.preprod.rebuild-it.fr:443 … | openssl x509 -noout -subject
  → subject=O=Acme Co, CN=Kubernetes Ingress Controller Fake Certificate
```

La cause est un réglage manquant. L'Ingress du vcluster demande
`nginx.ingress.kubernetes.io/ssl-passthrough: "true"`, mais le contrôleur ne
tourne **pas** avec `--enable-ssl-passthrough` :

```
kubectl -n ingress-nginx get pods -o json | jq -r '.items[].spec.containers[].args[]'
  → …--validating-webhook…, --enable-metrics=false   (pas de --enable-ssl-passthrough)
```

L'annotation est donc ignorée. Résultat : le port 443 sert le faux certificat de
nginx, et le port 80 relaie en clair vers l'API server. Or le kubeconfig
distribué aux tenants pointe précisément là :

```
secret vc-vcluster-recette-restore-a-ext → server: https://recette-restore-a.api.preprod.rebuild-it.fr
```

Ce kubeconfig **ne peut pas fonctionner** tel quel (le certificat servi n'est pas
celui du vcluster), ce qui pousse mécaniquement vers `--insecure-skip-tls-verify`
ou vers `http://` — et dans les deux cas le jeton d'admission du tenant circule
en clair ou sans authentification du serveur, sur Internet.

### 3.3 Les droits qui semblent bornés et ne le sont pas — Moyen à Haut

La famille du `list secrets` d'`ingress-nginx`. Vérifié par impersonation :

| Sujet | Ce que dit le ClusterRole | Ce que ça donne |
|---|---|---|
| `ingress-nginx:ingress-nginx` | `secrets: [list, watch]`, pas de `get` | `can-i list secrets -A` → **yes**. Un `list` renvoie les objets complets, `data` comprise. Le `get: no` n'est pas une restriction, c'est un décor. |
| `cattle-fleet-system:helmops` | `secrets: [create, list, watch]` | `can-i list secrets -A` → **yes**, `get` → **no**. Même illusion, plus le `create`. |
| `reflector:reflector` | `secrets: [*]` cluster-wide | tout, lecture et écriture, dans tous les namespaces. C'est ce composant qui propage le wildcard TLS dans chaque namespace tenant. |
| `vcluster-manager:vcluster-manager` | `secrets: [get]` | cf. §1.4 : `get` sans `resourceNames` sur tous les namespaces, avec des noms de secrets déterministes. |

Le sens inverse mérite d'être noté, parce qu'il est contre-intuitif dans l'autre
sens : `resourceNames` **borne** bien un `get`, et il **interdit** le `list` au
lieu de le borner. Vérifié sur le ClusterRole `probe-secret-role`
(`resourceNames: [foo-secret]`, verbes `get,list,watch`) :

```
can-i get  secrets/foo-secret   → yes
can-i get  secrets/autre-secret → no
can-i list secrets              → no      (une requête list n'a pas de nom : la règle ne matche pas)
```

Note : `probe-secret-role` / `probe-secret-binding` / le namespace
`rbac-scratch-test` ont été créés à 16:24 aujourd'hui par un autre chantier en
cours. Ce sont des objets de recette, pas une faille — mais un ClusterRole
d'essai laissé en place est le genre d'objet qui devient un résidu. À nettoyer
par celui qui l'a posé.

Enfin, un binding orphelin :
`vc-vcluster-recette-n6-c-v-vcluster-recette-n6-c` pointe vers un ServiceAccount
d'un namespace supprimé. Ses droits sont anodins (`persistentvolumes: get,list`,
`volumesnapshotcontents`), donc l'impact est faible, mais le mécanisme mérite
d'être noté : un CRB survit à son namespace, et l'opérateur a
`namespaces: create`. Un CRB dormant plus puissant serait un vecteur de reprise
de privilèges par simple recréation d'un namespace et d'un ServiceAccount.
*Remédiation* : que le finalizer de suppression retire aussi les
ClusterRoleBindings du vcluster. *Coût* : quelques lignes et un droit
`clusterrolebindings: delete` de plus pour l'opérateur — un droit puissant à
n'accorder qu'avec `resourceNames`, ce qui marche pour `delete`.

---

## Annexe — ce qui a été exécuté

Toutes les commandes ci-dessous sont des lectures. Rien n'a été créé, modifié ou
supprimé ; aucun mot de passe n'a été testé ; les valeurs de secrets n'ont jamais
été affichées (la comparaison du §1.3 est un test d'égalité en shell, sans écho).

```
export KUBECONFIG=~/GIT/github/vcluster-manager-infra/terraform/kubeconfig

# inventaire
kubectl get clusterrolebindings -o json | jq '… roleRef.name=="cluster-admin" …'
kubectl get ns ; kubectl get ingress -A ; kubectl get netpol -A
kubectl get clusterrole -o json | jq '… secrets / verbes …'

# velero
kubectl -n velero-system get deploy velero -o yaml
kubectl -n velero-system get ds node-agent -o json
kubectl get clusterrolebinding velero-server -o yaml
kubectl -n velero-system get role velero-server -o yaml
kubectl -n velero-system get backups,restores,schedules,backupstoragelocations -o json
kubectl -n velero-system logs deploy/velero --since=6h | grep 'Backing up item'

# rancher
kubectl get users.management.cattle.io -o json
kubectl get globalrolebindings.management.cattle.io -o json
kubectl get clusterrolebinding globaladmin-user-psmxl -o yaml
kubectl get settings.management.cattle.io first-login server-url
kubectl get tokens.management.cattle.io -o json

# droits effectifs, par impersonation
kubectl auth can-i <verbe> <res> [-n ns] --as=system:serviceaccount:<ns>:<sa>
kubectl auth can-i --list --as=…
kubectl auth can-i create pods --subresource=portforward --as=…   # jamais la forme pods/portforward

# config tenant
kubectl -n vcluster-recette-restore-a get secret vc-config-… -o jsonpath='{.data.config\.yaml}' | base64 -d
kubectl -n vcluster-recette-restore-a get secret vc-… -o json | jq '.data|keys[]'   # clés seules

# exposition (requêtes sortantes non authentifiées)
curl -o /dev/null -w '%{http_code}' http://recette-restore-a.api.preprod.rebuild-it.fr/version
openssl s_client -connect recette-restore-a.api.preprod.rebuild-it.fr:443 …
```

Côté dépôts, lectures seules :
`terraform/02-platform/{velero,rancher,variables}.tf`, `terraform.tfstate`
(structure et longueurs, pas les valeurs), `.gitignore`, `git ls-files`,
`git log -S`, et côté application `internal/kubernetes/velero.go`,
`internal/controller/namespace_guard.go`,
`internal/controller/rbac_probe_test.go`,
`docs/design-backup-restore-annotation.md`, `docs/etat-brique-operateur.md`.

### 4.6 Le test symétrique : `baseline` refuse-t-il vraiment ? — prouvé

Le §4.4 laissait un point ouvert : le gain de R-PSA-1 était **déduit** du
fonctionnement de vcluster, pas prouvé. R-PSA-1 ayant été retenue et
`VCLUSTER_POD_SECURITY` passé à `baseline` dans `deploy/base/configmap.yaml`, ce
point est maintenant tranché. Même manœuvre que le §4.2 : un second vcluster
jetable `audit-baseline`, même chart 0.36.1, mêmes values, **une seule ligne
changée** — `podSecurityStandard: baseline` au lieu de `privileged`. Puis le
**manifeste de pod strictement identique**.

**Résultat : oui, refusé, au bon endroit, avec un message utile.**

```
kubectl apply -f pod-priv.yaml          # identité: kubernetes-super-admin, groupe system:masters
  → Error from server (Forbidden): pods "pod-priv-audit" is forbidden:
    violates PodSecurity "baseline:latest": host namespaces (hostPID=true),
    hostPath volumes (volume "hostroot"),
    privileged (container "c" must not set securityContext.privileged=true)
```

Trois points comptent dans cette sortie :

1. **Le refus a lieu à l'admission de l'API du vcluster**, pas à la synchro côté
   hôte. Le pod n'existe nulle part : ni dans le vcluster, ni dans le namespace
   hôte. Vérifié dans les deux.
2. **Le message nomme les trois violations**, une par une, avec le nom du volume
   et celui du conteneur. Un tenant sait quoi corriger sans aide.
3. **`system:masters` n'y échappe pas.** C'est le point qui aurait pu tout ruiner :
   l'identité que le produit distribue au tenant est admin plein du vcluster
   (§4.2), et PSA sait exempter des utilisateurs. Ici il ne l'exempte pas. Le
   refus s'applique bien à celui qu'on cherche à contenir.

Le `--dry-run=server` renvoie la même erreur — le tenant peut valider son
manifeste avant de l'appliquer. C'est le comportement inverse du §4.2, où le
dry-run répondait `configured` sans un mot.

**Nuance à documenter : via un contrôleur, le refus ne s'affiche pas au même
endroit.** Le même pod privilégié déclaré dans un `Deployment` :

```
kubectl apply -f deploy-priv.yaml
  → Warning: would violate PodSecurity "baseline:latest": host namespaces (hostPID=true),
    privileged (container "c" must not set securityContext.privileged=true)
    deployment.apps/deploy-priv-audit created        ← créé quand même
```

Le Deployment est **accepté**, avec un simple avertissement. Aucun pod ne naît
jamais, et l'erreur réelle se lit deux niveaux plus bas :

```
kubectl get deploy deploy-priv-audit
  → 0/1 READY   (indéfiniment)

.status.conditions → ReplicaFailure=True FailedCreate:
   pods "…-c25t9" is forbidden: violates PodSecurity "baseline:latest": …
```

C'est le comportement standard de PSA, pas un défaut de vcluster : l'admission
porte sur les pods, pas sur les gabarits. Il reste sûr — **rien de privilégié
n'atteint l'hôte** — mais un tenant qui déploie par Helm ou GitOps verra un
`Deployment` bloqué à `0/1` sans erreur à l'apply, seulement un avertissement
qu'un pipeline avale souvent. **À écrire dans la doc tenant** : en cas de
`0/1` inexpliqué, lire `.status.conditions[ReplicaFailure]`.

**`baseline` laisse-t-il passer le cas normal ? Oui.** Deux contrôles :

| Cas testé | Résultat |
|---|---|
| Pod ordinaire, aucun `securityContext` | **Créé, `Running`, syncé sur l'hôte** (`pod-normal-audit-x-default-x-audit-baseline`) |
| Pod avec `runAsUser: 0` (root), sans privilège | **Créé, `Running`, syncé** — `baseline` n'est pas `restricted`, il n'interdit pas de tourner en root |

Ce second cas est celui qui aurait cassé la flotte si on avait visé `restricted` :
beaucoup d'images courantes tournent en root sans rien demander de privilégié.
`baseline` les laisse passer et ne bloque que ce qui sert à sortir du conteneur.
Le control-plane du vcluster lui-même démarre normalement sous `baseline`
(syncer, etcd, coredns syncé : tous `Running`), ce qui confirme la lecture du
§4.4 faite alors sur la seule spec.

**Conclusion sur R-PSA-1 : suffisante pour fermer le trou du §4.1, et sans dégât
sur le cas normal.** Le pod qui donnait root sur le nœud est refusé avant
d'exister. Ce qui était déduit est maintenant prouvé.

Deux réserves, qui ne remettent pas en cause la décision :

- **La protection vit dans la config de chaque vcluster.** Un vcluster provisionné
  hors du chemin nominal, ou dont les values sont modifiées, repart sans filet —
  et il n'y a rien côté hôte pour rattraper. C'est l'argument qui garde **R-PSA-2
  (labels PSA sur les namespaces hôtes) utile en défense en profondeur**, pas en
  remplacement. Priorité moindre, mais pas nulle.
- **Les vclusters existants ne sont pas couverts par le changement de
  `configmap.yaml` seul** : `demo`, `recette-restore-a` et `recette-restore-b`
  portent toujours `podSecurityStandard: privileged` dans leur `vc-config-*`
  déployé. Il faut régénérer et réappliquer chaque vcluster pour que la valeur
  prenne effet. Tant que ce n'est pas fait, le §4.1 reste vrai sur la flotte.

Ce qui reste **non testé** : le comportement d'un tenant ayant un besoin
légitimement privilégié (CSI, agent). Aucun n'existe aujourd'hui (§4.4), donc rien
à mesurer — mais la procédure d'exception reste à définir le jour où le cas se
présente.

### 4.7 État du cluster après le second test

Rien ne subsiste. Les trois objets de test (pod privilégié refusé donc jamais
créé, pod ordinaire, pod root, Deployment) supprimés, le chart `audit-baseline`
désinstallé, le namespace supprimé, aucun PV résiduel, port-forward fermé.
`demo` (5 pods), `recette-restore-a` (5), `recette-restore-b` (5),
`vcluster-manager` (2) et `velero-system` (3) tournent comme avant. Aucun objet
de plateforme touché. Seul fichier du dépôt modifié par moi : ce document.
