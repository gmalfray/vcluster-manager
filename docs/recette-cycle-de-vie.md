# Recette preprod — le cycle de vie d'un vcluster, par le parcours réel

> Périmètre : les six parcours du cycle de vie que `recette-n6-namespace.md` n'a pas
> couverts. Cette recette-là ne testait que la séquence de suppression, et elle la
> testait en **posant un CR à la main**. Ici on part de l'écran : l'app commite dans
> fluxprod, FluxCD réconcilie, le HelmRelease se déploie. C'est le contrat de l'app,
> et c'est lui qu'on vérifie.
>
> Rien de ce qui suit n'a jamais tourné sur un cluster réel avant le 2026-08-08.

## Ce que la recette doit trancher

Six questions, une par cas. Chacune a une réponse observable, et l'endroit où la lire.

| Cas | Question | Verdict lu dans |
|---|---|---|
| 0 | Qui fait quoi dans le parcours réel — l'app ou l'opérateur ? | présence/absence d'un `VCluster` CR après une création par l'écran |
| 1 | Une création par le dashboard produit-elle un vcluster qui démarre ? | commit fluxprod → `Kustomization tenant-<nom>` Ready → `HelmRelease` Ready → pods |
| 2 | La mise à jour de la chart vcluster atteint-elle les vclusters ? | `Chart.yaml` sur la branche preprod de platform-helm-charts, puis `status.history[0].chartVersion` du HelmRelease |
| 3 | Un changement de version K8s redéploie-t-il le control plane ? | `values.yaml` du tenant → `HelmRelease` → `kubectl version` **dans** le vcluster |
| 4 | La mise à jour d'ArgoCD embarqué atteint-elle le vcluster ? | `tenant/argocd/kustomization.yaml` (`newTag`) → Kustomization `argocd-<nom>` → image du pod ArgoCD |
| 5 | L'appairage Rancher fonctionne-t-il, et qui le déclenche ? | cluster présent dans l'API Rancher, `state: active`, agents dans le vcluster |
| 6 | Le débranchement Rancher nettoie-t-il des deux côtés ? | cluster absent de Rancher **et** job de cleanup passé dans le vcluster |

Le cas 0 n'est pas un préliminaire administratif : c'est lui qui décide si les cas 5
et 6 testent l'app ou l'opérateur, et la réponse n'est pas celle que la CRD laisse
supposer.

## Règles de sécurité de ce plan

- **Preprod uniquement**, sur l'infra jetable montée pour la recette. Aucun geste ici
  ne se transpose en prod : les cas 2 et 4 modifient des dépôts **partagés par toute
  la cell** (platform-helm-charts, le dépôt ArgoCD), pas un arbre par vcluster.
- **Cible nommée** : `recette-cdv-1`, créé et détruit par cette recette. Elle traverse
  les six cas dans l'ordre. Un second vcluster, `recette-cdv-2`, n'est créé que si un
  cas a besoin d'un support neuf.
- **Ne pas toucher `demo`.** C'est le témoin : il sert à distinguer « ce cas a cassé
  quelque chose » de « la plateforme était déjà cassée ». Les cas 2 et 4 étant
  globaux, `demo` les subit — c'est justement pourquoi on le regarde.
- Aucun kubeconfig, token, mot de passe ou contenu de sauvegarde dans le compte rendu.

## Préconditions

1. `kubectl` admin sur la cell :

   ```bash
   export KUBECONFIG=~/GIT/github/vcluster-manager-infra/terraform/kubeconfig
   kubectl get nodes
   ```
2. **La Kustomization racine de fluxprod est Ready.** À vérifier avant tout, parce
   qu'un `BuildFailed` à la racine fait échouer *toutes* les créations du plan sans
   qu'aucune ne soit en cause :

   ```bash
   kubectl -n flux-system get kustomization flux-system \
     -o jsonpath='{range .status.conditions[?(@.type=="Ready")]}{.status}/{.reason}: {.message}{end}{"\n"}'
   # attendu : True/ReconciliationSucceeded
   ```

   Cette précondition existe parce qu'elle a été violée : la recette N6 a retiré le
   répertoire `clusters/preprod/vclusters/recette-n6-c` sans retirer la ligne
   `- ./vclusters/recette-n6-c` de `clusters/preprod/kustomization.yaml`. Voir
   « Ce que la recette a trouvé », défaut D1.
3. **Le digest réellement exécuté**, app et opérateur. Les deux Deployments sont
   épinglés sur le tag mutable `:main` avec `imagePullPolicy: IfNotPresent` : un
   `rollout restart` ne retire rien, et on mesurerait du vieux code sans le savoir.

   ```bash
   kubectl -n vcluster-manager get pods \
     -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.containerStatuses[0].imageID}{"\n"}{end}'
   ```

   Relever les deux digests **avant** de commencer, et les re-relever après tout
   redémarrage. Un digest qui change en cours de recette invalide les cas déjà joués.
4. **Rancher est activé pour preprod sur cette cell** (cas 5 et 6) :

   ```bash
   kubectl -n vcluster-manager get cm vcluster-manager-config \
     -o jsonpath='{.data.RANCHER_ENABLED_PREPROD}{"\n"}'   # attendu : true
   ```
5. **De la place sur le nœud.** L'infra est un nœud unique (16 vCPU / 32 Gio) partagé
   avec Keycloak, Vault, Rancher, Flux et Velero. Les quotas **par défaut** du
   générateur (8 vCPU / 32 Gio / 500 Gio) saturent la machine à eux seuls — le
   `values.yaml` de `demo` porte déjà un commentaire qui le dit. Créer `recette-cdv-1`
   avec des quotas réduits (2 vCPU / 4 Gi / 20 Gi), sinon le cas 1 mesure la capacité
   du nœud et pas le parcours de création.

### Variables utilisées par les commandes

