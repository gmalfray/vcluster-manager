# Design — déclenchement backup/restore Velero piloté par annotation

> Étude, pas d'implémentation. Statut : proposition à valider (ne touche pas à l'ADR-001, ne
> dépend pas de la maturité de kro ni du POC controller-runtime sur le chemin de suppression).
> Portée : comment faire passer `TriggerVeleroBackup` et `CreateVeleroRestore`
> (`internal/service/velero.go`) d'un appel HTTP synchrone à un déclenchement déclaratif par
> annotation, consommé par un reconciler.

## 0. Reco en une phrase

Annotation-trigger façon Flux (`requestedAt` + nonce), portée par un **petit CRD marqueur dédié**
(`VClusterBackupOps` / `VClusterRestoreOps`, un par vcluster, co-localisé dans son namespace) —
**pas** par le namespace du vcluster lui-même (qui n'a pas de sous-ressource `status`
extensible) et **pas** par la future CRD `VCluster` (qui n'existe pas encore et est couplée à la
décision kro/generator.go). Le contrôleur qui la réconcilie appelle telles quelles les méthodes
de `internal/service/velero.go` — aucune logique métier réécrite. Détail et alternatives
ci-dessous.

## 1. Pourquoi une annotation, pas un champ de spec

Aujourd'hui le déclenchement est impératif : le handler HTTP appelle directement
`Service.TriggerVeleroBackup` / `Service.CreateVeleroRestore`, qui parlent à Velero via
`internal/kubernetes/velero.go`. Ça marche, mais ça ne survit pas à un redémarrage du process — le
suivi d'une restauration in-place vit dans une map en mémoire
(`veleroResumeStates`, `resumeAfterInPlaceRestore` lancé en goroutine avec un timeout de 2h) et un
poll HTMX côté navigateur. C'est exactement le genre d'état ad hoc que `docs/adr-001-source-de-verite.md`
et `docs/crd-vcluster.md` §3.2bis pointent déjà comme fragile pour la suppression (`vaultStates`,
`startCleaningReconciler`) — le même diagnostic s'applique ici.

Le point de départ de ce document : un backup ou une restauration n'est **pas un état désiré**. Un
`VCluster.spec.velero.enabled/hour/ttl` (déjà dans le schéma CRD de `crd-vcluster.md` §2.2) décrit
une politique permanente — « sauvegarder tous les jours à 3h, garder 30 jours » — et ça, oui, ça
appartient au `spec`, commité en Git, réconcilié en continu. Mais « fais un backup maintenant » ou
« restaure ce backup précis » est un ordre one-shot, sans rapport avec l'état désiré du vcluster :
une fois exécuté, il n'y a plus rien à réconcilier, il n'y a qu'un résultat à rapporter. Le mettre
dans `spec` poserait deux problèmes concrets :

- **Committer en Git une action ponctuelle n'a pas de sens** : il faudrait un commit pour
  déclencher, puis un autre pour « annuler » le déclencheur une fois traité (sinon le champ reste
  positionné en Git indéfiniment, ambigu entre « en cours » et « déjà fait la semaine dernière »).
  À l'inverse, `docs/adr-001-source-de-verite.md` explique déjà pourquoi le CR `VCluster` doit
  rester un objet de vingt lignes lisible en revue — un champ qui change de valeur à chaque clic
  UI casserait cette lisibilité.
- **Le contrôleur ne doit pas re-déclencher un backup à chaque reconcile** : un champ de `spec`
  observé par un reconciler classique redéclenche l'action à chaque passage tant qu'il reste
  positionné, sauf logique ad hoc pour ne le faire qu'une fois — c'est précisément ce que le
  pattern annotation résout nativement.

Le pattern Flux (`reconcile.fluxcd.io/requestedAt` sur `Kustomization`/`HelmRelease`/`GitRepository`)
répond à ce besoin exact : l'annotation porte une valeur qui change à chaque demande (un
timestamp RFC3339 fait un nonce lisible et ordonnable), le contrôleur compare cette valeur à la
dernière traitée, enregistrée en `status`. Trois propriétés en découlent directement :

