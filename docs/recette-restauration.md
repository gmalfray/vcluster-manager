# Recette preprod — restauration Velero, promotion prod, contenu de sauvegarde

> Périmètre : les parcours qui détruisent des données ou qui touchent la production.
> Restauration Velero in-place et croisée, sauvegarde manuelle et son contenu, promotion
> preprod → prod, édition d'un vcluster prod, migration d'apps ArgoCD.
>
> Le cycle de vie (création, chart, version k8s, ArgoCD, Rancher) est couvert ailleurs :
> `docs/recette-cycle-de-vie.md`. Rien de ce plan ne le rejoue.
>
> **Joué en réel le 2026-08-08** sur la cell de recette (k3s v1.32.4, Flux 2.4.0,
> Velero + MinIO dans `velero-system`, app `ghcr.io/gmalfray/vcluster-manager:main`
> digest `sha256:cbfc94de…`, `VELERO_TRIGGER_MODE=direct`). Les sections
> « Observé le 2026-08-08 » rapportent ce qui s'est passé, pas ce qui était prévu.
>
> **Verdict global : No-Go sur la restauration.** Le détail est en fin de document.

## Ce que la recette doit trancher

| Cas | Question | Verdict lu dans |
|---|---|---|
| A | Une sauvegarde faite par l'app contient-elle les données du vcluster ? | `PodVolumeBackup` dans `velero-system` |
| B | Une restauration in-place depuis cette sauvegarde rend-elle le vcluster à son état d'avant ? | un objet témoin posé dans le vcluster avant la sauvegarde |
| C | Et depuis une sauvegarde qui contient réellement les volumes ? | phase du `Restore`, et le témoin |
| D | Une restauration croisée met-elle les données de la source dans la cible ? | contenu du vcluster cible, et de son namespace hôte |
| E | Le contenu d'une sauvegarde est-il refusé à un lecteur côté serveur ? | code HTTP, pas l'affichage |
| F | « Supprimer le backup » supprime-t-il quelque chose ? | l'objet `Backup` trois minutes après |
| G | Promotion preprod → prod et édition d'un prod en attente | MR GitLab, relecture des quotas |
| H | Migration d'apps ArgoCD entre vclusters | non joué — voir la section |

## Règles de sécurité de ce plan

- **Preprod uniquement.** Les cas A à F détruisent un volume pour de bon. Aucun ne se
  joue sur un vcluster qui porte quoi que ce soit.
- Chaque cas nomme sa cible. Les vclusters jetables sont `recette-restore-a` (source et
  cible in-place), `recette-restore-b` (cible croisée) et `recette-prod-1` (portée prod).
  Ils sont créés par la recette et détruits à la fin. **`demo` n'est jamais touché** : il
  sert de témoin pour dire si un dégât vient de la recette ou d'ailleurs.
- Le cas C redémarre le pod `velero` pour toute la cell. Prévenir avant : pendant le
  blocage qu'il provoque, **aucune restauration ne démarre sur la plateforme**.
- Le cas D dépose un second plan de contrôle dans le namespace de la cible. Le nettoyage
  est décrit avec le cas, et il n'est pas optionnel.
- Aucun kubeconfig, token, mot de passe ni contenu de sauvegarde ne va dans le compte rendu.

## Préconditions

1. `kubectl` admin sur la cell, l'app joignable en HTTPS, un compte admin et un compte
   lecteur.

   ```bash
   export KUBECONFIG=~/GIT/github/vcluster-manager-infra/terraform/kubeconfig
   kubectl -n vcluster-manager get deploy vcluster-manager
   ```

2. **Vérifier le digest réellement exécuté**, pas le tag. Le Deployment est épinglé sur
   `:main` avec `imagePullPolicy: IfNotPresent` : un `rollout restart` ne retire rien du
   registre et la recette mesurerait du vieux code sans le dire.

   ```bash
   kubectl -n vcluster-manager get pods \
     -o jsonpath='{range .items[*]}{.metadata.name}{"  "}{.status.containerStatuses[0].imageID}{"\n"}{end}'
   ```

3. Le mode de déclenchement, parce qu'il change qui exécute la séquence :

   ```bash
   kubectl -n vcluster-manager get cm vcluster-manager-config -o jsonpath='{.data.VELERO_TRIGGER_MODE}{"\n"}'
   ```

   `direct` : l'app fait la séquence dans la requête HTTP. `annotation` : elle pose une
   annotation et l'opérateur la joue. **Ce plan a été joué en `direct`.** Le chemin
   `annotation` n'est pas couvert (voir « Ce que ce plan ne couvre pas »).

4. Velero et son node-agent debout, et un `BackupStorageLocation` disponible :

   ```bash
   kubectl -n velero-system get pods
   kubectl -n velero-system get backupstoragelocation default \
     -o jsonpath='{.status.phase}{"\n"}'   # attendu : Available
   ```

