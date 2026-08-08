# État de la brique opérateur

> Écrit le 2026-08-07, tenu à jour jusqu'à `3471db9`. Ce document dit **ce qui est construit**,
> pas ce qui est conçu — pour la conception, voir `adr-001-source-de-verite.md`,
> `adr-002-modele-cells.md` et `crd-vcluster.md`.

## Ce qui tourne

Un opérateur controller-runtime (`cmd/operator`), deux reconcilers.

`VClusterReconciler` enchaîne, dans cet ordre et pour une raison à chaque fois :

| Étape | Fichier | Ce qu'elle fait |
|---|---|---|
| garde de placement | `namespace_guard.go` | refuse tout CR hors du namespace autorisé |
| sommeil | `vcluster_controller.go` | applique `spec.suspend`, ouvre la fenêtre d'annulation |
| budget | `vcluster_budget.go` | refuse ce qui ferait dépasser le plafond de la cell |
| provisionnement | `vcluster_provision.go` | applique le namespace + le ConfigMap de substitutions |
| intégrations | `vcluster_integrations.go` | backend d'auth Vault, client OIDC Keycloak, appairage Rancher |
| status observé | `vcluster_status.go` | lit le cluster, agrège `Ready`, calcule la phase |
| suppression | `vcluster_finalizer.go` | finalizer, garde-fou, sauvegarde, destruction puis retrait du namespace |

L'ordre n'est pas cosmétique : le sommeil d'abord parce qu'il est inutile de
provisionner ce qu'on vient d'endormir ; le budget avant le provisionnement pour ne
rien matérialiser qu'on refuserait ensuite ; les intégrations avant l'observation
parce que c'est elle qui en constate le résultat, et constater avant d'agir ferait
toujours voir l'état du passage précédent.

`VeleroOpsReconciler` (`veleroops_controller.go`) porte la sauvegarde et la
restauration à la demande, déclenchées par annotation sur la CRD marqueur
`VClusterVeleroOps`.

## Les trois règles qui gouvernent tout le code

**Reprise par observation, jamais par registre écrit.** Un état d'avancement qu'on
écrit soi-même puis qu'on relit pour savoir où on en est est un registre qui ment dès
que le processus meurt entre l'action et l'écriture. Chaque étape se décide donc sur
un fait du monde : Rancher connaît-il encore ce cluster, existe-t-il une sauvegarde
postérieure au `deletionTimestamp`, le PVC est-il encore là. Le finalizer a refusé
pour cette raison le `status.deletion.stage` que le design demandait ; `stage` reste
écrit, mais pour un humain qui lit un objet coincé, jamais relu par le code.

**Inconnu n'est pas faux.** `True` = lu et bon, `False` = lu et pas prêt, `Unknown` =
pas lu. Rancher injoignable ne remet pas `paired` à `false` — ça se lirait
« dépairé » et inviterait à réappairer un cluster qui l'est déjà. Un cluster hôte
muet n'efface pas `chartVersion` — sa disparition se lirait comme une
désinstallation.

**Un test qui ne tue aucun mutant ne compte pas.** Chaque décision de conception est
annulée une par une, et le test qui la vise doit tomber. Piège vérifié : une mutation
qui rend un import inutilisé ne compile pas, et `go test` échoue alors pour la
mauvaise raison — un mutant compté tué à tort.

## Le provisionnement : un objet, pas neuf

L'arbre généré pour un vcluster fait 16 fichiers, dont 9 sont des objets Kubernetes
applicables tels quels. L'opérateur n'en applique **que deux** : le namespace et un
ConfigMap de 32 clés dérivées du CR, que Flux consomme par
`postBuild.substituteFrom`.

Appliquer les neuf aurait mis un second propriétaire sur des objets que Flux possède
déjà, sans supprimer un seul fichier commité : cinq des sept Kustomizations ont un
`path` qui pointe vers un répertoire par vcluster, lequel n'existe que parce que le
générateur le commite. Le gain d'ADR-001 n'y était pas.

Avec un seul objet, les deux inconnues du design disparaissent : pas de prune à gérer
puisque l'objet est toujours désiré, pas de bagarre de field manager puisque Flux n'y
touche jamais.