- **Idempotent** : la même valeur d'annotation vue deux fois (relecture après un redémarrage du
  contrôleur, double reconcile) ne redéclenche rien — `status.lastHandledRequestedAt` fait foi.
- **One-shot par construction** : il faut une *nouvelle* valeur pour re-déclencher, jamais la même
  ; contrairement à un champ de `spec` avec un bool `triggerBackup: true`, pas besoin de logique
  de remise à zéro.
- **Hors GitOps, par construction** : une annotation posée par un `kubectl patch`/`PATCH` API
  n'est pas commitée nulle part. C'est un ordre transitoire sur un objet vivant, pas une
  modification de l'état désiré versionné — cohérent avec le fait que backup/restore n'ont jamais
  été dans `internal/gitops/generator.go` aujourd'hui (contrairement au `Schedule` Velero
  automatique, qui lui *est* généré dans `values.yaml.tmpl` via `VeleroSchedule` et reste en
  spec/GitOps, sans changement proposé ici).

## 2. Où vit l'annotation

Trois options, dans l'ordre où le prompt les pose.

### Option A — sur le namespace `vcluster-{name}`

Ce namespace existe déjà pour chaque vcluster et porte déjà une annotation gérée par
l'application : `protect-deletion`, posée/retirée par `SetNamespaceProtection`
(`internal/kubernetes/protection.go:20-37`, `Get` → mutation des annotations → `Update`). Réutiliser
le même objet pour le trigger backup/restore serait cohérent avec ce précédent, et ne demande
**aucun nouveau type** — ça marche dès aujourd'hui, avant tout CRD.

Le problème : **`Namespace` n'a pas de sous-ressource `status` extensible**. Son `status` natif
(`.status.phase`, `.status.conditions`) a un schéma Kubernetes figé (`v1.NamespaceCondition`,
prévu pour le cycle de vie de suppression du namespace lui-même), on ne peut pas y ajouter
`lastBackupName`, `restorePhase`, `resumePending`, etc. La seule façon de « publier » un résultat
sur le namespace serait de le remettre en annotations — ce qui n'est plus un `status` au sens
Kubernetes (pas de sous-ressource distincte, pas de séparation RBAC spec/status, pas de garantie
optimistic-concurrency propre à `/status`). Ça enfreint directement la règle du projet
(« toujours des sous-ressources status + conditions typées ») — pas une préférence de style, une
règle absolue de ce chantier. Éliminé pour cette raison, malgré sa simplicité.

### Option B — une ressource marqueur dédiée

Un petit CRD, un par vcluster, dont le seul rôle est de porter les annotations de déclenchement et
un `status` en bonne et due forme. Proposition : `VClusterBackupOps` et `VClusterRestoreOps` (ou
un seul `VClusterVeleroOps` couvrant les deux — trancher au moment du contrôleur, pas ici), namespaced,
créé dans `vcluster-{name}` au même moment que le namespace lui-même (ou paresseusement à la
première annotation si on veut éviter de toucher `Service.Create`).

Avantages :
- **Sous-ressource `status` réelle**, conditions typées — respecte la règle absolue.
- **Découplé de la CRD `VCluster`** et donc de la décision kro/controller-runtime pas encore
  validée par POC (`docs/crd-vcluster.md` §7) — ce chantier n'attend pas la maturité de kro ni le
  reste du schéma `VClusterSpec`.
- **Colocalisé dans le namespace du vcluster** : suppression du namespace = suppression du
  marqueur, pas de `ownerReference` à maintenir à la main.
- Donne un **premier vertical slice controller-runtime** concret et à faible risque — pas de
  finalizer, pas d'irréversibilité de `deletionTimestamp` (voir `docs/crd-vcluster.md` §4.2), donc
  un bac à sable raisonnable pour valider client-go, RBAC du contrôleur, sous-ressource `status`,
  requeue/backoff, avant de les réutiliser sur le chemin bien plus délicat de la suppression. Ce
  n'est pas un remplacement du POC suppression déjà décidé dans `docs/crd-vcluster.md` §3.2bis (qui
  reste le POC prioritaire pour valider le modèle « controller-runtime remplace les goroutines
  hand-rollées ») — plutôt un complément qui peut avancer en parallèle, sur un risque strictement
  inférieur.

