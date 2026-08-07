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
| suppression | `vcluster_finalizer.go` | finalizer, garde-fou, sauvegarde puis destruction |

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

**N6 — le finalizer supprime le namespace qu'il a créé.** L'étape `Destroying` ne
supprimait rien : c'était le prune Flux d'un commit que le finalizer n'écrit ni ne
vérifie, donc il annonçait « suppression terminée » sans preuve. Des deux issues
possibles — supprimer soi-même, ou attendre de constater que Flux l'a fait — c'est
la première qui est retenue.

Raison : l'opérateur applique déjà ce namespace lui-même en Server-Side Apply, il
en est donc propriétaire de fait. Le faire supprimer par lui rend la séquence
auto-suffisante et vérifiable, au lieu d'introduire une attente qui peut ne jamais
aboutir et qu'il faudrait borner. Conséquence à traiter dans le même changement :
le commentaire de `ProvisionFieldManager` affirme que Flux n'écrit aucun des deux
objets appliqués — c'est **faux pour le namespace**, qui vient aussi de
`clusters/<cell>/base`. La propriété doit donc être tranchée explicitement là.

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
- **`GetNamespaceProtection`** rend `false` aussi bien pour « pas d'annotation » que
  pour « namespace illisible ». Le correctif est de lui faire rendre `(bool, error)`.
- **N6, décidé mais pas encore implémenté** : le finalizer doit supprimer le
  namespace qu'il a créé. Voir les arbitrages ci-dessus.
- **La recette réelle** — rien de tout cela n'a tourné sur un vrai cluster depuis la
  fusion des chantiers. L'infra est coupée. Verdict de recette : **No-Go prod, Go
  recette preprod sur vclusters jetables.**

## Dette assumée

Les chantiers récupèrent leurs dépendances de service par **assertion de type sur
`r.Ops`** (`VClusterObserver`, `VClusterProvisioner`, `VClusterDeletionOps`) plutôt
que par un champ, pour que cinq chantiers parallèles ne se battent pas sur la même
struct. Un `var _ X = (*service.Service)(nil)` empêche la pourriture silencieuse en
production. Mais en test, un faux qui n'implémente pas l'interface **dégrade au lieu
d'échouer** — ça a déjà mordu une fois à la fusion. Les quatre interfaces doivent se
rejoindre maintenant que les chantiers ont atterri.