5. Un accès au vcluster lui-même, pour poser et relire le témoin. Sur cette cell,
   l'ingress `<nom>.api.<domaine>` ne fonctionne pas (voir le défaut D6) : passer par un
   port-forward.

   ```bash
   kubectl -n vcluster-<nom> port-forward svc/vcluster-<nom> 18443:443 &
   kubectl -n vcluster-<nom> get secret vc-vcluster-<nom> -o jsonpath='{.data.config}' \
     | base64 -d | sed 's|https://localhost:8443|https://127.0.0.1:18443|' > /tmp/kc-<nom>.yaml
   ```

### Les commandes de lecture, valables partout

```bash
# 1. la sauvegarde contient-elle des volumes, ou seulement des manifestes ?
kubectl -n velero-system get podvolumebackups \
  -l velero.io/backup-name=<backup> \
  -o custom-columns='POD:.spec.pod.name,VOL:.spec.volume,STATUS:.status.phase,BYTES:.status.progress.bytesDone'

# 2. où en est la restauration, et combien de temps a-t-elle pris
kubectl -n velero-system get restore <restore> -o jsonpath='
phase : {.status.phase}
items : {.status.progress.itemsRestored}/{.status.progress.totalItems}
début : {.status.startTimestamp}  fin : {.status.completionTimestamp}{"\n"}'

# 3. le volume a-t-il vraiment été remplacé (l'UID change, pas le nom)
kubectl -n vcluster-<nom> get pvc data-vcluster-<nom>-etcd-0 \
  -o jsonpath='{.metadata.uid} {.metadata.creationTimestamp}{"\n"}'

# 4. l'état dans lequel la séquence a laissé Flux
kubectl -n vcluster-<nom> get helmrelease vcluster-<nom> -o jsonpath='suspend={.spec.suspend}{"\n"}'
kubectl -n flux-system get kustomization tenant-<nom> -o jsonpath='suspend={.spec.suspend}{"\n"}'
```

### Le témoin, et pourquoi il n'est pas négociable

Une restauration qui « se termine » ne prouve rien. Ce qui prouve, c'est un objet posé
dans le vcluster **avant** la sauvegarde et relu **après** la restauration. Sans lui,
une séquence qui restaure un volume vide passe tous les contrôles d'état.

```bash
KUBECONFIG=/tmp/kc-<nom>.yaml kubectl create ns recette-temoin
KUBECONFIG=/tmp/kc-<nom>.yaml kubectl -n recette-temoin \
  create configmap temoin-restore --from-literal=marqueur=avant-backup
```

---

## Cas A — la sauvegarde de l'app contient-elle les données ?

**Cible** : `recette-restore-a`. **Préconditions** : vcluster debout, témoin posé.

1. Déclencher une sauvegarde depuis l'app, en admin :

   ```bash
   curl -X POST -b cookies -H "X-CSRF-Token: $CSRF" \
     "$BASE/api/vclusters/recette-restore-a/velero/backup?env=preprod"
   ```

   Noter le nom rendu par le toast (`manual-<nom>-<millis>`).
2. Attendre la fin, puis lire **deux** choses, pas une :

   ```bash
   kubectl -n velero-system get backup <backup> \
     -o jsonpath='{.status.phase} {.status.progress.itemsBackedUp}{"\n"}'
   kubectl -n velero-system get podvolumebackups -l velero.io/backup-name=<backup>
   ```

   **Attendu** : phase `Completed`, **et au moins un `PodVolumeBackup` pour le pod etcd**,
   avec des octets transférés.

   **Ce qui invalide le cas** : `Completed` sans aucun `PodVolumeBackup`. La sauvegarde ne
   contient alors que des manifestes — dont un `PersistentVolumeClaim` vide. C'est le
   piège « vert sans rien exécuter » appliqué à la donnée.
3. Contre-épreuve, à faire une fois pour lever le doute sur l'installation Velero :
   refaire la même sauvegarde à la main **sans exclure les pods**, et comparer.

   ```bash
   cat <<'EOF' | kubectl apply -f -
   apiVersion: velero.io/v1
   kind: Backup
   metadata: {name: temoin-avec-pods, namespace: velero-system}
   spec:
     includedNamespaces: [vcluster-recette-restore-a]
     defaultVolumesToFsBackup: true
     snapshotVolumes: false
     storageLocation: default
     ttl: 24h0m0s
     excludedResources: ["events", "leases"]
   EOF
   ```

**Observé le 2026-08-08 — ÉCHEC.** La sauvegarde de l'app termine `Completed`, 127 items,
**zéro `PodVolumeBackup`, zéro `DataUpload`**. La même sauvegarde sans l'exclusion des
pods produit six `PodVolumeBackup`, dont `vcluster-recette-restore-a-etcd-0 / data` à
**144 801 792 octets**. La donnée etcd est capturable ; elle ne l'est pas par le chemin de
l'app.

```
$ kubectl -n velero-system get backup manual-recette-restore-a-1786205874675 \
    -o jsonpath='{.spec.excludedResources} phase={.status.phase} items={.status.progress.itemsBackedUp}'
["events","leases","pods","replicasets.apps"] phase=Completed items=127
$ kubectl -n velero-system get podvolumebackups
No resources found in velero-system namespace.
```

