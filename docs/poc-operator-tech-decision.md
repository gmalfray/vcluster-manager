# POC opérateur — controller-runtime ou kro ? Verdict

> Statut : **POC exécuté, verdict rendu, seam branché sur le vrai service.** 2026-08-06.
> Code : [`poc/operator/`](../poc/operator/) — module Go séparé, 9 tests envtest verts (`-race`).
> Prérequis lus : [`adr-001-source-de-verite.md`](adr-001-source-de-verite.md),
> [`crd-vcluster.md`](crd-vcluster.md) §3.2bis et §7,
> [`design-backup-restore-annotation.md`](design-backup-restore-annotation.md).
>
> **Mise à jour (même jour, 2ᵉ passe)** : les 3 prérequis service du §5 sont **faits**, additifs,
> avec leurs tests (`internal/service/velero_hooks_test.go`). Conséquence directe :
> `var _ veleroops.Ops = (*service.Service)(nil)` **compile** — le contrôleur consomme désormais le
> vrai service, plus une interface partiellement fictive. Les tests du reconciler gardent un fake
> pour la couche qui parle au cluster, la séquence elle-même restant couverte par les tests du
> service.

## 0. Verdict en une phrase

**controller-runtime**, sans hésitation, pour la logique de cycle de vie — et le
POC ne le déduit plus d'un raisonnement sur les docs de kro, il le **mesure** :
la propriété qui décide de tout (savoir, après un redémarrage, de quel côté du
point de non-retour on se trouve) est obtenue en 4 lignes de status persisté, et
disparaît dès qu'on retire ces 4 lignes. kro reste un candidat crédible pour un
seul usage, l'expansion du graphe statique de la future CRD `VCluster`
(`crd-vcluster.md` §3.1), et cet usage-là n'est **pas** tranché ici.

## 1. La question posée au POC

`crd-vcluster.md` §3.2bis avait déjà tranché sur le papier ; ce qui manquait
était une vérification exécutable. Le POC ne rejoue donc pas le débat, il teste
la seule affirmation qui, si elle est fausse, invalide tout le chantier :

> une boucle de reconcile level-triggered peut-elle porter un enchaînement
> **impératif, asynchrone et destructif** — suspendre Flux → scaler à 0 →
> supprimer le PVC → créer le `Restore` → attendre → reprendre Flux — en
> **réutilisant `internal/service` sans réécrire la logique métier**, et en
> reprenant correctement après un redémarrage brutal ?

Le chemin backup/restore a été choisi plutôt que le chemin de suppression
(pourtant désigné POC prioritaire par §3.2bis) parce qu'il donne la même preuve
sur un risque strictement inférieur : pas de finalizer, pas de
`deletionTimestamp` irréversible, mais **le même point de non-retour** (le PVC
supprimé) et le même besoin de reprise après redémarrage. C'est aussi le cap
courant du chantier. La conclusion se transpose ; le chemin de suppression reste
à porter, avec son finalizer, qui est le vrai morceau restant.

## 2. Ce qui a été construit

Un module Go séparé (`poc/operator/`), pour que controller-runtime n'entre pas
dans le `go.mod` de l'app avant que l'opérateur soit un vrai chantier :

- **CRD marqueur `VClusterVeleroOps`** — l'option B du design §2 : un objet par
  vcluster, dans `vcluster-<name>`, dont le seul rôle est de porter les
  annotations de déclenchement et un `status` en bonne et due forme. Choix
  tranché au passage : **un seul type** couvrant backup et restore (deux
  sous-objets de status) plutôt que deux CRD — un type de moins à faire vivre
  pour zéro perte de contrat.
- **Un reconciler** qui compare `metadata.annotations[...requestedAt]` à
  `status.*.lastHandledRequestedAt`, écrit **exclusivement** par la
  sous-ressource `/status`, et requeue à 10 s (la cadence du ticker qu'il
  remplace — un test le verrouille pour que la migration ne change pas
  silencieusement la cadence de polling).
- **Le seam vers le service** (`internal/veleroops/`), écrit dans les **types du
  service lui-même**, pas dans des DTO parallèles. `seam_assert.go` affirme à la
  compilation que `*service.Service` satisfait déjà la partie qui existe. Si une
  signature bouge, ça ne compile plus : le POC ne peut pas dériver en douce du
  service qu'il prétend réutiliser.
