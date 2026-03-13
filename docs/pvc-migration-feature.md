# Feature : Migration de PVCs entre vclusters via Longhorn Snapshots

## Contexte

La fonctionnalité de migration d'applications ArgoCD entre vclusters (implémentée) ne couvre pas
la migration des données persistantes (PVCs). Ce document décrit le plan d'implémentation et les
points à valider via un POC avant de développer la feature.

## Architecture des PVCs dans vcluster

Le syncer vcluster remonte automatiquement les PVCs créées à l'intérieur du vcluster vers le
cluster hôte. Convention de nommage sur le host :

```
{pvcName}-x-{namespace}-x-{vcName}
```

Exemples observés dans `vcluster-applications` :
```
n8n-claim0-x-n8n-x-vcluster-applications          → PVC "n8n-claim0" dans ns "n8n"
postgresql-pv-v16-x-n8n-x-vcluster-applications   → PVC "postgresql-pv-v16" dans ns "n8n"
pipelines-x-pdf-x-vcluster-applications            → PVC "pipelines" dans ns "pdf"
```

StorageClass utilisée : `longhorn` (driver `driver.longhorn.io`)

## Infrastructure Longhorn Snapshots

### État actuel

- CRDs CSI snapshot installés sur le cluster : `volumesnapshots.storage.k8s.io`,
  `volumesnapshotcontents.storage.k8s.io`, `volumesnapshotclasses.storage.k8s.io`
- Driver Longhorn CSI enregistré : `driver.longhorn.io`
- **`VolumeSnapshotClass` ajoutée** dans fluxprod (`infrastructure/longhorn/config/longhorn_snapshotclass.yaml`)
  sur les branches `preprod` et `master` — déployée via la Kustomization `longhorn-config`

```yaml
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshotClass
metadata:
  name: longhorn-snapshot-vsc
  annotations:
    snapshot.storage.kubernetes.io/is-default-class: "true"
driver: driver.longhorn.io
deletionPolicy: Delete
parameters:
  type: snap
```

### POC à valider avant implémentation

Valider manuellement les étapes suivantes sur le cluster preprod avant de coder la feature :

#### Étape 1 — Créer un VolumeSnapshot d'un PVC applicatif

```yaml
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata:
  name: test-migration-snapshot
  namespace: vcluster-applications   # namespace HOST du vcluster source
spec:
  volumeSnapshotClassName: longhorn-snapshot-vsc
  source:
    persistentVolumeClaimName: n8n-claim0-x-n8n-x-vcluster-applications
```

Vérifier que `status.readyToUse: true` après quelques secondes :
```bash
kubectl get volumesnapshot test-migration-snapshot -n vcluster-applications -w
```

#### Étape 2 — Créer un PVC dans le vcluster cible depuis le snapshot

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: n8n-claim0-x-n8n-x-vcluster-platform   # nom adapté au vcluster cible
  namespace: vcluster-platform                   # namespace HOST du vcluster cible
spec:
  storageClassName: longhorn
  dataSource:
    name: test-migration-snapshot
    kind: VolumeSnapshot
    apiGroup: snapshot.storage.k8s.io
  accessModes:
    - ReadWriteOnce
  resources:
    requests:
      storage: 2Gi   # même taille que le PVC source