**Mesure** : 16 fichiers commités par vcluster aujourd'hui, **10** après bascule
complète des Kustomizations vers `lib/tenant-template` + substitution. La bascule de
`navlink` est faite comme démonstration de bout en bout. Le `HelmRelease` de
`clusters/<env>/base` est irréductible depuis ce dépôt : il n'y est pas, le
`kustomization.yaml` racine ne fait que le patcher.

## La garde de placement

Les deux reconcilers dérivaient tout de `metadata.name` sans regarder le namespace.
Un marqueur nommé d'après le vcluster d'autrui, déposé chez soi, pilotait une
restauration destructrice sur la victime ; un marqueur nommé `manager` faisait
sauvegarder `vcluster-manager`, c'est-à-dire exporter les secrets de l'app vers le
bucket S3.

Les objets sont désormais **auto-bornés** : un marqueur ne parle que du vcluster dont
il habite le namespace ; un `VCluster` ne peut être déclaré que dans le namespace
unique d'où la Flux de la cell applique (`--vclusters-namespace`). Viser une victime
exige d'abord des droits chez elle — c'est le contrôle d'autorisation qui manquait,
exprimé là où l'API server peut l'appliquer.

Le namespace du CR est plat, et non `vcluster-<nom>` comme celui des marqueurs : le CR
est ce qui *déclare* un vcluster, donc il existe avant le namespace dont il parle. Le
faire vivre dedans serait circulaire.

## Arbitrages rendus le 2026-08-07

**N6 — le finalizer supprime le namespace qu'il a créé.** ✅ **Implémenté.**
L'étape `Destroying` ne supprimait rien : c'était le prune Flux d'un commit que le
finalizer n'écrit ni ne vérifie, donc il annonçait « suppression terminée » sans
preuve. Des deux issues possibles — supprimer soi-même, ou attendre de constater
que Flux l'a fait — c'est la première qui est retenue.

Raison : l'opérateur applique déjà ce namespace lui-même en Server-Side Apply, il
en est donc propriétaire de fait. Le faire supprimer par lui rend la séquence
auto-suffisante et vérifiable, au lieu d'introduire une attente qui peut ne jamais
aboutir et qu'il faudrait borner. Conséquence traitée dans le même changement :
le commentaire de `ProvisionFieldManager` affirmait que Flux n'écrit aucun des deux
objets appliqués — c'était **faux pour le namespace**, qui vient aussi de
`clusters/<cell>/base` et dont l'overlay tenant patche le `metadata.name`. La
propriété y est désormais tranchée : l'opérateur possède la **création** et la
**suppression**, Flux la **réapplication** tant que l'overlay est commité.

Ce qui a été livré avec, et qui ne va pas de soi :

- **L'observation conclut, pas l'appel.** Un `delete` sur un namespace ne fait que
  poser un `deletionTimestamp`. Rendre `true` juste après aurait reproduit le
  défaut corrigé, à un maillon près. L'étape demande, puis relit — et ne lâche le
  CR que sur une disparition constatée.
- **L'attente est bornée à 10 minutes**, comme Rancher. Au-delà, ce qui retient le
  namespace ne partira pas parce qu'on insiste : un finalizer tiers, ou la
  Kustomization Flux du tenant qui le réapplique aussi vite qu'on le supprime. On
  lâche alors le CR — le retenir n'efface rien et ajoute un objet coincé au
  problème — en **nommant** dans le message final ce qui reste debout. C'est la
  dernière chose écrite avant que l'objet disparaisse : ce qui n'y est pas n'est
  plus lisible nulle part.
- **La protection se relit avant de détruire, pas seulement avant d'écrire.** Le
  code ne levait pas `protect-deletion` quand la lecture échouait — bon réflexe —
  puis détruisait quand même. Le garde-fou était donc contourné par la panne qu'il
  aurait dû faire échouer. Ce n'était pas dangereux tant que la destruction passait
  par le prune Flux, qu'une annotation restée posée retient ; depuis que le
  finalizer supprime lui-même, plus rien ne la lit sur ce chemin. La séquence
  s'arrête maintenant sur `Ready=False/ProtectionUnknown`, avant toute destruction,
  et `status.protectionEnabled` n'affirme plus une levée qu'il n'a pas constatée.
- Le RBAC de l'opérateur gagne `delete` sur `namespaces`.