- **8 tests envtest**, contre un vrai kube-apiserver — pas un client fake. Les
  propriétés testées (sémantique de la sous-ressource `status`, ce qui persiste
  réellement, concurrence optimiste) sont exactement celles qu'un fake masque.
  Ça tourne **sans l'infra Hetzner** (coupée, coût 0) : `make test` dans un
  conteneur, ~6 s.

Ce qui reste faux dans le POC, volontairement : la couche qui parle à Velero et
à Flux est un `fakeOps` scriptable. Ce qui est prouvé, c'est la sémantique de
reconcile, pas l'intégration Velero — déjà couverte par les tests de
`internal/service`.

## 3. Résultats

| Propriété | Test | Ce que ça prouve |
|---|---|---|
| Une demande = une action, quel que soit le nombre de reconciles | `TestBackupRequestIsHandledExactlyOnce` | Le couple annotation / `lastHandledRequestedAt` (patron Flux) suffit ; 4 reconciles ⇒ 1 seul `TriggerVeleroBackup`, une nouvelle valeur ⇒ une nouvelle action |
| Deux restores ne tournent jamais en parallèle | `TestConcurrentRestoreIsDeferredNotStarted` | **Garantie nouvelle** : rien n'empêche aujourd'hui deux `CreateVeleroRestore` de lancer chacun leur scale-down/suppression de PVC sur le même vcluster |
| **Tué après suppression du volume ⇒ Flux N'EST PAS repris** | `TestRestoreKilledAfterPVCDeletionDoesNotResumeFlux` | Le cas qui fait perdre des données silencieusement. Un reconciler **neuf**, sans aucun état en mémoire, lit `status.restore.stage == PVCDeleted` et refuse de reprendre Flux (qui recréerait un PVC vide et masquerait l'échec) |
| Tué avant le point de non-retour ⇒ Flux **est** repris | `TestRestoreKilledBeforePointOfNoReturnResumesFlux` | Décision inverse, prise depuis la même unique source : l'étape persistée. Volume intact ⇒ on remet le vcluster debout |
| La boucle remplace `resumeAfterInPlaceRestore` | `TestInPlaceRestoreIsFollowedUntilFluxIsBack` | Requeue jusqu'à confirmation de la reprise de Flux, en signalant « pending » plutôt qu'un faux succès. Pas de goroutine, pas de timeout 2 h, aucune dépendance à un navigateur qui poll |
| Le contrôleur n'écrit **que** le status | `TestControllerWritesStatusOnly` | Annotations et `spec` intacts, `generation` inchangée ⇒ la séparation RBAC du design §5 est réelle, pas une intention |
| Le câblage manager tient | `TestSetupWithManager` | Enregistrement effectif contre un vrai API server |

### Les tests ont des dents (vérifié par mutation)

Une suite verte ne prouve rien si elle passe aussi sur du code cassé. Deux
mutations ont été appliquées puis annulées :

| Mutation | Effet |
|---|---|
| `stageWriter` n'écrit plus le status (l'étape reste en mémoire) | Les **deux** tests de redémarrage tombent : `stage persisté = "", attendu "PVCDeleted"` / `attendu "ScaledDown"` |
| Garde de concurrence désactivée | `CreateVeleroRestore appelé 1 fois alors qu'un restore tourne` |

La première mutation est exactement le monde d'aujourd'hui — l'étape connue
seulement du process vivant. C'est la mesure directe de ce que le passage en
opérateur apporte.

## 4. Pourquoi pas kro (et où kro reste pertinent)

kro compose des ressources : un `ResourceGraphDefinition` expose un schéma
d'API et décrit les objets Kubernetes derrière, reliés par des expressions
**CEL — délibérément non Turing-complet**, validé à la création. C'est un choix
de conception assumé côté kro, pas une lacune : composition déclarative,
vérifiable, sans logique impérative.

Or ce que le POC vient de faire tenir n'est pas de la composition :