```

#### Étape 3 — Vérifier que le vcluster cible voit le PVC

Depuis l'intérieur du vcluster `platform`, vérifier que le PVC `n8n-claim0` dans le namespace `n8n`
est visible et en état `Bound`. Le syncer vcluster devrait le reconnaître automatiquement si le nom
sur le host correspond à la convention `{pvcName}-x-{namespace}-x-vcluster`.

**Point critique** : le vcluster syncer va-t-il binder un PVC pré-existant sur le host, ou va-t-il
en créer un nouveau en ignorant le nôtre ? À valider.

#### Étape 4 — Déployer l'application et vérifier les données

Déployer l'app via ArgoCD sur le vcluster cible et vérifier que les données du PVC source sont
bien présentes.

### Questions ouvertes pour le POC

1. **Binding automatique** : le syncer vcluster détecte-t-il un PVC pré-existant sur le host avec
   le bon nom, ou faut-il forcer le binding via `claimRef` sur le PV ?
2. **AccessModes** : si le PVC source est `RWO`, peut-on faire un snapshot et restaurer en `RWO`
   sur un autre nœud ? (Longhorn le supporte normalement)
3. **Snapshot cross-namespace** : un `VolumeSnapshot` dans `vcluster-applications` peut-il être
   utilisé comme `dataSource` dans `vcluster-platform` ? (normalement non — il faudra peut-être
   passer par un `VolumeSnapshotContent` manuel ou copier le snapshot)
4. **Taille du PVC cible** : doit-elle être identique ou peut-elle être supérieure ?

> ⚠️ Le point 3 est probablement le plus bloquant : les VolumeSnapshots sont namespaced.
> Si cross-namespace n'est pas supporté nativement, il faudra utiliser un `VolumeSnapshotContent`
> pré-provisionné (cluster-scoped) comme intermédiaire.

### Workaround cross-namespace si nécessaire

```yaml
# 1. Récupérer le snapshotHandle du VolumeSnapshotContent lié au snapshot source
# 2. Créer un VolumeSnapshotContent dans le namespace cible pointant vers le même handle
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshotContent
metadata:
  name: migration-snapcontent-target
spec:
  volumeSnapshotClassName: longhorn-snapshot-vsc
  driver: driver.longhorn.io
  deletionPolicy: Retain
  source:
    snapshotHandle: <handle-du-snapshot-longhorn>  # à récupérer depuis le VolumeSnapshotContent source
  volumeSnapshotRef:
    name: migration-snapshot-target
    namespace: vcluster-platform

# 3. Créer un VolumeSnapshot dans vcluster-platform pointant vers ce content
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata:
  name: migration-snapshot-target
  namespace: vcluster-platform
spec:
  volumeSnapshotClassName: longhorn-snapshot-vsc
  source:
    volumeSnapshotContentName: migration-snapcontent-target
```

## Plan d'implémentation (après validation POC)

### Nouveaux fichiers

| Fichier | Rôle |
|---|---|
| `internal/kubernetes/snapshots.go` | Méthodes : CreateSnapshot, WaitSnapshotReady, CreatePVCFromSnapshot, GetSyncedPVCs |
| `internal/handlers/api.go` | Handler `MigratePVCs` — `POST /api/vclusters/{name}/apps/migrate-pvcs` |
| `web/templates/partials/apps_list.html` | UI : détection PVCs + étape de migration dans le formulaire |

### Flow dans l'outil

```
MigrateApp (existant) détecte les PVCs de l'app
  ↓
Affiche liste des PVCs avec tailles dans le formulaire
  ↓
Si l'utilisateur coche "Migrer les données" :
  ↓
  1. Lister PVCs host dans vcluster-{source} filtrés par namespace app
  2. Pour chaque PVC :
     a. CreateVolumeSnapshot dans vcluster-{source}
     b. WaitReadyToUse (poll jusqu'à readyToUse=true, timeout 5min)
     c. Créer VolumeSnapshotContent + VolumeSnapshot dans vcluster-{target} (workaround cross-ns)
     d. Créer PVC dans vcluster-{target} depuis le snapshot
  3. Migrer les manifests ArgoCD (flow existant)
  4. Cleanup snapshots source après confirmation
```

### Données nécessaires dans l'UI

Pour afficher les PVCs dans le formulaire de migration, il faut :
- Le `spec.destination.namespace` de l'Application ArgoCD (déjà disponible dans `ArgoApp.Namespace`)
- Lister les PVCs du host dans `vcluster-{sourceName}` filtrés par `*-x-{namespace}-x-*`
- Récupérer la taille de chaque PVC (`spec.resources.requests.storage`)

### Dépendances Go à ajouter

Aucune nouvelle dépendance — le client `k8s.io/client-go` déjà présent supporte les CRDs
VolumeSnapshot via le dynamic client (déjà utilisé pour HelmRelease et Kustomization).

Les GVR VolumeSnapshot :
```go
snapshotGVR = schema.GroupVersionResource{
    Group:    "snapshot.storage.k8s.io",
    Version:  "v1",
    Resource: "volumesnapshots",
}
snapshotContentGVR = schema.GroupVersionResource{
    Group:    "snapshot.storage.k8s.io",
    Version:  "v1",
    Resource: "volumesnapshotcontents",
}
```