```bash
export KUBECONFIG=~/GIT/github/vcluster-manager-infra/terraform/kubeconfig
export APP=https://vcluster-manager.rebuild-it.fr
export VC=recette-cdv-1          # la cible
export NS=vcluster-$VC           # son namespace hôte
export FLUXPROD=80239301         # project id GitLab de fluxprod
```

### Piloter l'app

Le parcours attendu est **l'écran**. Si un navigateur pilotable est disponible, jouer
les cas à la souris et le dire. Sinon, le repli est le même chemin HTTP que l'écran
emprunte — mêmes handlers, mêmes gardes, seul le rendu HTMX n'est pas exercé — et il
faut alors l'écrire noir sur blanc dans le compte rendu.

Session et CSRF (double-submit cookie, `internal/auth/csrf.go`) :

```bash
# 1. session (admin local ; le mot de passe vient de terraform output -raw admin_password)
curl -s -c cj -o /dev/null -X POST $APP/auth/local/login \
  --data-urlencode "username=admin" --data-urlencode "password=$PW"
# 2. le cookie csrf_token n'est posé que sur une méthode sûre
curl -s -b cj -c cj -o /dev/null $APP/
CSRF=$(awk '$6=="csrf_token"{print $7}' cj)
# 3. toute écriture porte l'en-tête
curl -s -b cj -H "X-CSRF-Token: $CSRF" -X POST $APP/...
```

### Les quatre commandes de lecture, valables partout

> **Deux noms à ne pas confondre**, corrigés après le passage du 2026-08-08 où ils ont
> fait conclure « pas de HelmRelease » pendant dix minutes : la Kustomization tenant
> s'appelle `tenant-<nom>`, mais le HelmRelease s'appelle **`vcluster-<nom>`**, dans le
> namespace `vcluster-<nom>`.

```bash
# 1. où en est Flux sur ce vcluster
kubectl -n flux-system get kustomization tenant-$VC \
  -o jsonpath='{range .status.conditions[?(@.type=="Ready")]}{.status}/{.reason}: {.message}{end}{"\n"}'

# 2. ce que le HelmRelease a réellement déployé (version de chart comprise)
kubectl -n $NS get helmrelease $NS \
  -o jsonpath='chart={.status.history[0].chartVersion} rev={.status.history[0].version}{"\n"}'

# 3. l'état réel dans le cluster hôte
kubectl -n $NS get pods,sts,pvc

# 4. ce que l'app a écrit dans fluxprod (le contrat de l'app, à vérifier AVANT l'effet)
#    via l'API GitLab, branche preprod
curl -s -H "PRIVATE-TOKEN: $GT" \
  "https://gitlab.com/api/v4/projects/$FLUXPROD/repository/commits?ref_name=preprod&per_page=5"
```

**L'ordre de lecture n'est pas indifférent.** Une action de l'app n'a pas d'effet
immédiat : elle commite, Flux réconcilie (intervalle 5 min à la racine), le
HelmRelease se déploie. Conclure sur l'état du cluster juste après le clic conclut
faux. On lit **d'abord le commit**, ensuite l'état réconcilié, avec une attente
explicite et bornée.

---

## Cas 0 — qui pilote quoi

**Ce qui est testé** : la chaîne réelle entre l'écran et l'opérateur. La CRD, les ADR
et `crd-vcluster.md` décrivent un opérateur qui provisionne, intègre et supprime à
partir d'un `VCluster`. Reste à savoir si le parcours de l'écran en crée un.

1. Avant toute création, relever l'inventaire :

   ```bash
   kubectl get vclusters -A
   ```
2. Jouer le cas 1 (création par le dashboard) en entier.
3. Relever le même inventaire.

**Attendu si la chaîne est branchée** : un `VCluster` nommé `recette-cdv-1` dans
`vcluster-manager`, posé par l'app ou par Flux depuis un fichier commité.

**Attendu si elle ne l'est pas** : aucun CR. Alors *aucune* étape de l'opérateur ne
tourne pour ce vcluster — ni provisionnement, ni intégrations Vault/Keycloak/Rancher,
ni finalizer de suppression — et les cas 5 et 6 testent l'app, pas l'opérateur. Le
noter explicitement : c'est ce qui décide de la lecture des deux derniers cas.

**Ce qui invalide le cas** : rien. Il n'a pas d'échec, il a une réponse. Mais une
réponse « aucun CR » doit être reportée comme un écart entre la doc de conception et
le parcours livré, pas classée en détail d'implémentation.

**Retour arrière** : aucun, c'est une lecture.

---

## Cas 1 — création par le dashboard

**Cible** : `recette-cdv-1`, préprod seule (`scope=preprod`), sans ArgoCD pour ce
premier passage — le cas 4 le rallumera. **Précondition** : aucun vcluster de ce nom,
Kustomization racine Ready, place sur le nœud.

1. Relever l'horodatage et le dernier commit de fluxprod, pour pouvoir attribuer ce
   qui suit :

   ```bash
   date -u +%H:%M:%S
   curl -s -H "PRIVATE-TOKEN: $GT" \
     "https://gitlab.com/api/v4/projects/$FLUXPROD/repository/commits?ref_name=preprod&per_page=1"
   ```
2. Créer par l'écran : `GET /vclusters/new`, remplir, valider. En repli HTTP :

   ```bash
   curl -s -b cj -H "X-CSRF-Token: $CSRF" -X POST $APP/vclusters/new \
     -d "name=$VC" -d "scope=preprod" \
     -d "cpu=2" -d "memory=4Gi" -d "storage=20Gi" \
     -d "rbac_groups=developers" -d "velero_enabled=on" -d "velero_hour=02:00"
   ```