- « appelle Velero, **puis** attends une phase terminale, **puis** décide de
  reprendre Flux ou pas selon l'étape atteinte » — une séquence d'effets de
  bord ordonnés avec des branchements sur des états externes ;
- deux branches de **sécurité des données** qui dépendent d'un état non
  Kubernetes (le PVC a-t-il été supprimé avant le crash ?) ;
- des appels à des API qui ne sont pas des ressources Kubernetes du cluster
  hôte — et, sur le chemin complet du cycle de vie, Keycloak, GitLab, Rancher,
  Vault (`crd-vcluster.md` §3.2).

Rien de tout ça n'est exprimable en CEL sur un graphe de ressources. **kro seul
est écarté**, ce qui confirme §3.2bis sur pièce plutôt que par déduction.

Ce que le POC **ne** tranche **pas** : les 5 inconnues kro de `crd-vcluster.md`
§7 (ownership et field managers face à Flux, prune quand un champ disparaît,
génération conditionnelle en CEL, ce que vaut le `status` agrégé de kro,
stabilité inter-versions). Elles portent sur l'expansion du graphe de la CRD
`VCluster`, pas sur ce chemin-ci, et restent ouvertes. La décision d'ADR-001
tient : contrôleur controller-runtime, embarquant éventuellement kro pour le
graphe statique.

## 5. Ce que le POC a trouvé dans le design et dans le service

Findings de fond. Aucun n'invalide le design ; tous auraient été découverts en
cours de route, plus cher. **1 à 5 sont faits** (2ᵉ passe, additifs, testés) ;
6 à 8 restent des choix ouverts.

