# POC opérateur — controller-runtime ou kro ? Verdict

> Statut : **POC exécuté, verdict rendu, seam branché sur le vrai service.** 2026-08-06.
> Code : [`poc/operator/`](../poc/operator/) — module Go séparé, 9 tests envtest verts (`-race`).
> Prérequis lus : [`adr-001-source-de-verite.md`](adr-001-source-de-verite.md),
> [`crd-vcluster.md`](crd-vcluster.md) §3.2bis et §7,
> [`design-backup-restore-annotation.md`](design-backup-restore-annotation.md).
>
> **Mise à jour (même jour, 2ᵉ passe)** : les 3 prérequis service du §5 sont **faits**, additifs,
> avec leurs tests. Conséquence directe :
> `var _ veleroops.Ops = (*service.Service)(nil)` **compile** — le contrôleur consomme désormais le
> vrai service, plus une interface partiellement fictive. Les tests du reconciler gardent un fake
> pour la couche qui parle au cluster, la séquence elle-même restant couverte par les tests du
> service.
>
> **Mise à jour (3ᵉ passe — revue de l'agent `simplificateur`)** : le breadcrumb d'étape est
> **supprimé**, remplacé par une lecture de l'état réel du cluster ; il masquait deux bugs, corrigés.
> Voir §5bis. Bilan de cette passe : **−306 lignes**, deux énumérations parallèles et un contrat
> subtil en moins, deux propriétés de sécurité en plus. Les findings 1 et 3 du §5 sont **caducs** :
> ce qu'ils réparaient n'existe plus.

## 0. Verdict en une phrase

**controller-runtime**, sans hésitation, pour la logique de cycle de vie — et le POC ne le déduit
plus d'un raisonnement sur les docs de kro, il le **mesure** : la propriété qui décide de tout
(savoir, après un redémarrage, de quel côté du point de non-retour on se trouve) tient, et elle tient
**sans rien écrire d'avance** — le contrôleur relit l'état du cluster (§5bis). **kro est écarté
entièrement**, y compris pour l'expansion du graphe statique (§4).

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
  `status.*.lastHandledRequestedAt`, écrit **exclusivement** par la sous-ressource `/status`, et
  requeue à 10 s (la cadence du ticker qu'il remplace). Le marqueur n'a **pas de `spec`** : le
  vcluster piloté est son propre `metadata.name`, dans le namespace `vcluster-<name>`.
- **Le seam vers le service** (`internal/veleroops/`), écrit dans les **types du
  service lui-même**, pas dans des DTO parallèles. `seam_assert.go` affirme à la
  compilation que `*service.Service` satisfait déjà la partie qui existe. Si une
  signature bouge, ça ne compile plus : le POC ne peut pas dériver en douce du
  service qu'il prétend réutiliser.
- **9 tests envtest**, contre un vrai kube-apiserver — pas un client fake. Les
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
| **Volume supprimé ⇒ Flux N'EST PAS repris** | `TestInterruptedRestoreWithVolumeGoneDoesNotResumeFlux` | Le cas qui fait perdre des données silencieusement. Un reconciler **neuf**, sans aucun état en mémoire, relit le cluster (PVC absent ou en `Terminating`) et refuse de reprendre Flux, qui recréerait un PVC vide et masquerait l'échec |
| Volume intact ⇒ Flux **est** repris | `TestInterruptedRestoreWithVolumeIntactResumesFlux` | Décision inverse, prise depuis la même unique source : l'état réel du volume |
| **Un restore en cours est adopté, pas pris pour une perte de volume** | `TestInterruptedRestoreAdoptsARunningRestore` | Le cas où un registre écrit se trompe (bug 2, §5bis) : le nom du restore n'a jamais été enregistré, mais le cluster le connaît |
| La reprise de Flux **renonce et le dit** | `TestFluxResumeGivesUpAfterTheDeadline` | Un échec durable finit signalé au lieu d'être réessayé en silence toutes les 10 s (bug 1, §5bis). La borne vit dans le status, donc elle survit à un redémarrage |
| La boucle remplace `resumeAfterInPlaceRestore` | `TestInPlaceRestoreIsFollowedUntilFluxIsBack` | Requeue jusqu'à confirmation de la reprise de Flux, en signalant « pending » plutôt qu'un faux succès. Pas de goroutine, pas de timeout 2 h, aucune dépendance à un navigateur qui poll |
| Le contrôleur n'écrit **que** le status | `TestControllerWritesStatusOnly` | Annotations et `spec` intacts, `generation` inchangée ⇒ la séparation RBAC du design §5 est réelle, pas une intention |
| Le câblage manager tient | `TestSetupWithManager` | Enregistrement effectif contre un vrai API server |

### Les tests ont des dents (vérifié par mutation)

Une suite verte ne prouve rien si elle passe aussi sur du code cassé. Deux
mutations ont été appliquées puis annulées :

| Mutation | Effet |
|---|---|
| Garde de concurrence désactivée | `restore lancé 1 fois alors qu'un restore tourne` |
| On ignore un restore en cours à la reprise | `requeue 0s, attendu 10s : le restore adopté doit être suivi` |
| Borne de renoncement retirée | `requeue 10s : on continue de réessayer indéfiniment au lieu de signaler` |
| *(2ᵉ passe, sur le breadcrumb depuis supprimé)* `stageWriter` n'écrit plus le status | Les deux tests de redémarrage de l'époque tombaient — ce qui prouvait que le mécanisme marchait, pas qu'il était nécessaire |

La dernière ligne est instructive : une mutation qui fait tomber les tests prouve que le code fait ce
qu'il dit, pas qu'il méritait d'exister. C'est exactement l'angle mort qu'une revue de simplicité
couvre et qu'une revue de correction ne couvre pas.

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

**Et pour l'expansion du graphe statique (`crd-vcluster.md` §3.1) non plus** — porte fermée à la
3ᵉ passe. C'était le seul usage qui restait à kro. Le raisonnement : `internal/gitops/generator.go`
produit déjà ce gabarit, avec des tests ; le remplacer par un `ResourceGraphDefinition` ajoute un
opérateur tiers à installer, versionner et débugger, dont les **5 inconnues du §7** (ownership et
field managers face à Flux, prune quand un champ disparaît, génération conditionnelle en CEL, valeur
du `status` agrégé, stabilité inter-versions) sont **toutes du coût opérationnel**, payé par une seule
personne. Le contrôleur peut appliquer les objets rendus par les templates existants en Server-Side
Apply sous son propre field manager : du code ennuyeux, dans un binaire qu'on a déjà.

Deux nuances qui restent vraies : les inconnues **1 et 2** du §7 (field manager face à Flux, prune)
ne sont **pas** des questions kro — elles se posent à n'importe quel mécanisme d'expansion, y compris
le nôtre, et devront être levées de toute façon. Et rien de tout ça ne touche la source de vérité :
ADR-001 (le CR `VCluster` versionné) ne dépend pas de qui l'expanse, donc ce choix reste réversible
sans changer le contrat.

## 5. Ce que le POC a trouvé dans le design et dans le service

Findings de fond. Aucun n'invalide le design ; tous auraient été découverts en
cours de route, plus cher. **1 à 5 sont faits** (2ᵉ passe, additifs, testés) ;
6 à 8 restent des choix ouverts.

1. ⛔️ **CADUC (3ᵉ passe)** — le design §4 point 4 est infaisable tel quel. Il demande d'écrire
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
3. ✅ **La goroutine `resumeAfterInPlaceRestore` devient un second pilote.** *(le flag est devenu une
   méthode dédiée, `CreateVeleroRestoreUnwatched`, plutôt qu'un booléen dans une structure)* En
   mode annotation, `CreateVeleroRestore` la lance toujours, en parallèle de la
   boucle de reconcile : deux mécanismes pour la même reprise.
   → **Fait** : `CreateVeleroRestoreUnwatched` ne la démarre pas, et un test à deux branches vérifie
   les deux comportements. L'intervalle du watcher est injectable pour que « la goroutine a-t-elle
   démarré ? » soit observable en millisecondes au lieu de dix secondes. **Décision Greg (3ᵉ passe) :
   la cohabitation doit être la plus courte possible** — la goroutine, `veleroResumeStates` et cette
   seconde méthode partent dans la même release que la bascule, pas « à terme ».
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
9. ⛔️ **CADUC (3ᵉ passe), et c'était un signal** : puisqu'une
   étape est annoncée **avant** de s'exécuter, un échec de la suppression du PVC
   laisse `status.restore.stage == PVCDeleted` alors que le volume est intact —
   et le service, lui, a bien repris Flux. Sans correctif, le reconcile suivant
   lisait ce marqueur, concluait « interrompu après suppression du volume » et
   signalait une perte de données qui n'a pas eu lieu. → **Corrigé** : sur
   `ErrRestoreStageFailed` (donc en amont du point de non-retour), le contrôleur
   remet `stage` à vide ; l'échec reste décrit par les conditions. C'est le
   revers du choix « le registre peut être en avance sur la réalité, jamais en retard ». Devoir
   nettoyer un registre parce qu'il mentait était le signe qu'il ne fallait pas de registre : sans
   breadcrumb, ce cas n'existe plus.

## 5bis. Ce que la revue de simplicité a changé (3ᵉ passe)

Le POC a été relu par l'agent `simplificateur` (`.claude/agents/`), dont le mandat est de chercher la
solution la plus simple qui tienne les propriétés exigées. Il a trouvé mieux qu'une simplification :
**deux bugs**, tous deux causés par le mécanisme qu'il proposait de retirer.

**Ce qui est supprimé : le breadcrumb d'étape** (`RestoreHooks.OnStage`, `service.RestoreStage`,
`v1alpha1.RestoreStage`, `status.restore.stage`). Le constat qui emporte la décision : sur quatre
étapes persistées, **une seule discriminait** (`PVCDeleted`) — `FluxSuspended` et `ScaledDown`
tombaient dans la même branche, et `RestoreCreated` était redondant avec `restoreName != ""`. Quatre
constantes dans deux packages, plus un contrat subtil, pour un booléen.

**Ce qui remplace : lire le cluster** (`Service.InspectInterruptedRestore`, appuyé sur
`StatusClient.GetVClusterPVCState` et `ListActiveVeleroRestores`, qui existait déjà). Les deux faits
qui décident sont directement observables : le PVC est-il absent ou en `Terminating`, et un `Restore`
tourne-t-il ? Un fait **observé** ne peut pas être périmé, contrairement à un fait **écrit** par un
process qui est mort juste après.

**Bug 1 — `ResumeFailed` était devenu inatteignable.** Le seul endroit qui écrit `failed=true` est le
renoncement à 2 h de `resumeAfterInPlaceRestore`, or le mode opérateur empêche cette goroutine de
démarrer. Donc la branche `ResumeFailed` du contrôleur était du code mort, et une reprise de Flux
durablement en échec **réessayait toutes les 10 s indéfiniment**, sans borne ni escalade — j'avais
retiré le filet de la goroutine sans le remplacer, sur un état que le code qualifie lui-même de vrai
problème pour l'exploitant. Corrigé par `status.restore.firstTerminalAt` + `ResumeGiveUpAfter` (2 h,
le même budget) : la borne vit maintenant dans le status, donc elle **survit à un redémarrage**, ce
que le `context.WithTimeout` de la goroutine ne faisait pas.

**Bug 2 — le breadcrumb se trompait, précisément là où il était censé sauver.** Si le process mourait
entre la création du `Restore` Velero et l'enregistrement de son nom, le reconcile suivant lisait
`stage=PVCDeleted` + `restoreName=""`, concluait « volume détruit, aucun restore » et **rendait la
main sans requeue** — alors qu'un restore tournait et allait réussir. Flux n'aurait jamais été repris.
La lecture du cluster voit ce restore et **l'adopte** (condition `Adopted`). C'est l'argument décisif
en faveur de l'observation : le registre écrit était faux dans un cas où le cluster, lui, savait.

**Bug 3, mineur** — `MaxConcurrentReconciles` valait 1 par défaut alors qu'un reconcile de restore est
synchrone et long (il attend la terminaison des pods) : il gelait le polling de tous les autres
marqueurs. Une ligne dans `SetupWithManager`.

**Trois suppressions sèches, validées par Greg** : `spec.vclusterName` et `spec.env` (le nom était en
triple exemplaire, et `crd-vcluster.md` §2.1 argumente explicitement contre un `env` en spec — le POC
faisait le contraire de la règle du projet) ; d'où un `spec` désormais **vide**, donc
`metadata.generation` immobile, donc `observedGeneration` supprimé. `RequeueInterval` injectable :
**aucun test ne s'en servait**, la valeur était toujours celle par défaut — devenue une constante. Et
les deux champs de status du TTL par demande, avec l'annotation qui allait avec : personne n'a jamais
demandé cette fonctionnalité, écrire « pas implémenté » dans un status coûtait plus que de ne rien
promettre.

**Ce qui n'a pas bougé d'un iota** : la séquence in-place et ses deux sentinelles. Le `git diff` sur
ce bloc ne montre plus que la disparition des appels de hook.

**Une réserve à lever avant la mise en service** : l'inférence « PVC absent ⇒ nous avons supprimé le
volume » suppose que rien d'autre ne supprime ce PVC. Aucun autre appelant de `DeleteVClusterPVC` dans
le code, mais les manifests Flux et un éventuel `reclaimPolicy` agressif côté StorageClass n'ont pas
été audités. **À confirmer sur preprod au prochain `up.sh`.**

## 6. Suite

Dans l'ordre, chaque étape restant petite et vérifiable :

1. ~~Les 3 changements de service~~ — **faits** (findings 1 à 5), additifs, avec
   tests, toute la suite de l'app verte en `-race`.
2. ~~Câbler le vrai `*service.Service`, binaire opérateur, RBAC, création du marqueur~~ — **fait**.
   Le module `poc/operator` a été **fusionné dans l'app** (l'agent l'avait assorti d'une date de
   péremption : « à fusionner dès que l'opérateur a un `main()` ») → `api/v1alpha1`,
   `internal/controller`, `internal/veleroops`, `cmd/operator`, `config/{crd,rbac}`. Le `replace` et
   le second `go.mod` disparaissent. `make test-operator` lance la suite envtest ; `go test ./...` la
   saute proprement quand les binaires ne sont pas là, plutôt que de casser toute la suite.
   Le marqueur est créé **paresseusement**, à la première demande (`Service.RequestVeleroBackup` →
   `StatusClient.RequestVeleroOps`) : `Service.Create` n'est pas touché, et un vcluster déjà en place
   en obtient un sans migration. **Reste** : le manifeste de déploiement (domaine `gitops-flux`) et
   la bascule du handler.
3. **Chemin backup en prod d'abord** (non destructif) puis restore, derrière `VELERO_TRIGGER_MODE`,
   validé sur preprod au prochain `up.sh`. `Service.RequestVeleroBackup` existe et est testé, mais
   **aucun handler ne l'appelle encore** : c'est la seule pièce volontairement en avance sur son
   appelant, parce qu'elle est la cible de cette bascule et qu'elle rend l'opérateur testable à la
   main (`kubectl annotate`) sans écrire de YAML de marqueur.
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