Cause : `internal/kubernetes/velero.go`, `CreateVeleroBackup` exclut `pods` et
`replicasets.apps`. La sauvegarde de volume par système de fichiers passe par les pods
qui montent le volume ; sans pods dans la sauvegarde, Velero ne crée aucun
`PodVolumeBackup`. Le commentaire du code explique pourquoi l'exclusion a été ajoutée
(les init containers `restore-wait` échouent sur un `runAsNonRoot`) — le remède a
supprimé la sauvegarde en même temps que le symptôme.

Deux conséquences à ne pas séparer :

- une sauvegarde de l'app ne restaure rien ;
- le `Schedule` de la plateforme (`vcluster-backup`, hors app), lui, n'exclut pas les
  pods et capture bien les volumes. Les sauvegardes automatiques et manuelles n'ont donc
  pas le même contenu, et rien dans l'UI ne les distingue.

**Retour arrière** : aucun, la sauvegarde n'est pas destructive.

---

## Cas B — restauration in-place depuis une sauvegarde de l'app

C'est le parcours annoncé aux utilisateurs. Il suspend Flux, descend le vcluster à zéro,
**supprime le PVC**, crée le `Restore`, puis reprend Flux.

**Cible** : `recette-restore-a`. **Préconditions** : cas A joué, témoin posé **avant** la
sauvegarde, et un second marqueur posé **après** pour distinguer « restauré » de « jamais
touché ».

1. Poser le second marqueur :

   ```bash
   KUBECONFIG=/tmp/kc-recette-restore-a.yaml kubectl -n recette-temoin \
     create configmap temoin-apres-backup --from-literal=marqueur=apres-backup
   ```
2. Ouvrir l'observation dans un second terminal — la séquence dure quelques secondes et
   un échantillonnage à 10 s la rate entièrement :

   ```bash
   while :; do
     printf '%s hr=%s ks=%s deploy=%s pvc=%s uid=%s\n' "$(date -u +%H:%M:%S)" \
       "$(kubectl -n vcluster-recette-restore-a get helmrelease vcluster-recette-restore-a -o jsonpath='{.spec.suspend}')" \
       "$(kubectl -n flux-system get kustomization tenant-recette-restore-a -o jsonpath='{.spec.suspend}')" \
       "$(kubectl -n vcluster-recette-restore-a get deploy vcluster-recette-restore-a -o jsonpath='{.spec.replicas}')" \
       "$(kubectl -n vcluster-recette-restore-a get pvc data-vcluster-recette-restore-a-etcd-0 -o jsonpath='{.status.phase}')" \
       "$(kubectl -n vcluster-recette-restore-a get pvc data-vcluster-recette-restore-a-etcd-0 -o jsonpath='{.metadata.uid}' | cut -c1-8)"
     sleep 2
   done
   ```
3. Lancer la restauration :

   ```bash
   curl -X POST -b cookies -H "X-CSRF-Token: $CSRF" \
     "$BASE/api/vclusters/recette-restore-a/velero/restore?env=preprod&backup=<backup>"
   ```
4. **Attendu** : PVC détruit puis recréé (UID différent), `Restore` en `Completed`, Flux
   repris, vcluster de nouveau `1/1`, **et le témoin `temoin-restore` présent, le
   marqueur `temoin-apres-backup` absent**.

   **Ce qui invalide le cas** : le témoin manque. La séquence a détruit le volume sans le
   remplacer.
5. Vérifier que le vcluster **fonctionne**, pas seulement qu'il tourne :

   ```bash
   curl -b cookies "$BASE/api/vclusters/recette-restore-a/kubeconfig?env=preprod" > /tmp/kc.yaml
   # puis, via port-forward :
   KUBECONFIG=/tmp/kc.yaml kubectl get ns
   ```

**Observé le 2026-08-08 — ÉCHEC, avec perte de données.**

- La séquence s'est exécutée : PVC `43266d74…` supprimé, PVC `ce1530f1…` créé à
  16:21:19. Le point destructeur marche.
- Le `Restore` est passé `Completed` **en une seconde** (16:21:19 → 16:21:20), 56 items.
  Une seconde pour un vcluster de 5 Gio : rien de volumineux n'a transité.
- Flux a été repris, le vcluster est remonté `1/1` à 16:21:46. Du point de vue de l'UI et
  des objets Flux, tout est vert.
- **Les deux témoins ont disparu.** L'etcd est reparti vide, et le namespace
  `recette-temoin` n'est jamais revenu.
- **Le vcluster n'est plus joignable par personne.** Le kubeconfig téléchargé depuis
  l'app *après* la restauration est refusé :

  ```
  tls: failed to verify certificate: x509: certificate signed by unknown authority
  # et, en ignorant la vérification :
  error: You must be logged in to the server (Unauthorized)
  ```

  L'autorité présentée par l'apiserver (empreinte `4BEA4AB2…`) ne correspond plus à celle
  des secrets du namespace (`446CF1FD…`), que Velero vient de réécrire avec la version
  sauvegardée. Le vcluster a régénéré sa PKI sur un volume vide pendant que la
  restauration remettait l'ancienne dans les secrets.