3. **Le premier attendu, et c'est le contrat de l'app** : un commit
   `feat: add vcluster recette-cdv-1` sur la branche `preprod` de fluxprod, qui
   contient l'arbre du tenant **et** la ligne ajoutée dans la kustomization racine :

   ```bash
   curl -s -H "PRIVATE-TOKEN: $GT" \
     "https://gitlab.com/api/v4/projects/$FLUXPROD/repository/tree?ref=preprod&path=clusters/preprod/vclusters/$VC&recursive=true&per_page=100"
   curl -s -H "PRIVATE-TOKEN: $GT" \
     "https://gitlab.com/api/v4/projects/$FLUXPROD/repository/files/clusters%2Fpreprod%2Fkustomization.yaml/raw?ref=preprod" \
     | grep "vclusters/$VC"
   ```

   **Ce qui invalide le cas** : l'arbre est commité mais la ligne de la kustomization
   racine manque. Le vcluster n'existerait alors nulle part pour Flux, et l'écran
   annoncerait quand même un succès. `Create` traite l'échec de
   `kustomizationAction` par un `slog.Warn` et **continue** — c'est précisément la
   moitié silencieuse de ce cas.
4. **Attendre Flux, sans tricher sur la durée.** L'intervalle de la Kustomization
   racine est de 5 min ; celui du tenant aussi. Le chemin complet est donc de l'ordre
   de la dizaine de minutes, pas de la minute. Forcer la réconciliation est autorisé
   **à condition de le dire** — mais alors on ne mesure plus le délai réel :

   ```bash
   # mesure honnête : on attend
   for i in $(seq 40); do
     printf '%s ' "$(date -u +%H:%M:%S)"
     kubectl -n flux-system get kustomization tenant-$VC \
       -o jsonpath='{range .status.conditions[?(@.type=="Ready")]}{.status}/{.reason}{end}' 2>&1
     echo
     sleep 30
   done
   ```
5. **Attendu, dans cet ordre** :
   - `Kustomization tenant-recette-cdv-1` → `True/ReconciliationSucceeded` ;
   - `HelmRelease recette-cdv-1` dans `vcluster-recette-cdv-1` → Ready, avec
     `status.history[0].chartVersion` renseignée ;
   - le StatefulSet du control plane a un pod `Running`, et le PVC est `Bound`.
6. **Le vcluster répond-il ?** C'est la seule preuve qu'il « démarre » :

   ```bash
   kubectl -n $NS get secret vc-$NS-int -o jsonpath='{.data.config}' | base64 -d > /tmp/kc-$VC
   # puis, depuis un pod du cluster hôte ou via port-forward :
   KUBECONFIG=/tmp/kc-$VC kubectl get ns
   ```

   **Ce qui invalide le cas** : le HelmRelease est Ready mais aucun pod ne tourne, ou
   le kubeconfig ne répond pas. Un HelmRelease Ready ne dit que « Helm a installé »,
   pas « le control plane fonctionne ».
7. **La question de la moitié.** Couper le chemin en son milieu et regarder ce qui
   reste : que se passe-t-il si le commit part mais que Flux échoue ? Le relever
   dans l'écran de détail (`GET /vclusters/$VC`) : l'app affiche-t-elle un vcluster
   qui n'existe pas, ou dit-elle qu'il n'est pas déployé ?

**Retour arrière** : cas 7 (suppression), qui est aussi le dernier cas joué.

---

## Cas 2 — mise à jour de la chart vcluster

**Attention, c'est un geste global.** `POST /api/chart/update` ne vise aucun
vcluster : il écrit dans `charts/vcluster/Chart.yaml` sur la branche `preprod` du
dépôt **platform-helm-charts**, et ouvre une MR vers prod. Tous les vclusters de la
cell — `demo` compris — prendront la nouvelle chart au prochain passage de Flux.

**Précondition** : connaître la version courante et une version cible qui existe
réellement dans le dépôt de charts. Une version inventée produirait un échec Helm et
on mesurerait la gestion d'erreur, pas le parcours.

1. Relever l'état de départ, des deux côtés :

   ```bash
   kubectl -n vcluster-demo get helmrelease vcluster-demo \
     -o jsonpath='{.status.history[0].chartVersion}{"\n"}'
   kubectl -n flux-system get gitrepository platform-helm-charts \
     -o jsonpath='{.status.artifact.revision}{"\n"}'
   ```
2. Déclencher depuis le dashboard (le formulaire de mise à jour de chart). En repli :

   ```bash
   curl -s -b cj -H "X-CSRF-Token: $CSRF" -X POST $APP/api/chart/update -d "version=<tag>"
   ```
3. **Premier attendu — le contrat** : un commit sur la branche `preprod` de
   platform-helm-charts touchant `charts/vcluster/Chart.yaml`, et une MR ouverte vers
   la branche prod. L'URL de la MR est dans le message flash rendu par l'app.

   **Ce qui invalide le cas** : le flash annonce un succès sans commit dans le dépôt,
   ou une MR vide.