1. ✅ **Le design §4 point 4 est infaisable tel quel.** Il demande d'écrire
   `status.restore.stage` à chaque étape, tout en appelant
   `Service.CreateVeleroRestore` « sans aucune modification de sa séquence
   interne » (§4 point 3). Les deux ensemble sont contradictoires : la séquence
   entière se déroule dans un seul appel bloquant, donc un contrôleur tué en
   cours d'appel ne peut pas savoir si le PVC a été supprimé. **Correctif
   proposé, additif : un paramètre `onStage`** appelé juste avant chaque étape
   destructrice, qui persiste l'étape et dont l'échec **annule** l'étape
   annoncée — de sorte qu'une étape enregistrée ne soit jamais en retard sur la
   réalité (la direction dangereuse : croire le volume intact alors qu'il ne
   l'est plus). La séquence interne et les deux sentinelles ne bougent pas.
   → **Fait** : `RestoreHooks{OnStage, OwnsFollowUp}` +
   `CreateVeleroRestoreWithHooks`. `CreateVeleroRestore` devient une façade à
   hooks vides, donc **les appelants web/REST et leurs tests sont inchangés**.
   Le contrat « échec d'enregistrement ⇒ l'étape annoncée ne s'exécute pas » est
   testé, et vérifié par mutation (neutraliser l'abandon fait tomber le test).
2. ✅ **`abortInPlaceRestore` doit être exporté.** Reprendre une séquence
   interrompue *avant* le point de non-retour demande de reprendre Flux ; le
   service ne sait le faire qu'en interne, depuis `CreateVeleroRestore`.
   → **Fait** : `AbortInPlaceRestore(ctx, actor, name, env)`, admin only,
   validation du nom avant tout accès cluster, ligne d'audit dédiée.
3. ✅ **La goroutine `resumeAfterInPlaceRestore` devient un second pilote.** En
   mode annotation, `CreateVeleroRestore` la lance toujours, en parallèle de la
   boucle de reconcile : deux mécanismes pour la même reprise.
   → **Fait** : `RestoreHooks.OwnsFollowUp` la neutralise, et un test à deux
   branches vérifie les deux comportements (avec le flag : aucun poll ; sans :
   le watcher poll comme avant). L'intervalle du watcher est devenu injectable
   pour que « la goroutine a-t-elle démarré ? » soit observable en millisecondes
   au lieu de dix secondes. À terme `veleroResumeStates` + la goroutine
   disparaissent — le status du marqueur devient le registre.
4. ✅ **Pas d'accesseur de phase pour un backup donné.** Le contrôleur pollait
   via `GetVeleroBackups`, qui liste tout et filtre côté appelant, alors que
   `internal/kubernetes` a déjà `GetVeleroBackupPhase`.
   → **Fait** : `Service.GetVeleroBackupPhase(ctx, backup, env)`, read-only.
5. ✅ **`isTerminalRestorePhase` non exporté** ⇒ dupliqué dans le POC.
   → **Fait** : exporté en `IsTerminalRestorePhase`, la copie du POC supprimée.
6. **Le TTL par demande (design §3) n'est pas honorable aujourd'hui** :
   `TriggerVeleroBackup` est figé sur `cfg.VeleroDefaultTTL`. Le POC enregistre
   `status.backup.requestedTTL` **et** `ttlHonoured: false` — écrit noir sur
   blanc plutôt que silencieusement ignoré.
7. **« Refuser » une demande concurrente : refus sec ou report ?** Le design dit
   refuser (§4 point 2). Le POC **ne consomme pas** l'annotation et requeue : la
   demande est honorée quand le restore en cours se termine. Plus utile qu'un
   refus qui perd la demande, mais c'est un choix de sémantique à valider — une
   demande peut attendre longtemps sans que personne la regarde.
8. **Réservation avant action = at-most-once, assumé.** Le POC écrit
   `lastHandledRequestedAt` **avant** de lancer la séquence. Un crash entre les
   deux perd la demande, ce qui est la bonne direction : l'alternative relance
   une séquence destructrice à chaque redémarrage. Une nouvelle valeur
   d'annotation est le retry, et c'est un humain qui la pose.
9. ✅ **Un piège que seule l'implémentation a fait apparaître** : puisqu'une
   étape est annoncée **avant** de s'exécuter, un échec de la suppression du PVC
   laisse `status.restore.stage == PVCDeleted` alors que le volume est intact —
   et le service, lui, a bien repris Flux. Sans correctif, le reconcile suivant
   lisait ce marqueur, concluait « interrompu après suppression du volume » et
   signalait une perte de données qui n'a pas eu lieu. → **Corrigé** : sur
   `ErrRestoreStageFailed` (donc en amont du point de non-retour), le contrôleur
   remet `stage` à vide ; l'échec reste décrit par les conditions. C'est le
   revers assumé du choix « le registre peut être en avance sur la réalité,
   jamais en retard » : il faut nettoyer explicitement quand l'avance s'avère
   fausse.

## 6. Suite

Dans l'ordre, chaque étape restant petite et vérifiable :

1. ~~Les 3 changements de service~~ — **faits** (findings 1 à 5), additifs, avec
   tests, toute la suite de l'app verte en `-race`.
2. ~~Câbler le vrai `*service.Service` derrière `veleroops.Ops`~~ — **fait**,
   l'assertion de type le prouve à la compilation. **Reste** : le binaire
   opérateur + RBAC (`Role` scopé sur `vcluster-*`, cf. design §6 et le finding
   #1 de `recette-1.4-findings.md` sur le `ClusterRole` insuffisant), et la
   création du marqueur (à `Service.Create` ou paresseusement).
3. **Chemin backup en prod d'abord** (non destructif) puis restore, derrière
   `VELERO_TRIGGER_MODE`, validé sur preprod au prochain `up.sh`.
4. **Porter le chemin de suppression + finalizer** — le POC prioritaire de
   §3.2bis, désormais avec un patron éprouvé pour la reprise après redémarrage.
   Le finalizer et l'irréversibilité du `deletionTimestamp` restent la vraie
   inconnue.
5. **Page « backups globale »** (design §10 bonus) : `ListAllVeleroBackups` +
   restauration d'un backup orphelin vers une cible, en annotant le marqueur de
   la cible. Aucune mécanique nouvelle.

## Note d'exécution

Poste sans Go local : tout passe par un conteneur `golang:1.25` (voir
`poc/operator/Makefile`). Le disque `/` était **plein à 100 %** au début de la
session — la compilation `-race` échoue alors en `no space left on device` avant
tout message clair. Après nettoyage, **tout tourne en `-race`**, POC et app, sans
data race.

Sources kro : [kro.run/docs/overview](https://kro.run/docs/overview/) ·
[kubernetes-sigs/kro](https://github.com/kubernetes-sigs/kro)