- L'app ne dit rien de tout cela. La carte d'état affiche `HelmRelease: Progressing`,
  `Kustomization: Ready`, `Vault Auth: Ready` — elle lit des objets Flux, jamais
  l'apiserver du vcluster.

**Retour arrière** : il n'y en a pas pour les données, elles sont perdues. Pour rendre le
vcluster utilisable :

```bash
kubectl -n vcluster-<nom> delete secret vc-vcluster-<nom> vc-vcluster-<nom>-ext vc-vcluster-<nom>-int
kubectl -n vcluster-<nom> annotate helmrelease vcluster-<nom> \
  reconcile.fluxcd.io/requestedAt="$(date +%s)" --overwrite
```

Vérifié : le vcluster remonte et son kubeconfig refonctionne, **vide**.

---

## Cas C — restauration in-place depuis une sauvegarde qui contient les volumes

Le cas B ne dit pas si la mécanique de restauration est bonne, seulement que la
sauvegarde était creuse. Ce cas sépare les deux.

**Cible** : `recette-restore-a`. **Précondition** : une sauvegarde faite à la main sans
exclure les pods (cas A point 3), phase `Completed`, avec ses `PodVolumeBackup`.

1. Lancer la restauration in-place sur cette sauvegarde-là.
2. Suivre `Restore` **et** `PodVolumeRestore` :

   ```bash
   kubectl -n velero-system get restore <restore> -o jsonpath='{.status.phase}{"\n"}'
   kubectl -n velero-system get podvolumerestores \
     -o custom-columns='POD:.spec.pod.name,VOL:.spec.volume,STATUS:.status.phase'
   ```
3. **Attendu** : `Completed`, chaque `PodVolumeRestore` en `Completed`, témoin de retour.

**Observé le 2026-08-08 — ÉCHEC, blocage.**

- Six `PodVolumeRestore` créés, **aucun n'a jamais eu de phase**.
- Le `Restore` est resté `InProgress` indéfiniment. Logs Velero :

  ```
  Waiting for all pod volume restores to complete
  Failed to check node-agent pod status, disengage
    error="pods \"vcluster-recette-restore-a-d58875f8c-cmrcl\" not found"
  ```

  La restauration de volume attend le pod nommé dans la sauvegarde. Ce pod n'existe plus :
  la séquence de l'app l'a supprimé en descendant le vcluster à zéro, et Velero a ensuite
  restauré le `Deployment` avec `existingResourcePolicy: update`, ce qui **a annulé la
  mise à l'échelle à zéro** et fait naître un pod portant un autre nom.
- **Flux est resté suspendu** (`tenant-recette-restore-a` à `suspend=true`) pendant tout
  le blocage. L'app ne reprend qu'à la fin de la restauration, et il n'y a pas de fin.
- L'UI, elle, est honnête : elle affiche « Restauration en cours… (Flux suspendu) —
  Phase : InProgress » et continue de sonder. Le défaut « faux succès » de la 1.4.0 ne
  s'est pas reproduit. Elle n'a en revanche aucune borne : elle tournerait ainsi
  jusqu'aux deux heures du garde-fou côté service.
- `kubectl delete restore` ne suffit pas à débloquer : l'objet garde son finalizer
  `restores.velero.io/external-resources-finalizer` et reste `InProgress`.
- **Le pire est là** : tant que ce `Restore` occupe le contrôleur, **plus aucune
  restauration ne démarre sur la plateforme**. Celle du cas D, créée à 16:33, est restée
  sans statut jusqu'au redémarrage de Velero à 16:35.

**Retour arrière** — obligatoire, dans cet ordre :

```bash
kubectl -n velero-system patch restore <restore> --type=merge -p '{"metadata":{"finalizers":[]}}'
kubectl -n velero-system rollout restart deploy/velero        # rend la main au contrôleur
kubectl -n flux-system patch kustomization tenant-<nom> --type=merge -p '{"spec":{"suspend":false}}'
kubectl -n vcluster-<nom> patch helmrelease vcluster-<nom> --type=merge -p '{"spec":{"suspend":false}}'
```

Aucun de ces gestes n'a d'équivalent dans l'app. `AbortInPlaceRestore` et
`InspectInterruptedRestore` existent dans `internal/service/velero.go` mais ne sont
câblés que sur le chemin opérateur (`internal/controller/veleroops_controller.go`). En
`VELERO_TRIGGER_MODE=direct`, **il n'y a aucun bouton pour sortir un vcluster resté
suspendu** ; il faut un `kubectl` d'admin de la cell.

---

## Cas D — restauration croisée vers un autre vcluster

**Cibles** : source `recette-restore-a`, cible `recette-restore-b`. **Préconditions** :
les deux vclusters debout, un témoin propre à `b` posé dedans pour savoir si `b` survit.

1. Poser le témoin de `b`, et relever l'état de son namespace hôte :

   ```bash
   kubectl -n vcluster-recette-restore-b get all,helmrelease,pvc,resourcequota
   ```