4. **Deuxième attendu — l'effet.** Attendre que la `GitRepository
   platform-helm-charts` prenne la nouvelle révision, puis que les HelmReleases
   passent à la nouvelle `chartVersion` :

   ```bash
   for i in $(seq 30); do
     printf '%s demo=' "$(date -u +%H:%M:%S)"
     kubectl -n vcluster-demo get helmrelease vcluster-demo -o jsonpath='{.status.history[0].chartVersion}'
     printf ' cdv1='
     kubectl -n $NS get helmrelease $VC -o jsonpath='{.status.history[0].chartVersion}'
     echo; sleep 30
   done
   ```

   **La précaution qui rendrait ce cas aveugle** : s'arrêter au point 3. Vérifier la
   MR est confortable et ne prouve rien — une chart peut être commitée et ne jamais
   se déployer (chart absente du dépôt, valeurs incompatibles, HelmRelease qui
   reste sur son ancienne révision). Le point 4 est le cas ; le point 3 n'en est que
   la moitié.
5. **La question de la moitié.** Une mise à jour de chart qui échoue à mi-chemin
   laisse le dépôt commité et les vclusters sur l'ancienne chart, avec une MR prod
   ouverte qui promet un déploiement qui n'a pas eu lieu en preprod. Relever
   explicitement : l'app signale-t-elle quelque part que le déploiement preprod a
   échoué, ou son écran reste-t-il au vert sur la foi du commit ?

**Retour arrière** : recommiter la version précédente par le même chemin (`POST
/api/chart/update` avec l'ancien tag), et **fermer la MR prod ouverte par le test**.
Une MR de recette laissée ouverte finit par être fusionnée par quelqu'un d'autre.

---

## Cas 3 — changement de version Kubernetes d'un vcluster

**Cible** : `recette-cdv-1`. Contrairement au cas 2, celui-ci est **par vcluster** :
il passe par `POST /vclusters/{name}/settings`, écrit
`vcluster.controlPlane.distro.k8s.version` dans le `values.yaml` du tenant, et laisse
Flux redéployer le control plane.

1. Relever la version courante, **dans le vcluster** et pas seulement dans le fichier :

   ```bash
   KUBECONFIG=/tmp/kc-$VC kubectl version -o json | python3 -c "import json,sys; print(json.load(sys.stdin)['serverVersion']['gitVersion'])"
   ```
2. Soumettre le formulaire de settings avec la nouvelle version. En repli, **tous les
   champs du formulaire doivent être renvoyés** : `UpdateSettings` régénère le
   `values.yaml` entier depuis l'entrée, un champ omis est un champ remis à sa valeur
   par défaut.

   ```bash
   curl -s -b cj -H "X-CSRF-Token: $CSRF" -X POST $APP/vclusters/$VC/settings \
     -d "env=preprod" -d "k8s_version=<version>" \
     -d "cpu=2" -d "memory=4Gi" -d "storage=20Gi" \
     -d "rbac_groups=developers" -d "velero_enabled=on" -d "velero_hour=02:00"
   ```
3. **Premier attendu** : commit sur `preprod` touchant
   `clusters/preprod/vclusters/recette-cdv-1/values.yaml`, avec le bloc

   ```yaml
   distro:
     k8s:
       version: "<version>"
   ```
4. **Deuxième attendu — l'effet, et il faut aller jusqu'au bout** : le HelmRelease
   passe à une nouvelle révision (`status.history[0].version` incrémentée), le pod du
   control plane est recréé, et `kubectl version` **dans le vcluster** rend la
   nouvelle version.

   **Ce qui invalide le cas** : le fichier change, le HelmRelease reste sur la même
   révision. C'est un changement commité que rien n'applique — le pire des deux,
   parce que l'écran le montre comme fait.
5. **La question de la moitié, et c'est le vrai risque de ce cas.** Un changement de
   version K8s redémarre le control plane. S'il échoue (version inexistante, image
   introuvable, données etcd incompatibles), le vcluster est **par terre**, le commit
   est dans fluxprod, et Flux réapplique la mauvaise version toutes les 5 minutes. À
   vérifier, sur `recette-cdv-2` pour ne pas détruire la cible : soumettre une
   version K8s qui n'existe pas, et relever
   - ce que l'app affiche (accepte-t-elle la valeur ? `validateVersion` ne vérifie
     que la forme, pas l'existence),
   - où le vcluster s'arrête (`ImagePullBackOff` du control plane, HelmRelease en
     échec, ou pire : un HelmRelease Ready sur un pod qui ne démarre pas),
   - et si l'écran de détail du vcluster le signale.

**Retour arrière** : resoumettre le formulaire avec la version d'origine (ou le champ
version vide, qui veut dire « le défaut configuré »), attendre le retour du
HelmRelease à Ready et du pod à Running.

---

## Cas 4 — mise à jour d'ArgoCD embarqué dans un vcluster

ArgoCD a **deux** chemins de mise à jour, qui ne font pas la même chose. Les jouer
tous les deux, et ne pas confondre leurs verdicts.

- **Par vcluster** : `POST /vclusters/{name}/settings`, champ `argocd_version` →
  `tenant/argocd/kustomization.yaml`, bloc `images: newTag:`.
- **Global** : `POST /api/argocd/update` → dépôt ArgoCD partagé + MR prod. Même
  nature que le cas 2 : il touche tous les vclusters de la cell.

**Précondition** : `recette-cdv-1` doit avoir ArgoCD activé. Il a été créé sans au
cas 1 — l'allumer ici par le toggle `argocd_toggle=on` du formulaire de settings est
lui-même une partie du test (c'est le chemin « reconfigure complet » de
`UpdateSettings`).

1. Allumer ArgoCD par le formulaire de settings, puis vérifier le contrat :
   - arbre `clusters/preprod/vclusters/$VC/tenant/argocd/` commité,
   - dépôt GitLab d'app-manifests créé,
   - clients OIDC ArgoCD créés dans Keycloak (realm `vcluster-manager`).
2. Attendre la Kustomization `argocd-$VC` et l'installation dans le vcluster :

   ```bash
   kubectl -n $NS get kustomization argocd-$VC \
     -o jsonpath='{range .status.conditions[?(@.type=="Ready")]}{.status}/{.reason}: {.message}{end}{"\n"}'
   KUBECONFIG=/tmp/kc-$VC kubectl -n argocd get pods
   ```
3. Changer la version ArgoCD par le formulaire de settings (`argocd_version=<tag>`).
   **Attendu** : `newTag: <tag>` dans `tenant/argocd/kustomization.yaml`, puis l'image
   réellement tirée par les pods ArgoCD **dans le vcluster** :

   ```bash
   KUBECONFIG=/tmp/kc-$VC kubectl -n argocd get pods \
     -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.containerStatuses[0].imageID}{"\n"}{end}'
   ```

   Lire l'`imageID` et pas l'`image` : le `newTag` peut pointer un tag mutable, et
   c'est exactement le piège du §Préconditions point 3, un étage plus bas.
4. Jouer le chemin global (`POST /api/argocd/update`), vérifier le commit et la MR,
   et **fermer la MR** au retour arrière.

**Ce qui invalide le cas** : `newTag` change dans le fichier mais l'image des pods
ArgoCD ne bouge pas après un cycle Flux complet — le kustomize `images:` ne s'applique
pas à ce qu'on croit.

**Retour arrière** : remettre la version d'origine par le même chemin ; si ArgoCD a
été allumé pour ce cas, l'éteindre (`argocd_toggle=off`) et, seulement si le dépôt
d'app-manifests a été créé par la recette, cocher sa suppression.

---

## Cas 5 — appairage Rancher

**Précondition dure, à trancher au cas 0** : savoir qui est censé appairer.

- L'app (`cmd/server`) a un client Rancher : `RANCHER_URL` et `RANCHER_TOKEN` sont
  dans sa configuration, `PairRancher` est exposé par `POST
  /api/vclusters/{name}/pair-rancher`.
- L'opérateur a l'**étape** (`reconcileRancherPairing`, `vcluster_integrations.go`)
  mais pas le client : son Deployment reçoit le ConfigMap, pas le Secret qui porte
  `RANCHER_TOKEN`.

Donc le geste testable aujourd'hui est celui de l'app. Le vérifier plutôt que le
supposer :

```bash
kubectl -n vcluster-manager get deploy vcluster-manager-operator -o json \
  | python3 -c "import json,sys; c=json.load(sys.stdin)['spec']['template']['spec']['containers'][0]; print(c.get('envFrom'))"