Inconvénient assumé : un type Kubernetes de plus à faire vivre, potentiellement provisoire si la
CRD `VCluster` finit par absorber ce rôle (§10).

### Option C — sur la future CRD `VCluster`

Le « chez-soi » naturel à terme : `docs/crd-vcluster.md` prévoit déjà `status.lastBackup.*` et
`status.deletion.*` sur le même objet. Mais cette CRD n'existe pas — ni le type Go
(`api/v1alpha1/vcluster_types.go`), ni son contrôleur, ni la décision kro validée par POC. La lier
maintenant au déclenchement backup/restore couplerait un chantier simple et isolé à un chantier
bien plus large et pas encore tranché en implémentation. À écarter **pour maintenant**, pas pour
toujours (§10 détaille l'absorption prévue).

### Comparatif

| | Namespace (A) | Ressource marqueur (B) | CRD `VCluster` (C) |
|---|---|---|---|
| Existe déjà | ✅ | ❌ (à créer, petit) | ❌ (gros chantier, pas tranché) |
| `status` sous-ressource conforme | ❌ | ✅ | ✅ (à terme) |
| Dépend du POC kro / decision generator.go | Non | Non | Oui |
| Précédent dans le code | `protect-deletion` | Aucun | Aucun |
| Risque d'implémentation | Faible mais viole une règle absolue | Faible | Élevé (couplé à un chantier plus vaste, pas prêt) |

**Reco : Option B maintenant, migration vers Option C prévue et sans perte à l'étape 2** (§10).

## 3. Schéma de l'annotation backup

Sur `VClusterBackupOps` (namespace `vcluster-{name}`, nom = `{name}`) :

```yaml
metadata:
  annotations:
    backup.vcluster.rebuild-it.fr/requestedAt: "2026-08-06T14:32:00Z"   # RFC3339, change à chaque demande
    backup.vcluster.rebuild-it.fr/ttl: "720h0m0s"                        # optionnel, sinon cfg.VeleroDefaultTTL
    backup.vcluster.rebuild-it.fr/storage-location: ""                   # optionnel, sinon défaut Velero
```

Reconcile côté contrôleur :
1. Lire l'annotation `requestedAt`. Si absente ou égale à `status.backup.lastHandledRequestedAt` →
   rien à faire (idempotence, §5).
2. Sinon, appeler `Service.TriggerVeleroBackup(ctx, actor, name, env)` (signature inchangée,
   §7) — le paramètre `ttl` supplémentaire suppose une petite extension de la méthode pour accepter
   un TTL explicite au lieu du seul `s.cfg.VeleroDefaultTTL` codé en dur
   (`internal/service/velero.go:541`), ou un second appel dédié `TriggerVeleroBackupWithTTL` — détail
   d'implémentation à trancher au moment du contrôleur, pas ici.
3. Écrire `status.backup.lastHandledRequestedAt`, `lastBackupName`, `lastPhase` (`New` immédiatement,
   puis pollé jusqu'à un état terminal — même logique que `GetVeleroBackupPhase`), condition
   `BackupCompleted` (`True`/`False`/`Unknown`).

## 4. Schéma de l'annotation restore

```yaml
metadata:
  annotations:
    restore.vcluster.rebuild-it.fr/requestedAt: "2026-08-06T15:00:00Z"   # RFC3339, change à chaque demande
    restore.vcluster.rebuild-it.fr/from-backup: "manual-demo-1754489523000"  # requis
    restore.vcluster.rebuild-it.fr/target: "demo-restored"                    # optionnel ; vide/= name → in-place
    restore.vcluster.rebuild-it.fr/requested-by: "gmalfray"                   # optionnel, traçabilité kubectl
```

Reconcile :
1. Comparer `requestedAt` à `status.restore.lastHandledRequestedAt` — idempotence.
2. **Refuser une nouvelle demande tant qu'une restauration précédente n'a pas atteint une phase
   terminale** (`Completed`/`Failed`/`PartiallyFailed`, `isTerminalRestorePhase` existe déjà,
   `internal/service/velero.go:75-77`). C'est une garantie **nouvelle** par rapport à
   l'aujourd'hui : rien dans `CreateVeleroRestore` n'empêche actuellement deux appels concurrents
   de lancer chacun leur séquence de scale-down/suppression de PVC sur le même vcluster. Le
   contrôleur, en comparant l'état courant avant d'agir, ferme ce trou — condition
   `RestoreRejectedBusy` avec le nom de la restauration en cours.
3. Sinon, appeler `Service.CreateVeleroRestore(ctx, actor, name, env, fromBackup, target)` **sans
   aucune modification de sa séquence interne** — c'est le point le plus important de ce document :
   suspend Flux → scale à 0 → attente de terminaison des pods → suppression du PVC → point de
   non-retour (`pvcDeleted`) → création du `Restore` Velero, avec les sentinelles `ErrRestoreStageFailed`
   (avant `pvcDeleted`, best-effort `abortInPlaceRestore` qui reprend Flux) et
   `ErrRestoreStageFailedVolumeGone` (après, on laisse volontairement suspendu — voir
   `internal/service/velero.go:218-344`). Cette logique ne bouge pas : c'est exactement la matière
   que la fiche de mission demande de ne pas casser.
4. Écrire `status.restore.stage` à **chaque étape** de la séquence (`FluxSuspended`, `ScaledDown`,
   `PVCDeleted`, `RestoreCreated`) — c'est ce qui remplace le rôle que jouait
   `resumeAfterInPlaceRestore` en mémoire : si le contrôleur redémarre entre deux étapes, il relit
   `status.restore.stage` et sait où il en était, au lieu de perdre le fil comme une goroutine tuée
   par un restart le ferait aujourd'hui.
5. Une fois le `Restore` Velero créé, le reconciler continue de requeue (par ex. toutes les 10s,
   même cadence que le ticker actuel de `resumeAfterInPlaceRestore`) tant que la phase n'est pas
   terminale, en pollant `GetRestoreStatus` — puis, en cas de restauration in-place, tente la
   reprise de Flux (`SetFluxSuspend(ctx, name, false)`, idempotent) jusqu'à ce qu'elle réussisse.
   C'est un remplacement direct de la boucle `resumeAfterInPlaceRestore` + `veleroResumeStates`
   (`internal/service/velero.go:359-445`) : la boucle de reconcile controller-runtime fait ce que
   cette goroutine fait à la main, mais de façon *level-triggered*, avec requeue natif et sans état
   en mémoire de process — exactement l'argument déjà posé dans `docs/adr-001-source-de-verite.md`
   pour la suppression, transposé ici au restore.

Status résultant :

```yaml
status:
  restore:
    lastHandledRequestedAt: "2026-08-06T15:00:00Z"
    restoreName: "demo-restore-abc123"
    fromBackup: "manual-demo-1754489523000"
    target: ""              # vide = in-place
    inPlace: true
    stage: RestoreCreated   # survit au redémarrage, §4 point 4
    phase: InProgress       # New/InProgress/Completed/Failed/PartiallyFailed
    resumePending: false
    resumeFailed: false
    resumeError: ""
    volumeDestroyed: false  # miroir de phase==Failed && inPlace, comme aujourd'hui
  conditions:
    - type: RestoreInProgress
      status: "True"
    - type: FluxResumePending
      status: "False"
```

Ces champs sont un report direct de `VeleroRestoreStatusView`
(`internal/service/velero.go:461-471`) — le contrôleur ne réinvente pas le contrat, il l'écrit en
`status` au lieu de le retourner en JSON/HTML à une requête HTTP.

## 5. Idempotence, dedup, asynchronisme, status

- **Dedup** : `status.*.lastHandledRequestedAt` face à `metadata.annotations[...requestedAt]` —
  identique en substance au patron Flux `Reconciler.lastHandledReconcileAt`. Une annotation relue
  deux fois (redémarrage du contrôleur, double webhook d'admission, reconcile forcé) ne redéclenche
  rien.
- **Asynchronisme** : Velero est intrinsèquement async (un `Backup`/`Restore` objet Kubernetes que
  Velero réconcilie de son côté). Le contrôleur vcluster-manager ne bloque jamais dans son
  `Reconcile()` en attendant une phase terminale — il **crée**, note l'étape, et **requeue** (soit
  via `RequeueAfter`, soit passivement via un watch sur l'objet Velero lui-même si le manager
  controller-runtime est configuré pour le surveiller — détail d'implémentation). C'est le même
  modèle que Flux applique déjà à ses propres `HelmRelease`.
- **Status subresource** : chaque écriture de `status` passe par le client `/status` dédié
  (`UpdateStatus`, pas `Update` sur l'objet entier) — évite un conflit d'écriture avec un éventuel
  patch d'annotation concurrent sur `metadata`, et respecte la séparation RBAC (qui peut modifier
  les annotations vs qui peut modifier le status ne sont pas la même question — l'UI/le service
  patch les annotations, seul le contrôleur écrit le status).
- **Concurrence entre deux demandes de restore** : traitée en §4 point 2 (rejet explicite tant
  qu'une restauration précédente n'est pas terminale) — un gain net par rapport à l'existant, pas
  juste une reprise à l'identique.

## 6. Qui pose l'annotation

L'UI (via `internal/handlers`, et demain `internal/api` REST — pas encore mergé sur `main`, voir §8)
ne parle plus directement à Velero. Elle **patch l'annotation** sur `VClusterBackupOps` /
`VClusterRestoreOps`, via une nouvelle paire de méthodes service, par exemple
`Service.RequestVeleroBackup(ctx, actor, name, env, ttl)` et
`Service.RequestVeleroRestore(ctx, actor, name, env, fromBackup, target)`, qui :
1. Font **le même contrôle admin** que `TriggerVeleroBackup`/`CreateVeleroRestore` aujourd'hui
   (`if !actor.IsAdmin { return ErrForbidden }`) — le contrôle d'autorisation ne bouge pas de place,
   il se fait toujours dans `internal/service`, avant que quoi que ce soit touche Kubernetes.
2. Font la **même validation** (`validBackupName`, `validName` sur `target` —
   `internal/service/velero.go:29-34`, `261-263`).
3. **Journalisent l'audit immédiatement** (`audit.LogActor(...)`) — l'audit ne se déplace pas dans
   le contrôleur, il reste au moment où l'humain a demandé l'action, avec son identité réelle. Le
   contrôleur, lui, journalise séparément l'exécution technique (voir §7, acteur système) — deux
   lignes d'audit complémentaires : « qui a demandé, quand » et « ce que l'opérateur a exécuté,
   quand ».
4. Patch l'annotation via Server-Side Apply, `FieldManager: "vcluster-manager"` — même patron déjà
   utilisé ailleurs dans le code (`internal/kubernetes/vcluster_access.go:562`, pour l'application de
   manifests dans un vcluster) — et renvoient tout de suite une vue "Requested"/202, sans attendre
   la fin de l'opération : exactement le comportement déjà observé aujourd'hui côté UI (l'appel HTTP
   revient déjà avec `Phase: "New"` avant que Velero ait terminé, et l'UI poll ensuite — voir §8, le
   changement de plomberie ne change quasiment rien à l'expérience déjà en place).

### RBAC — qui peut annoter = qui peut déclencher

Point à ne pas manquer : dans ce projet, le contrôle admin ne repose **pas** sur le RBAC natif
Kubernetes aujourd'hui. Les utilisateurs humains ne touchent jamais l'API Kubernetes directement —
ils s'authentifient sur vcluster-manager (OIDC/local, `internal/auth/oidc.go`, groupes admin via
`SetAdminGroups`/`IsAdmin`), et c'est le **ServiceAccount de l'application** qui parle à Kubernetes
en leur nom, après le contrôle `actor.IsAdmin` fait côté service. Ça ne change pas avec ce design :
le patch de l'annotation continue de passer par `internal/service`, jamais par un accès Kubernetes
direct donné à l'utilisateur.

Ce que le RBAC Kubernetes doit garantir, en défense en profondeur (pas comme gate primaire) :
seuls le ServiceAccount de vcluster-manager (écriture des annotations) et celui du contrôleur
(écriture du status, lecture des annotations, gestion des objets Velero) ont des droits sur
`VClusterBackupOps`/`VClusterRestoreOps` — un `Role`/`RoleBinding` scoping par namespace
`vcluster-*`, pas un accès cluster-wide ouvert. C'est la même remarque que
`docs/recette-1.4-findings.md` fait déjà à propos du `ClusterRole` insuffisant du SA sur
`helmreleases`/`statefulsets` : le contrôleur aura besoin d'un périmètre RBAC au moins équivalent
(`patch` sur `helmreleases.helm.toolkit.fluxcd.io`, `statefulsets`, `deployments`,
`persistentvolumeclaims` dans `vcluster-*`, plus `get/list/watch/create/delete` sur
`backups.velero.io`/`restores.velero.io` dans le namespace Velero, plus
`get/list/watch/patch/update` sur son propre CRD marqueur), à définir précisément avec le manifeste
`config/crd` + `deploy/base/rbac.yaml` au moment de l'implémentation.

## 7. Où vit le contrôleur, et comment il réutilise le service

Le contrôleur est un `Reconciler` controller-runtime standard dans le futur
`internal/controller/` (`VClusterBackupOpsReconciler`, `VClusterRestoreOpsReconciler`, ou un seul
couvrant les deux CRD marqueurs). Son `Reconcile(ctx, req)` :

1. `Get` l'objet marqueur.
2. Compare annotation vs `status.*.lastHandledRequestedAt` (§5).
3. Si nouvelle demande, **appelle directement les méthodes de `internal/service/velero.go`** —
   `TriggerVeleroBackup`, `CreateVeleroRestore`, `GetVeleroRestoreStatus`, `GetVeleroBackupPhase`.
   Aucune de ces méthodes ne dépend de `net/http` (`internal/service/service.go` le garantit déjà en
   commentaire de package : « It never imports net/http ») — c'est très exactement ce qui rend ce
   plan possible sans réécriture : le contrôleur devient un **troisième adaptateur**, au même titre
   que `internal/handlers` (web) et le futur `internal/api` (REST), consommant la même instance de
   `*service.Service`.

Deux détails d'intégration à trancher au moment de l'implémentation, pas dans cette étude :

- **L'acteur** : `TriggerVeleroBackup`/`CreateVeleroRestore` vérifient `actor.IsAdmin` — un contrôle
  déjà fait une fois par `RequestVeleroBackup`/`RequestVeleroRestore` avant que l'annotation
  n'existe (§6). Le contrôleur doit passer un acteur système explicite, par exemple
  `models.Actor{Username: "vcluster-operator", IsAdmin: true}`, pour ne pas dupliquer un second
  contrôle métier côté Kubernetes RBAC (qui n'a pas la granularité groupes OIDC de toute façon). À
  documenter clairement dans le code pour qu'un futur lecteur ne confonde pas ce `IsAdmin: true`
  systématique avec un contournement — c'est un acteur technique, pas un humain.
- **`env`** : `s.k8sForEnv(env)` suppose aujourd'hui un process central capable de joindre
  plusieurs environnements (via tunnel SSH ou kubeconfig direct, `internal/kubernetes/status.go:94-169`).
  L'hypothèse déjà posée dans `docs/crd-vcluster.md` §2.1 — un opérateur par cluster hôte — rend ce
  paramètre vestigial pour le contrôleur : il tourne *dans* l'environnement qu'il reconcilie, et n'a
  besoin que d'un client vers son propre cluster (`kubernetes.NewStatusClient` avec un kubeconfig
  in-cluster, pas de tunnel). Le contrôleur peut construire un `*service.Service` avec un seul
  `k8sClients` fixé à `"" : localClient` et toujours appeler les méthodes service avec `env=""`
  (`envOrDefault` le résout déjà vers un environnement par défaut).

Ce que le contrôleur **n'appelle pas** directement : `internal/kubernetes/velero.go`. Il passe
systématiquement par `internal/service`, exactement comme les handlers web le font aujourd'hui —
c'est la couche qui porte RBAC, validation, audit, et la séquence data-safety. Court-circuiter le
service depuis le contrôleur reproduirait la logique en double, à l'exact endroit où
`docs/refactor-api.md` a justement investi pour l'éviter.

## 8. Chemin de migration

Le point rassurant : côté UI, **le comportement observable ne change presque pas**. Aujourd'hui déjà,
`CreateVeleroRestore` renvoie immédiatement `Phase: "New"` et l'UI poll ensuite via
`GetVeleroRestoreStatus` toutes les 3s en HTMX (`refactor-api.md`, table `velero`). Le modèle
annotation ne fait que déplacer *qui* exécute la séquence entre le retour immédiat et l'état
terminal — d'une goroutine lancée par le handler à une boucle de reconcile. La migration peut donc
se faire sans big-bang :

1. **Dark launch** : créer le CRD marqueur + le contrôleur, sans rien qui les déclenche encore —
   zéro risque, le chemin impératif actuel reste seul en service.
2. **Nouveau chemin en option** : ajouter `RequestVeleroBackup`/`RequestVeleroRestore` (§6) à côté
   de `TriggerVeleroBackup`/`CreateVeleroRestore` existants, sélectionné par un paramètre de config
   (par exemple `VELERO_TRIGGER_MODE=annotation|direct`, défaut `direct`) — les deux chemins
   partagent le même service en aval, seul le point d'entrée diffère.
3. **Validation preprod** : basculer `VELERO_TRIGGER_MODE=annotation` sur preprod, rejouer le
   scénario de recette existant (`docs/recette-1.4-findings.md` en donne déjà la trame : backup
   FSB, restauration in-place nominale, et les cas d'échec couverts par `ErrRestoreStageFailed`/
   `ErrRestoreStageFailedVolumeGone`), comparer les deux lignes d'audit (demande / exécution) et le
   `status` du marqueur contre ce que l'ancien flux rapportait.
4. **Bascule** : `direct` devient le fallback de secours pour une release, puis est retiré — la
   logique métier qu'il appelait ne disparaît pas, elle est déjà celle que le contrôleur utilise.
5. **REST (`/api/v1`, phase B de `docs/refactor-api.md`, pas encore mergée sur `main`)** : construire
   ses endpoints backup/restore directement au-dessus du modèle annotation dès le départ — pas
   besoin de faire exister puis migrer un endpoint REST synchrone qui n'a jamais servi.

## 9. Trade-offs

| | Impératif actuel | Annotation-trigger (proposé) | GitOps déclaratif complet (trigger dans `spec` commité) |
|---|---|---|---|
| Survit à un redémarrage du process | Non (`veleroResumeStates` en mémoire) | Oui (`status` sur objet K8s) | Oui, mais... |
| Latence de déclenchement | Immédiate | Immédiate (annotation = écriture API directe, pas de commit Git) | Minutes (commit → pull Flux → reconcile) |
| Trace de « qui a demandé quoi, quand » | Audit log applicatif seul | Audit log + `status`/events K8s | Historique Git (mais un ordre one-shot n'a rien à faire en Git, §1) |
| Complexité ajoutée | Aucune (déjà là) | Un petit CRD + un reconciler | Nécessite de committer puis « annuler » le déclencheur — pas de mécanisme propre |
| Concurrence (deux demandes simultanées) | Non gardée aujourd'hui (trou réel) | Gardée par le contrôleur (§4, `RestoreRejectedBusy`) | Dépend d'un contrôleur de toute façon — même gain, retardé par le commit |
| Cohérent avec la direction opérateur du projet | Non — c'est justement ce qu'on remplace | Oui — 3ᵉ adaptateur du service, même patron que web/REST | Contredit la distinction spec=désiré/annotation=action posée en §1 |

## 10. Lien avec la future CRD `VCluster`

Le marqueur `VClusterBackupOps`/`VClusterRestoreOps` (Option B) n'est pas pensé comme définitif.
Quand la CRD `VCluster` existera (étape 2, après le POC kro et le POC suppression de
`docs/crd-vcluster.md` §3.2bis) deux issues sont possibles, à trancher à ce moment-là avec plus de
recul :

- **Absorption** : les mêmes annotations (`backup.../requestedAt`, `restore.../requestedAt`) migrent
  sur l'objet `VCluster` lui-même, et son `status.lastBackup.*` (déjà prévu dans le schéma,
  `docs/crd-vcluster.md` §2.4) grandit pour couvrir les champs `restore.*` détaillés en §4. Le
  marqueur disparaît, remplacé sans perte de contrat — le nom des annotations et la forme du status
  ne changent pas, seul l'objet qui les porte change.
- **Cohabitation** : le marqueur reste une CRD compagnon, référencée par `ownerReference` depuis le
  `VCluster` (garbage collection automatique à la suppression du vcluster) — utile si on préfère
  garder le `status` du `VCluster` concentré sur son cycle de vie propre (Ready/Provisioning/...)
  plutôt que d'y mélanger l'historique backup/restore.

Dans les deux cas, rien de ce document n'est jeté : le contrat annotation + status + reconciler
reste identique, seul son support change.

### Bonus — page « gestion globale des backups »

Une vue listant tous les `Backup` Velero du cluster, y compris ceux dont le vcluster source a été
supprimé (orphelins), avec restauration vers une cible choisie, bénéficie directement de ce modèle
sans design supplémentaire :

- **Lister** : `internal/kubernetes/velero.go:ListVeleroBackups` filtre déjà par
  `spec.includedNamespaces` pour un nom donné (`internal/kubernetes/velero.go:21-36`) — la vue globale
  a besoin d'une variante sans filtre (`ListAllVeleroBackups`), petite extension, pas un nouveau
  concept.
- **Restaurer un orphelin vers une cible vivante** : c'est exactement le cas
  `targetName != name` déjà supporté par `CreateVeleroRestore` (restauration cross-vcluster, pas
  in-place, donc pas de séquence destructrice PVC) — dans le modèle annotation, on patch le
  marqueur `VClusterRestoreOps` **de la cible** (qui existe forcément, contrairement à la source
  potentiellement disparue) avec `from-backup: <nom du backup orphelin>`, `target` vide (puisque le
  marqueur est déjà sur la cible). Aucune nouvelle mécanique, juste la réutilisation du même
  contrat.

## Points ouverts

- **Nom exact et granularité du/des CRD marqueur(s)** : un `VClusterVeleroOps` combiné backup+restore,
  ou deux CRD séparés — implique un `status` un peu différent (deux sous-objets sur un seul type vs
  deux types simples). Pas d'impact sur le reste du design, à trancher à l'implémentation.
- **Extension de `TriggerVeleroBackup` pour un TTL explicite par demande** (§3) — aujourd'hui figé
  sur `s.cfg.VeleroDefaultTTL`.
- **RBAC précis du ServiceAccount du contrôleur** (§6) — à écrire avec `config/crd` +
  `deploy/base/rbac.yaml`, sur le modèle du gap déjà identifié par
  `docs/recette-1.4-findings.md` finding #1.
- **Cadence de requeue** pendant un backup/restore en cours — proposé 10s par cohérence avec le
  ticker existant, à revalider selon le comportement réel du controller-runtime `RequeueAfter` vs
  un watch sur les objets Velero eux-mêmes (moins de polling, plus réactif, mais plus de câblage).
- **Comment ce marqueur est créé** : au moment de `Service.Create` (comme le namespace) ou
  paresseusement à la première annotation (le contrôleur le crée s'il n'existe pas) — la seconde
  option évite de toucher `Service.Create` mais complique légèrement le premier reconcile
  (create-then-requeue). À trancher à l'implémentation, sans impact sur le contrat annotation/status.