2. Lancer la restauration croisée :

   ```bash
   curl -X POST -b cookies -H "X-CSRF-Token: $CSRF" \
     "$BASE/api/vclusters/recette-restore-a/velero/restore?env=preprod&backup=<backup>&target=recette-restore-b"
   ```
3. **Attendu par un utilisateur qui lit le libellé** : les données de `a` se retrouvent
   dans `b`.
4. **Ce qu'il faut vérifier** :

   ```bash
   kubectl -n vcluster-recette-restore-b get all,helmrelease,secret,cm,pvc,resourcequota,kustomization \
     -o name | grep restore-a
   KUBECONFIG=/tmp/kc-recette-restore-b.yaml kubectl get ns   # le témoin de b est-il là ?
   ```

**Observé le 2026-08-08 — ÉCHEC, et dégât collatéral sur la source.**

- Le témoin de `b` est intact et son vcluster fonctionne : **rien des données de `a`
  n'est arrivé dans le vcluster `b`**. La restauration croisée ne fait pas ce que son
  libellé promet.
- À la place, elle a déposé **24 objets** nommés `vcluster-recette-restore-a` dans le
  namespace hôte de `b` : un `Deployment` de plan de contrôle (en `CrashLoopBackOff`),
  un `StatefulSet` etcd (en marche), un `PersistentVolumeClaim`, six `Service`, les
  secrets de PKI et de kubeconfig, le `HelmRelease`, et **une seconde `ResourceQuota**
  `vc-vcluster-recette-restore-a` dans le namespace de `b`. Elle a aussi créé trois
  `Kustomization` Flux (`cert-manager-…`, `vault-webhook-…`) dans le namespace de `b`,
  qui appliquent des ressources dans celui de `a`.
- Le namespace de `b` s'est retrouvé à faire tourner deux plans de contrôle sur le
  quota d'un seul.
- **En nettoyant ce dépôt, la source a été détruite.** Le `HelmRelease` cloné est arrivé
  avec le secret de release Helm de `a` ; le supprimer dans le namespace de `b` a déclenché
  une désinstallation Helm qui a effacé le `Deployment`, le `StatefulSet`, les `Service`
  et les secrets **dans le namespace de `a`**. Il n'est resté que le PVC et les
  `HelmRelease`. Enchaînement établi par corrélation (les objets disparaissent dans la
  minute qui suit la suppression, helm-controller est le seul acteur à en avoir le droit
  et le manifeste), **pas prouvé par une trace explicite**.
- Risque du même ordre, non joué : le `HelmRelease` cloné porte les libellés Flux
  d'origine. Un ramasse-miettes de `kustomize-controller` pourrait le supprimer tout seul
  et produire la même destruction sans qu'un humain touche à quoi que ce soit. À vérifier
  dans une prochaine passe.

**Retour arrière** — long, et à faire objet par objet :

```bash
for k in kustomization helmrelease deployment statefulset replicaset service secret \
         configmap serviceaccount role rolebinding pvc ingress resourcequota; do
  kubectl -n vcluster-<cible> get $k -o name 2>/dev/null | grep '<source>' \
    | xargs -r kubectl -n vcluster-<cible> delete
done
# puis remonter la source, qui a probablement été désinstallée :
kubectl -n vcluster-<source> annotate helmrelease vcluster-<source> \
  reconcile.fluxcd.io/requestedAt="$(date +%s)" --overwrite