# attendu ici : le configMapRef seul, pas le secretRef
```

1. État de départ, lu dans Rancher lui-même et pas seulement dans l'app :

   ```bash
   kubectl get clusters.management.cattle.io \
     -o custom-columns=NAME:.metadata.name,DISPLAY:.spec.displayName,STATE:.status.conditions[0].type
   ```
2. Appairer depuis l'écran de détail (bouton Rancher). En repli :

   ```bash
   curl -s -b cj -H "X-CSRF-Token: $CSRF" -X POST "$APP/api/vclusters/$VC/pair-rancher?env=preprod"
   ```
3. **Attendu immédiat** : le fragment rendu passe en `Pairing` (le cluster existe
   dans Rancher mais n'est pas encore `active`). L'appairage réel est asynchrone —
   `PairRancher` rend tout de suite et lance le travail en goroutine.
4. **Attendu au bout du compte**, et c'est ce qui compte :
   - un `clusters.management.cattle.io` dont le `spec.displayName` est
     `vcluster-recette-cdv-1`, en `state: active` ;
   - les pods `cattle-cluster-agent` **dans le vcluster** :

     ```bash
     KUBECONFIG=/tmp/kc-$VC kubectl -n cattle-system get pods
     ```
   - l'écran de détail affiche `Paired`.

   **La borne** : Rancher met plusieurs minutes à passer un cluster importé en
   `active`. Attendre au moins 10 minutes avant de conclure à un échec, et le dire si
   on n'attend pas — une fenêtre courte ferait conclure « échec » sur un import qui
   marche.
5. **La question de la moitié.** L'appairage est une goroutine dans le processus de
   l'app. Redémarrer le pod `vcluster-manager` **pendant** l'import, puis relever :
   l'import se termine-t-il quand même (parce qu'il vit dans Rancher, pas dans
   l'app) ? L'écran retrouve-t-il l'état correct après redémarrage, ou reste-t-il sur
   « pas appairé » ? C'est la reprise après redémarrage appliquée à ce parcours.

**Retour arrière** : le cas 6.

---

## Cas 6 — débranchement Rancher

**Cible** : `recette-cdv-1`, appairé par le cas 5.

Le dépairage a lui aussi deux chemins, et ils ne sont pas équivalents :

- **Par l'app** : `POST /api/vclusters/{name}/unpair-rancher` → suppression du
  cluster dans Rancher **et** dépôt d'un job de nettoyage *dans* le vcluster.
- **Par le finalizer de l'opérateur** : `UnpairForDeletion`
  (`internal/service/vcluster_deletion.go`), joué à la suppression d'un `VCluster`.
  Sans client Rancher, il rend `ErrRancherNotConfigured` et l'étape est rapportée
  comme non faite — c'est le comportement voulu, il faut vérifier qu'il est tenu.

1. Dépairer depuis l'écran. En repli :

   ```bash
   curl -s -b cj -H "X-CSRF-Token: $CSRF" -X POST "$APP/api/vclusters/$VC/unpair-rancher?env=preprod"
   ```
2. **Attendu côté Rancher** : le cluster disparaît de
   `clusters.management.cattle.io` (ou passe par `state: removing` avant).
3. **Attendu côté vcluster, et c'est la moitié qu'on oublie** : un job
   `cattle-cleanup` (ou équivalent) est déposé *dans* le vcluster et se termine ; les
   pods `cattle-system` disparaissent.

   ```bash
   KUBECONFIG=/tmp/kc-$VC kubectl -n cattle-system get jobs,pods
   ```

   **Ce qui invalide le cas** : Rancher ne connaît plus le cluster mais les agents
   tournent toujours dedans. Le vcluster essaierait indéfiniment de joindre un
   Rancher qui l'a oublié, et un réappairage serait détecté comme « appairage
   manuel » (`ManuallyPaired`) au lieu de repartir propre.
4. **L'ordre importe, et il est déjà écrit dans le code** : le nettoyage tourne *dans*
   le vcluster, donc il doit être déposé **avant** que le vcluster disparaisse. Le
   vérifier au cas 7 : supprimer un vcluster encore appairé, et regarder si le
   dépairage passe bien en premier (`service.Delete` rend `Async=true` et diffère la
   suppression derrière le job de nettoyage).
5. **Le chemin opérateur, à vérifier séparément.** Poser un `VCluster` CR nommé
   `recette-cdv-2` à la main, l'appairer par l'app, puis supprimer le CR et lire ce
   que le finalizer écrit :

   ```bash
   kubectl -n vcluster-manager get vcluster recette-cdv-2 \
     -o jsonpath='{range .status.conditions[?(@.type=="RancherPaired")]}{.status}/{.reason}: {.message}{end}{"\n"}'
   ```

   **Attendu** : `Unknown` avec une raison qui dit « pas de client Rancher », et le
   message final de la suppression qui **nomme** le cluster resté dans Rancher. Un
   `True`, ou un message de suppression propre, voudrait dire qu'un binaire incapable
   de dépairer rapporte l'étape comme faite.

**Retour arrière** : si le cluster reste dans Rancher, le supprimer depuis l'UI
Rancher ; si les agents restent dans le vcluster, le vcluster est de toute façon
détruit au cas 7.

---

## Cas 7 — remise en état : suppression par le parcours de l'app

Ce cas n'est pas dans la liste des six, mais il est le retour arrière de tous les
autres, et il couvre le trou que `recette-n6-namespace.md` déclarait explicitement :
« la suppression via le parcours de l'app » n'y était pas testée.

1. Supprimer par l'écran (`GET /vclusters/$VC/delete` puis confirmation). En repli :

   ```bash
   curl -s -b cj -H "X-CSRF-Token: $CSRF" -X POST $APP/vclusters/$VC/delete -d "env=preprod"
   ```
2. **Attendu** : commit de retrait dans fluxprod — l'arbre du tenant **et** la ligne
   de la kustomization racine. Le second point est celui que la recette N6 a raté à
   la main ; il faut vérifier que l'app, elle, ne le rate pas.

   ```bash
   curl -s -H "PRIVATE-TOKEN: $GT" \
     "https://gitlab.com/api/v4/projects/$FLUXPROD/repository/files/clusters%2Fpreprod%2Fkustomization.yaml/raw?ref=preprod"
   kubectl -n flux-system get kustomization flux-system \
     -o jsonpath='{range .status.conditions[?(@.type=="Ready")]}{.status}/{.reason}{end}{"\n"}'
   # attendu : True — une référence orpheline casserait TOUTE la cell
   ```
3. **Attendu ensuite** : la Kustomization `tenant-$VC` disparaît, le HelmRelease est
   désinstallé, le namespace hôte part.
4. Vérifier qu'il ne reste rien :

   ```bash
   kubectl get ns $NS                       # attendu : NotFound
   kubectl -n flux-system get kustomization | grep $VC   # attendu : rien
   kubectl get clusters.management.cattle.io | grep $VC  # attendu : rien
   ```

---

## Remise en état après la recette

```bash
for n in recette-cdv-1 recette-cdv-2; do
  kubectl -n vcluster-manager delete vcluster $n --ignore-not-found --wait=false
  kubectl delete ns vcluster-$n --ignore-not-found
  kubectl -n flux-system delete kustomization tenant-$n --ignore-not-found