**Ce qui borne réellement ce `delete`, et il faut être exact** : le préfixe
`vcluster-` (aucun chemin n'atteint la suppression avec un nom non validé), le nom
réservé `manager`, et le fait que personne n'ait `create` sur `vclusters` dans ce
dépôt. Ce n'est **pas** la garde de placement, contrairement à ce qui vaut pour les
marqueurs Velero : là-bas la règle force l'objet dans le namespace de la victime,
donc viser quelqu'un exige déjà des droits chez lui ; ici le namespace des
`VCluster` est plat et unique, donc « l'objet ne parle que de là où il vit » ne
discrimine plus personne. Déléguer `create vclusters` équivaut à donner
`delete namespaces` sur tout `vcluster-*` sans CR — la flotte historique comprise.
Le resserrement qui manque est une `ValidatingAdmissionPolicy` : voir `TODO.md`.

Vérifié par mutation, sur les trois couches : l'étape non jouée, conclure sans
observer, une lecture ratée valant disparition, la borne retirée, le reste non
nommé, un refus de suppression ignoré, l'ordre sauvegarde/destruction inversé, les
avertissements du teardown perdus entre deux étapes, le préfixe `vcluster-` perdu,
`name` et `env` intervertis dans le seam — chacune fait tomber son test. Rien de
tout cela n'a encore tourné sur un vrai cluster : voir `recette-n6-namespace.md`.

Deux angles morts fermés au passage, tous deux trouvés parce qu'ils étaient
**verts** : un test affirmait que la séquence laisse le namespace derrière elle —
il passait encore après N6, parce que le double bascule un booléen en mémoire au
lieu de toucher le cluster ; et la branche « la lecture de la protection n'a pas
abouti » n'était empruntée par aucun test du paquet, donc retirer le garde
`ProtectionKnown` du status ne faisait rien tomber.

**Les accès aux intégrations : après la recette preprod, pas avant.** L'opérateur
n'a ni client Vault, ni Keycloak, ni Rancher, donc les intégrations rendent
`Unknown/NotConfigured` et `Ready` n'atteint jamais le vert pour un vcluster
demandant ArgoCD. Les lui donner est la bonne direction, mais l'ordre compte :
ajouter le token GitLab, le secret client Keycloak et les creds Vault à ce pod
élargit ce qu'une compromission emporte, et le faire sur du code qui n'a jamais
tourné sur un vrai cluster, c'est prendre le risque avant d'avoir la preuve.

Ordre retenu : **recette preprod → correction de ce qu'elle trouve → câblage des
accès.**

## Ce qui n'est pas couvert

- **Les accès aux intégrations, pas le code.** Le code est là : `VaultConfigured` et
  `ArgoCDReady` sont écrites, l'appairage Rancher est piloté, et la map mémoire
  `vaultStates` des handlers est remplacée par des étapes de reconcile. Ce qui manque
  est le **câblage** : `cmd/operator/main.go` ne passe ni client Vault, ni Keycloak,
  ni Rancher, donc les étapes rendent `Unknown/NotConfigured` — honnête, mais `Ready`
  n'atteint jamais le vert pour un vcluster demandant ArgoCD. Décision prise :
  après la recette preprod (voir les arbitrages ci-dessus).
- **`ArgoCDReady` ne couvre que le volet Keycloak.** Le dépôt GitLab et la santé de
  la Kustomization ArgoCD ne sont pas encore vérifiés — la réserve est portée par le
  message de la condition, pas cachée derrière un `True` optimiste.
- **`podCount`** — aucun lecteur ne le remplit. Écrire 0 affirmerait « aucun pod »,
  alors qu'on n'a qu'une absence.
- **Les events Kubernetes** proposés en §3.3 — `VClusterReconciler` n'a pas de
  `record.EventRecorder`.
- **La restaurabilité des sauvegardes** — la recette a vérifié qu'une sauvegarde
  Velero se termine avant destruction, pas qu'elle restaure.
- **Les intégrations pendant la suppression** — Keycloak, Vault et le dépôt
  GitLab sont traversés sans client, donc rapportés comme non traités. C'est
  honnête, et c'est ce que la recette a lu dans le message final ; ça reste à
  câbler (voir les arbitrages).

## La recette réelle a eu lieu — 2026-08-08

Premier passage de la brique opérateur sur un vrai cluster (k3s v1.32.4, infra
montée pour l'occasion). Plan et protocole : `recette-n6-namespace.md`.

| Cas | Ce qu'il tranche | Verdict |
|---|---|---|
| A | le namespace part-il, et le CR le suit-il ? | ✅ sauvegarde Velero `Completed`, namespace disparu, CR lâché **après** |
| B | un namespace retenu bloque-t-il puis débloque-t-il ? | ✅ bloqué 10 min pile, puis `RemovalUnconfirmed` nommant les restes |
| C | que fait Flux quand on supprime son namespace ? | ⚠️ voir ci-dessous |
| E | un redémarrage perd-il la borne ? | ✅ `lastTransitionTime` identique avant/après |

**Ce que la recette a trouvé et qu'aucun test ne pouvait trouver.** Le ClusterRole
de l'opérateur n'avait pas `list` sur les `resourcequotas`, dont dépend l'étape
budget. Conséquence : **aucun `VCluster` n'était réconcilié**, jamais. Et tout
était vert — parce qu'**envtest n'applique pas le RBAC**. C'est la limite
structurelle de la campagne de tests, et elle s'est payée à la première minute du
premier contact avec un vrai API server.

**Cas C — le namespace orphelin est le comportement NORMAL, pas un cas rare.**
La question qu'on ne pouvait pas trancher sans cluster était : Flux réapplique-t-il
plus vite qu'un namespace ne termine ? Réponse mesurée : la Kustomization tenant a
un `interval: 5m`, et la terminaison a pris **45 s**. Donc le namespace disparaît,
l'opérateur le constate, lâche le CR — et Flux recrée un namespace vide quelques
minutes plus tard (constaté à 4 min 20). Il n'est plus mentionné par aucun CR.

Ce n'est pas un échec de N6 : la séquence a constaté ce qu'elle annonçait. C'est
une **contrainte d'ordre** qu'il faut écrire dans le parcours de suppression :
retirer l'arbre du tenant de fluxprod (ou suspendre sa Kustomization) **avant**
de supprimer le CR. Sinon l'orphelin est systématique, pas accidentel.

**Deux faits vérifiés que le code affirmait sans preuve** : l'opérateur
n'apparaît pas dans les `managedFields` du namespace, parce qu'il ne déclare
qu'un nom — et un nom est l'identité de l'objet, pas un champ géré (ce que
`ProvisionFieldManager` soutenait) ; et `protect-deletion` est bien posée par
`kustomize-controller` depuis l'arbre tenant, à `false`.

Verdict inchangé pour la production : **No-Go prod.** Ce qui a tourné est la
séquence de suppression sur des vclusters jetables, pas le cycle complet.

## Dette soldée : les six seams se sont rejoints

Les chantiers récupéraient leurs dépendances de service par **assertion de type sur
`r.Ops`**, pour que cinq chantiers parallèles ne se battent pas sur la même struct.
En production c'était sûr — `var _ X = (*service.Service)(nil)` l'exclut. En test,
un faux qui n'implémentait qu'une moitié **dégradait au lieu d'échouer** : la
branche `!ok` posait une condition « ce contrôleur n'a pas de générateur » et
continuait, donc le test passait au vert en mesurant autre chose que ce qu'il
prétendait. Ça avait déjà mordu une fois.

`Ops` porte maintenant un type unique qui embarque les six interfaces. Un double
incomplet **ne compile plus** — l'incomplétude est redevenue une erreur de
compilation au lieu d'un vert trompeur.

Ce que le refactor a fait tomber en passant, et qui vaut d'être noté : un test
d'agrégation reposait, sans le savoir, sur le fait que les intégrations étaient
sautées pour son faux partiel. C'était exactement le faux vert que ce chantier
ferme. Quatre des cinq branches `!ok` supprimées n'étaient couvertes que par des
tests du seam manquant lui-même — ils ne mesuraient rien d'autre ; la cinquième
n'était couverte par rien.

Fermé au même endroit : les seams passaient `(vc.Name, r.Cell)` en deux `string`
voisines, donc une interversion **compilait** et faisait agir sur le mauvais
vcluster. Les cinq appels du finalizer sont maintenant gardés par un test qui rend
cette mutation mortelle. Un type nommé la rendrait impossible plutôt que testée —
chiffré, pas appliqué : le diff déborderait sur `internal/service` et ses
appelants, largement au-delà de la dette qu'on solde.