```

Ne pas oublier les `Kustomization` : elles ne sont pas dans `kubectl get all` et se
faufilent dans le nettoyage.

---

## Cas E — contenu d'une sauvegarde, et le lecteur

**Cible** : n'importe quelle sauvegarde `Completed`.

1. En **admin** :

   ```bash
   curl -b cookies-admin "$BASE/api/vclusters/<nom>/velero/backups/<backup>/content?env=preprod"
   ```

   **Attendu** : 200, du JSON indenté.
2. En **lecteur**, la même requête, et les autres écritures du domaine. Vérifier le code
   HTTP, pas l'affichage : un bouton masqué n'est pas un contrôle d'accès.

**Observé le 2026-08-08 — PASS.** Réponses obtenues avec une session Keycloak
`testreader` (« Test Reader ») :

| Route | Lecteur |
|---|---|
| `GET …/velero/backups` | 200 (lecture seule, normal) |
| `GET …/velero/backups/<b>/content` | **403** |
| `POST …/velero/backup` | **403** |
| `POST …/velero/restore` | **403** |
| `DELETE …/velero/backups/<b>` | **403** |
| `POST …/create-prod-mr` | **403** |
| `POST …/apps/migrate` | **403** |

Une écriture sans en-tête `X-CSRF-Token`, session admin valide : **403**.

Deux remarques sur le contenu lui-même, sans conséquence de sécurité mais qui corrigent
ce que dit le code :

- ce que rend la route est la **liste de ressources** de la sauvegarde — des noms
  d'objets, y compris des noms de `Secret`, **pas leurs valeurs**. Le commentaire de
  `GetVeleroBackupContent` parle d'un « raw resource dump, secrets included » : c'est
  faux, et ça fait croire à une exposition plus grave qu'elle n'est. Le garde admin reste
  justifié — la liste décrit la topologie du tenant.
- l'exclusion `events` ne porte que sur `v1/Event` ; `events.k8s.io/v1/Event` est bien
  présent dans le contenu. Cosmétique, mais la sauvegarde est plus grosse que prévu.

**Retour arrière** : aucun, tout est en lecture.

---

## Cas F — « Supprimer le backup » supprime-t-il quelque chose ?

**Cible** : une sauvegarde jetable.

1. Supprimer depuis l'app, en admin :

   ```bash
   curl -X DELETE -b cookies -H "X-CSRF-Token: $CSRF" \
     "$BASE/api/vclusters/<nom>/velero/backups/<backup>?env=preprod"
   ```
2. **Ne pas s'arrêter au toast.** Attendre **trois minutes**, puis relire :

   ```bash
   kubectl -n velero-system get backup <backup> -o jsonpath='{.metadata.creationTimestamp}{"\n"}'
   kubectl -n velero-system get deletebackuprequests
   ```

   **Attendu** : `NotFound`, et les données parties du bucket.

   **Ce qui invalide le cas** : l'objet est revenu, avec un `creationTimestamp` postérieur
   à la suppression.

**Observé le 2026-08-08 — ÉCHEC.** Sauvegarde supprimée à 16:38:24, toast « Backup
temoin-avec-pods-restore-a supprimé ». À 16:44:55, l'objet est de retour avec
`creationTimestamp: 2026-08-08T16:41:07Z` — recréé par le contrôleur de synchronisation
de Velero depuis le stockage objet. **Aucun `DeleteBackupRequest` n'a jamais existé.**

`DeleteVeleroBackup` (`internal/kubernetes/velero.go`) supprime l'objet Kubernetes. La
suppression d'une sauvegarde Velero passe par un `DeleteBackupRequest` — c'est ce que
fait `velero backup delete` — sans quoi les données restent dans le bucket et l'objet
réapparaît. Un exploitant qui supprime une sauvegarde pour se conformer à une durée de
rétention croit l'avoir fait ; il ne l'a pas fait.

**Retour arrière** : sans objet, rien n'a été supprimé.

---

## Cas G — promotion preprod → prod, et édition d'un prod en attente

**Cible** : `recette-prod-1`, créé en portée `prod`, jamais déployé (« en attente »).

1. Créer le vcluster en portée `prod`, puis ouvrir sa fiche :

   ```bash
   curl -b cookies "$BASE/vclusters/recette-prod-1?env=prod"
   ```

   **Attendu** : « Ce vcluster n'est pas encore déployé en production. Les modifications
   sont appliquées directement sur la branche preprod. »
2. Éditer les quotas, puis relire :

   ```bash
   curl -X POST -b cookies -H "X-CSRF-Token: $CSRF" \
     "$BASE/vclusters/recette-prod-1/settings?env=prod" -d "cpu=3" -d "memory=4Gi" -d "storage=20Gi" -d "rbac_groups=it"
   curl -b cookies "$BASE/api/vclusters/recette-prod-1/quotas?env=prod"
   ```
3. Demander la MR de promotion :

   ```bash
   curl -X POST -b cookies -H "X-CSRF-Token: $CSRF" \
     "$BASE/api/vclusters/recette-prod-1/create-prod-mr?env=prod"
   ```
4. **Ouvrir la MR et lire son diff.** Ne pas la fusionner.
5. Supprimer le vcluster (prod en attente → commit direct) :

   ```bash
   curl -X POST -b cookies -H "X-CSRF-Token: $CSRF" \
     "$BASE/vclusters/recette-prod-1/delete" -d "env=prod" -d "confirm_name=recette-prod-1"
   ```

**Observé le 2026-08-08 — PASS, avec une réserve.**

- Édition d'un prod en attente : « Paramètres mis à jour », relecture `cpu=3`. Le commit
  direct sur `preprod` fonctionne. Vérifié en relisant par l'app, pas en inspectant le
  dépôt.
- `create-prod-mr` rend une MR `…/fluxprod/-/merge_requests/3`, **idempotent** : rappelé,
  il rend la même. Le champ de confirmation de suppression s'appelle `confirm_name`, pas
  `confirm` — un mauvais nom rend « Le nom ne correspond pas », donc la garde tient.
- Suppression d'un prod en attente : le vcluster disparaît de la liste prod. Commit direct,
  pas de MR, conforme à ce qui est annoncé.
- **Réserve** : la MR est une promotion `preprod → master` **globale**. Elle n'est pas
  filtrée sur le vcluster depuis lequel on a cliqué : elle emporte tout ce qui est en
  attente sur `preprod`, y compris les changements d'autres personnes. La description
  générée le laisse entendre à demi-mot (« Ce MR contient des fichiers sous
  `clusters/preprod/` **et** `clusters/prod/` »), mais le bouton se lit comme « promouvoir
  ce vcluster ». Sur une plateforme partagée, cliquer met en production le travail des
  autres. À arbitrer : soit le libellé change, soit la MR est construite par vcluster.
- **Non testé** : le cas « prod déjà déployé → MR automatique à chaque édition ». Il
  demande une cell prod qui réconcilie `master` ; la cell de recette n'en a pas.

**Retour arrière** : la suppression du point 5. La MR reste ouverte — elle est
idempotente et ne s'auto-fusionne pas, mais elle est à fermer à la main si la recette
n'est pas suivie d'une promotion réelle.

---

## Cas H — migration d'apps ArgoCD

**Non joué.** Aucun vcluster de la cell n'a ArgoCD, et en créer un demande un dépôt
`app-manifests-<nom>` peuplé plus le temps de déploiement d'ArgoCD — hors du budget de
cette fenêtre. Ce qui a été vérifié se limite aux gardes de la route :

- `GET /api/vclusters/<nom>/apps` sur un vcluster sans ArgoCD : « Aucune Application
  ArgoCD trouvée », pas d'erreur ;
- cible `../evil` : « Nom invalide : doit commencer par une lettre, uniquement [a-z0-9-] »,
  refusée **avant** tout appel GitLab ;
- cible inexistante `nexistepas` : le message porte sur la source
  (« Erreur liste fichiers source : project app-manifests-<source> not found »), pas sur
  la cible. L'existence de la cible n'est donc pas contrôlée avant que la source ne soit
  lue. Pas un trou de sécurité, mais un message qui envoie chercher au mauvais endroit.

Ce qui reste entier à recetter : la migration elle-même (commit dans le dépôt de la
cible, suppression optionnelle de la source, et surtout ce qui reste si la seconde étape
échoue après la première — le chemin `DeleteSourceFailed`, qui laisse l'app dans les deux
vclusters).

---

## Défauts trouvés, par ordre de gravité

| # | Défaut | Preuve |
|---|---|---|
| D1 | La sauvegarde de l'app ne contient aucune donnée de volume (`pods` exclus → pas de `PodVolumeBackup`) | cas A |
| D2 | La restauration in-place détruit le volume et le remplace par du vide ; le vcluster revient sans données et **injoignable** (PKI désynchronisée) | cas B |
| D3 | Une restauration in-place depuis une sauvegarde complète reste bloquée `InProgress`, laisse Flux suspendu, et **bloque toutes les restaurations de la plateforme** jusqu'au redémarrage de Velero | cas C |
| D4 | La restauration croisée ne restaure rien dans le vcluster cible ; elle clone le plan de contrôle de la source dans le namespace de la cible, quota compris, et son nettoyage détruit la source | cas D |
| D5 | « Supprimer le backup » ne supprime rien : pas de `DeleteBackupRequest`, l'objet revient en trois minutes, les données restent dans le bucket | cas F |
| D6 | Le kubeconfig distribué par l'app ne fonctionne pas : `ssl-passthrough` est demandé par l'ingress du vcluster mais `ingress-nginx` n'est pas lancé avec `--enable-ssl-passthrough`, donc nginx termine le TLS avec son certificat `ingress.local` | `kubectl -n ingress-nginx get pods -o jsonpath='{.items[0].spec.containers[0].args}'` — le drapeau est absent ; côté client `certificate is valid for ingress.local`. **Défaut d'infra, pas de l'app** |
| D7 | En `VELERO_TRIGGER_MODE=direct`, aucune route ne permet de sortir un vcluster resté suspendu ; `AbortInPlaceRestore` n'est câblé que sur le chemin opérateur | lecture de `cmd/server/main.go` et `internal/controller/veleroops_controller.go` |
| D8 | La MR « promotion prod » est globale `preprod → master`, pas limitée au vcluster depuis lequel on la demande | cas G |
| D9 | Le commentaire de `GetVeleroBackupContent` annonce un dump avec les secrets ; la route rend une liste de noms | cas E |

D2, D3 et D4 laissent chacun un vcluster dans un état qu'aucun écran de l'app ne montre
et qu'aucun bouton ne répare.

## Ce que la recette confirme comme corrigé

Les trois défauts de la 1.4.0 visés par ce passage étaient marqués corrigés dans le code
sans avoir jamais tourné. Résultat sur cluster réel :

- **RBAC du ServiceAccount** — corrigé et exercé. `SetFluxSuspend`, `ScaleVClusterWorkloads`
  et `DeleteVClusterPVC` se sont exécutés sans `forbidden`. C'est ce qui rend la séquence
  destructrice réellement destructrice — elle ne l'était pas en 1.4.0, et c'est ce qui
  masquait D1 et D2.
- **Topologie** — corrigée. La détection vise bien le `StatefulSet` etcd et
  `data-vcluster-<nom>-etcd-0` : l'UID du PVC change à chaque restauration.
- **Remontée d'échec** — pas de faux succès observé. Sur le blocage du cas C, l'UI affiche
  « Restauration en cours… (Flux suspendu) — Phase : InProgress » et continue de sonder ;
  elle ne dit jamais « Flux repris ». Ce qui manque n'est plus un mensonge, c'est une
  borne : rien ne signale au bout de N minutes que ça ne finira pas.

## Grille de verdict

Go sur la restauration si, et seulement si :

- cas A : au moins un `PodVolumeBackup` pour le pod etcd, avec des octets transférés ;
- cas B : le témoin posé avant la sauvegarde est relu après la restauration, et le
  kubeconfig distribué par l'app fonctionne ;
- cas C : `Restore` en `Completed`, tous les `PodVolumeRestore` en `Completed`, Flux repris
  sans intervention ;
- cas D : les données de la source sont dans le vcluster cible, et rien de la source n'est
  posé dans le namespace de la cible ;
- cas F : la sauvegarde supprimée ne revient pas.

**Aucun de ces cinq n'est rempli.** Verdict : **No-Go**.

No-Go ferme tant que l'un de ceux-ci tient :

- une sauvegarde `Completed` sans `PodVolumeBackup` (D1) — c'est la racine : tout le reste
  du parcours est correct et détruit quand même ;
- une restauration in-place qui rend un vcluster injoignable (D2) ;
- un `Restore` bloqué qui gèle le contrôleur Velero pour toute la plateforme (D3) ;
- une restauration croisée qui écrit dans le namespace de la cible autre chose que les
  données restaurées (D4).

## Ce que ce plan ne couvre pas

- **Le chemin `VELERO_TRIGGER_MODE=annotation`**, où l'opérateur joue la séquence à partir
  du marqueur `VClusterVeleroOps`. La cell est en `direct`. Tout ce qui est écrit ici sur
  la remontée d'état (`ResumePending`, `VolumeDestroyed`, reprise après redémarrage de
  l'opérateur) n'a pas été exercé. D1 et D4 sont indépendants du mode — ils vivent dans
  `internal/kubernetes/velero.go` — mais D3 et D7 pourraient se comporter autrement.
- **La reprise après redémarrage de l'app** en pleine séquence : `InspectInterruptedRestore`
  n'a pas été déclenché. Le cas qui compte — tuer le pod de l'app entre la suppression du
  PVC et la création du `Restore` — n'a pas été joué.
- **La restauration d'un vcluster qui porte une vraie charge** : le témoin est un
  `ConfigMap`. Un tenant avec des PVC applicatifs à l'intérieur du vcluster ajoute une
  couche que rien ici ne touche.
- **La migration d'apps ArgoCD** (cas H) au-delà de ses gardes d'entrée.
- **Le cas « prod déployé → MR automatique »** : pas de cell prod sur cette infra.
- **La prod.** Rien de ce document ne s'y transpose. Les cas A à F détruisent.
- **Le mécanisme exact de D4** : la destruction de la source par la désinstallation Helm
  est établie par corrélation, pas par une trace. Et l'hypothèse qu'un ramasse-miettes
  Flux produirait le même effet sans geste humain n'a pas été vérifiée — elle mérite de
  l'être avant de conclure que le cas D est seulement « sale ».

## Remise en état après la recette

```bash
export KUBECONFIG=~/GIT/github/vcluster-manager-infra/terraform/kubeconfig