done
# fluxprod : plus aucune référence aux cibles de recette
curl -s -H "PRIVATE-TOKEN: $GT" \
  "https://gitlab.com/api/v4/projects/$FLUXPROD/repository/files/clusters%2Fpreprod%2Fkustomization.yaml/raw?ref=preprod" \
  | grep recette-cdv    # attendu : rien
kubectl -n flux-system get kustomization flux-system   # attendu : Ready True
# MR prod ouvertes par les cas 2 et 4 : les fermer
velero backup get | grep recette-cdv                   # supprimer les sauvegardes de recette
```

## Grille de verdict

Go pour la suite si, et seulement si :

- cas 1 : commit **et** ligne de kustomization racine **et** vcluster qui répond ;
- cas 3 : la version lue dans le vcluster est celle qui a été demandée ;
- cas 5 : cluster `active` dans Rancher **et** agents dans le vcluster ;
- cas 6 : cluster parti de Rancher **et** agents nettoyés dans le vcluster ;
- cas 7 : la kustomization racine reste Ready après la suppression.

No-Go si l'un de ceux-ci se produit :

- une écriture annoncée en succès par l'écran sans commit correspondant dans fluxprod ;
- un commit dans fluxprod qui casse la kustomization racine (toute la cell tombe) ;
- un dépairage Rancher qui vide l'inventaire Rancher en laissant les agents tourner ;
- une étape d'intégration rapportée comme faite par un binaire qui n'a pas le client
  pour la faire.

## La recette réelle a eu lieu — 2026-08-08

Premier passage du cycle de vie sur cluster réel (k3s v1.32.4, infra jetable). Tout ce
qui suit est **observé**, avec l'heure UTC.

**Comment les cas ont été joués** : par le chemin HTTP des handlers, pas au navigateur —
aucun outil de pilotage de navigateur n'était disponible. Les handlers, les gardes admin
et le CSRF sont donc exercés ; les fragments HTMX, les toasts et le polling de l'écran ne
le sont pas.

| Cas | Verdict | Ce qui a été observé |
|---|---|---|
| 0 | réponse : **aucun CR** | `kubectl get vclusters -A` → `No resources found` après une création complète |
| 1 | ✅ avec réserve | commit + arbre + ligne racine, vcluster qui répond en 1 min ; HelmRelease jamais `Ready` (D2) |
| 2 | ⚠️ moitié | commit preprod parti, MR en échec, **erreur affichée comme si rien n'avait eu lieu** (D3) |
| 3 | ❌ en conditions nominales | commit conforme, jamais appliqué : bloqué par D2. Débloqué à la main → v1.35.7 en 1 min |
| 4 | ✅ | `newTag` → image réellement tirée, installation **et** mise à jour (v2.13.3 → v2.13.4) |
| 5 | ❌ | bloqué par D4 (RBAC), puis par une configuration Rancher de la plateforme |
| 6 | ✅ après D4 | cluster retiré de Rancher, job `rancher-cleanup` terminé en 90 s, agents partis |
| 7 | ✅ | arbre **et** ligne racine retirés, racine restée Ready, namespace parti en 4 min |

### Mesures

| Grandeur | Valeur observée |
|---|---|
| commit → `tenant-<nom>` Ready | **54 s** (16:09:27 → 16:10:20) |
| commit → vcluster qui répond | **~1 min** |
| commit chart → HelmRelease sain à jour | **~1 min** après la révision de la source |
| commit ArgoCD → image basculée | **90 s** (17:09:07 → 17:10:37) |
| suppression → namespace parti | **4 min 30** (17:11:01 → 17:15:31) |
| dépairage → nettoyage terminé | **90 s** (16:49:53 → 16:51:21) |

### D1 — la kustomization racine était cassée depuis la recette N6

Précondition violée avant même de commencer. `clusters/preprod/kustomization.yaml`
portait encore `- ./vclusters/recette-n6-c` alors que le répertoire avait été retiré à la
main. La Kustomization racine était en `BuildFailed` depuis **4 h 26**, donc plus rien de
fluxprod ne réconciliait sur la cell. Réparé par un commit avant de continuer.

La leçon n'est pas « quelqu'un a oublié une ligne » : c'est que le geste manuel n'a pas la
symétrie du geste de l'app. Le cas 7 a vérifié que l'app, elle, retire bien les deux.

### D2 — le Schedule Velero porte le même nom pour tous les vclusters

**Le défaut le plus grave de cette recette.**
`charts/vcluster/templates/velero-schedule.yaml` nomme l'objet
`{{ include "vcluster.name" . }}-backup`, et `vcluster.name` rend `.Chart.Name`, pas
`.Release.Name`. Tous les vclusters écrivent donc le **même** objet :
`Schedule/velero-system/vcluster-backup`.

Trois conséquences, toutes constatées :

1. **Un seul vcluster à la fois a une sauvegarde planifiée.** En fin de recette, l'unique
   Schedule appartenait à `recette-restore-a` avec
   `includedNamespaces: [vcluster-recette-restore-a]`. Il appartenait à `demo` au début.
   `demo` a `veleroBackup.enabled: true` et **n'est plus sauvegardé** — sans un mot nulle
   part.
2. **Aucun HelmRelease de vcluster n'atteint `Ready`** dès qu'il y en a plus d'un. Les
   releases se disputent l'objet, et la correction de drift échoue :
   `Schedule/velero-system/vcluster-backup patch failure: the name of the object
   (vcluster-backup based on URL) was undeterminable: name must be provided`.
   Les events montrent le ping-pong : `DriftDetected` ×19, `DriftCorrectionFailed` ×8,
   puis `removed` / `created` / `configured` en alternance entre releases.
3. **Plus aucun changement de settings n'est appliqué** (voir cas 3).

Causalité prouvée, pas déduite : `driftDetection.mode=disabled` posé à la main sur le
HelmRelease → upgrade en **1 minute** (16:35:44, `UpgradeSucceeded`, v1 → v2) ; Flux a
réverté le patch à 16:37:05 → retour immédiat à `correcting cluster drift`.

Le correctif est dans la chart (`.Release.Name`, ou le namespace cible), pas dans ce dépôt.

### D3 — la mise à jour de chart commite d'abord, ouvre la MR ensuite, et n'annonce que l'échec

`Updater.UpdateChart` commite sur `preprod`, **puis** crée la MR. Sur cette cell le dépôt
de charts n'a qu'une branche `preprod`, donc la MR échoue :

```
Erreur lors de la mise a jour : creating merge request:
POST .../merge_requests: 400 {message: {target_branch: [does not exist]}}
```

L'écran ne montre **que** cette erreur. Pourtant `Chart.yaml` était déjà passé à `0.36.0`
(commit `170e1268`, 16:56:19), et `demo` a été mis à jour en une minute — un changement de
chart sur **toute la cell**, derrière un message qui dit « erreur ».

Ce n'est pas un accident de cette infra : `UpdateK8sVersion` et `UpdateArgoCDVersion`
suivent le même ordre. L'historique du dépôt de charts montre le même demi-succès à 12:16
et 15:55, avant cette recette.

Rejoué à l'identique pour le retour arrière (v0.36.1) : même erreur, même commit réussi.

### D4 — le binaire qui appaire n'a pas le droit dont il a besoin

L'app fait l'appairage et le dépairage Rancher (le cas 0 montre qu'aucun CR n'existe, donc
l'opérateur ne joue aucun rôle). Ces deux gestes passent par un port-forward dans le
vcluster. Or le ClusterRole `vcluster-manager` n'a que `pods: [get, list]`.

Preuve, dans le log de l'app elle-même — pas un `can-i`, l'API server :

```
"could not deploy rancher-cleanup in vcluster"
err="port-forward failed: ... pods \"vcluster-recette-cdv-1-...\" is forbidden:
User \"system:serviceaccount:vcluster-manager:vcluster-manager\" cannot create
resource \"pods/portforward\" in API group \"\" in the namespace \"vcluster-recette-cdv-1\""
```

Pendant ce temps `deploy/base/operator-rbac.yaml` accorde `pods/portforward` à
l'**opérateur**, avec un commentaire qui explique précisément pourquoi il en a besoin —
sur le binaire qui ne fait pas le travail.

Ce qu'un appairage refusé laisse derrière lui : un cluster créé dans Rancher
(`c-bld9w`, `Pending: Unknown`), **aucun agent** dans le vcluster, l'écran bloqué sur
« appairage en cours », et une relance refusée par
« Ce vcluster existe déjà dans Rancher (état: pending). Attendez ou désappairez-le
d'abord. » Attendre ne sert à rien : rien ne reprend l'appairage. La seule sortie est le
dépairage.

Un dépairage refusé est pire : le cluster est retiré de Rancher **avant** le nettoyage, la
suppression du cluster réussit, le nettoyage échoue en `slog.Warn`, et l'écran affiche
« Nettoyage... ». Rancher a oublié le vcluster, les agents tournent toujours dedans.

> **Piège de vérification.** `kubectl auth can-i create pods/portforward` rend `no`
> **même quand le droit est accordé** : la forme qui répond juste est
> `kubectl auth can-i create pods --subresource=portforward`. Ne pas conclure sur la
> première.

Fenêtre de test : Kustomization `vcluster-manager` suspendue, `pods/portforward` ajouté,
cas 5 et 6 rejoués, ClusterRole restauré (`kubectl diff` vide) et Kustomization reprise.

### Cas 5 — ce qui reste bloqué même une fois D4 levé

Le droit accordé, l'appairage va jusqu'au bout de ce que l'app doit faire : cluster
importé (`c-x9b55`), manifeste téléchargé, **appliqué dans le vcluster** (l'agent apparaît
à 16:41:51). L'agent ne démarre pas :

```
level=error msg="unable to read CA file from /etc/kubernetes/ssl/certs/serverca: no such file"
level=error msg="Strict CA verification is enabled but encountered error finding root CA"
```

Cause nommée : `settings.management.cattle.io/agent-tls-mode` est à son défaut `strict`
et `cacerts` est vide sur ce Rancher (v2.14.3). C'est une configuration de la plateforme,
pas un défaut de vcluster-manager, et je n'y ai pas touché.

L'app a renoncé proprement et l'a dit : `"rancher: cluster did not become active"
err="cluster c-x9b55 did not become active within 5m0s"`. Elle le dit **dans son log**
seulement — l'écran, lui, reste sur « appairage en cours ».

### Le repli de `k8sForEnv` — question posée, réponse mesurée

`PairRancher` demande le client `"prod"` en dur, quel que soit l'env. Sur cette
installation la question est sans effet : ni `KUBECONFIG_PREPROD` ni `KUBECONFIG_PROD` ne
sont configurés, donc le repli de `cmd/server/main.go` enregistre **le même** client
in-cluster sous les deux clés (`"k8s client initialized" scope=single-cluster-both-envs`).
`k8sForEnv("prod")` trouve donc une clé existante et le repli « premier client venu » de
`service.go` n'est jamais emprunté.

Ce que la recette ne peut pas trancher, et qu'il faut donc écrire : sur une installation à
deux clusters, `k8sForEnv("prod")` rendrait le client **prod** pour appairer un vcluster
**preprod**. Et si seul `KUBECONFIG_PREPROD` était configuré, le repli rendrait un client
non-nil, donc `ErrRancherK8sProdUnavailable` ne se déclencherait jamais. Les deux branches
sont muettes : aucun log ne dit sur quel cluster le manifeste a été appliqué.

### Ce que la recette a changé sur l'infra, et qu'il faut savoir

- `clusters/preprod/kustomization.yaml` : ligne orpheline `recette-n6-c` retirée (D1).
- `RANCHER_TOKEN` du Secret `vcluster-manager-secrets` : l'ancien était refusé par
  Rancher (`HTTP 401`), rendant les cas 5 et 6 intestables. Un token API a été créé dans
  Rancher (`token-5clwk`, description `recette-cycle-de-vie vcluster-manager`) et posé
  dans le Secret. **Il est laissé en place** : le remettre à l'ancien rendrait la
  fonction Rancher de l'app inopérante. Le Secret n'est pas réconcilié par Flux.
- `Chart.yaml` du dépôt de charts : passé à `0.36.0` puis remis à `0.36.1` par le même
  chemin applicatif.
- ClusterRole `vcluster-manager` et Kustomization `vcluster-manager` : remis à l'identique
  (`kubectl diff` vide, `suspend=false`).

### Deux observations d'environnement, attribuées ailleurs

- **L'API des vclusters n'est pas joignable par son ingress** :
  `x509: certificate is valid for ingress.local`. Rejoué sur `demo` (le témoin) : même
  échec. C'est `ssl-passthrough` demandé par annotation mais non activé sur le contrôleur,
  donc cassé pour toute la cell — pas un défaut du kubeconfig ni de la création. Tous les
  accès aux vclusters de cette recette sont passés par `kubectl port-forward`.
- **Un redémarrage de l'app tue l'appairage en vol.** Un `rollout restart` a été réverté
  par Flux à 16:29:58, ce qui a recréé le pod pendant l'import Rancher lancé à 16:26:40.
  La goroutine est morte avec le process et rien ne l'a reprise. Le symptôme est le même
  que D4 ; c'est un second chemin vers le même état bloqué.
- **Digest vérifié** : `ghcr.io/gmalfray/vcluster-manager@sha256:cbfc94de…` identique
  avant et après redémarrage. Aucune mesure de cette recette ne porte sur du vieux code.

## Ce que ce plan ne couvre pas

- **La prod.** Rien ici ne s'y transpose : `RANCHER_ENABLED_PROD` est à `false` sur
  cette cell, et les cas 2 et 4 y ouvriraient des MR sur des dépôts partagés réels.
- **Le rendu HTMX**, si les cas sont joués par HTTP plutôt qu'au navigateur. Les
  handlers, les gardes admin et le CSRF sont exercés ; les fragments rendus, les
  toasts et le polling de l'écran ne le sont pas.
- **La restauration Velero, la migration d'apps, la protection de namespace, le setup
  Vault** — parcours voisins, hors des six questions posées ici.
- **Le RBAC lecteur vs admin sur chaque écran** : les cas ci-dessus sont tous joués en
  admin. Un passage lecteur est un plan à part.
- **La bascule FluxCD par vcluster** (`fluxcd_toggle`), symétrique de la bascule
  ArgoCD du cas 4, non jouée ici.