# 1. plus aucun Restore ne doit être InProgress
kubectl -n velero-system get restores -o custom-columns='NAME:.metadata.name,PHASE:.status.phase'

# 2. plus aucune Kustomization ni HelmRelease suspendue
kubectl get kustomizations -A -o custom-columns='NS:.metadata.namespace,NAME:.metadata.name,SUSPEND:.spec.suspend'

# 3. aucun objet d'un vcluster dans le namespace d'un autre
for ns in $(kubectl get ns -o name | grep '^namespace/vcluster-' | sed 's#namespace/##'); do
  kubectl -n "$ns" get all,helmrelease,kustomization,resourcequota -o name 2>/dev/null \
    | grep -v "${ns#vcluster-}" | sed "s|^|$ns : |"
done

# 4. supprimer les vclusters jetables par l'app, pas par kubectl
#    (la suppression est un parcours à part entière, et kubectl saute le GitOps)

# 5. sauvegardes de recette : les retirer avec velero, pas avec kubectl delete (cas F)
velero backup delete filet-avec-pods-restore-a temoin-avec-pods-restore-a
```

**État laissé le 2026-08-08** : `demo` intact (jamais touché). `recette-restore-a` remonté
et joignable, mais **vide** — ses données ont été perdues au cas B et ne sont pas
récupérables. `recette-restore-b` intact, son témoin en place, débarrassé du plan de
contrôle parasite et des trois `Kustomization` du cas D. `recette-prod-1` supprimé par
l'app. Velero redémarré une fois. Aucune `Kustomization` ni `HelmRelease` laissée
suspendue. Sauvegardes `filet-avec-pods-restore-a` et `temoin-avec-pods-restore-a`
laissées en place : elles portent les seules données etcd capturées de la journée et
servent de pièce à conviction pour D1 et D5.
