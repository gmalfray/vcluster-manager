# ADR-002 — Le modèle en cells remplace `prod`/`preprod`

> Statut : **acceptée** (2026-08-06) pour le principe et le vocabulaire.
> **Une question reste explicitement ouverte** (§5) : elle n'est pas arbitrée par
> défaut, et plusieurs décisions en dépendent.
> Complète [`adr-001-source-de-verite.md`](adr-001-source-de-verite.md), ne la remplace pas.

## 1. Pourquoi cette ADR existe

Elle existe parce que son absence a déjà coûté quelque chose. Une revue de
simplicité a été menée sur « les environnements en dur » (123 littéraux
`"preprod"`, 24 fichiers) et a conclu **ne pas refactorer**, en s'appuyant sur une
prémisse : « les deux environnements ne sont pas des pairs symétriques ». La
prémisse était exacte au vu du code — et périmée au vu de la cible, qui n'était
écrite nulle part. Le verdict était donc juste et inutilisable.

Une cible d'architecture qui ne vit que dans une tête fait prendre les décisions à
l'aveugle. D'où ce document, court exprès.

## 2. Ce que `prod`/`preprod` mélange aujourd'hui

Deux axes orthogonaux, portés par le même mot :

- **L'hébergement** : sur quel cluster hôte tourne un vcluster.
- **Le cycle de vie** : la promotion, l'état `pending` réservé à prod, la MR
  permanente, la branche `preprod` qui fait foi pour les deux environnements
  (ADR-001), la relation « contrepartie » (`DeleteCounterpart`).

C'est cette confusion qui rend `env` présent partout : il sert tantôt à choisir un
client Kubernetes, tantôt un chemin dans fluxprod, tantôt une branche Git, tantôt
une règle métier asymétrique.

## 3. Décision

**Une « cell » est un cluster hôte kubeadm.** Il y en aura plusieurs — `cell1`,
`cell2`, … — et ce sont des **pairs**, pas des rôles. Chaque cell porte :

- ses **vClusters** (aujourd'hui) ;
- plus tard, les **plans de contrôle managés** des clusters Cluster API : etcd et
  contrôleurs hébergés sur la cell, les nœuds workers ailleurs.

La cell est donc une **unité de capacité et d'hébergement de plans de contrôle**,
pas un étage de cycle de vie.

### Vocabulaire

`cell` est retenu. C'est un nom de patron d'architecture reconnu dans l'industrie,
pas un terme propriétaire. Les alternatives examinées et écartées :

| Terme | Écarté parce que |
|---|---|
`tenant` | déjà pris dans le dépôt : les manifests d'un vcluster (`tenant/cert-manager`, `tenant_flux.yaml`) |
`platform` | déjà pris (`internal/service/platform.go`) |
`fleet` | **Rancher Fleet** existe et Rancher est dans la stack |
`host` / `hostcluster` | le meilleur second choix : c'est déjà le mot des docs (« cluster hôte ») ; `host` seul est surchargé (hostname, ingress) |
`mgmt` | le plus juste techniquement (terme standard CAPI pour un cluster hébergeant des plans de contrôle), mais long, et le mot serait consommé si un cluster central pilotait un jour les autres |
`shard` | sous-entend le découpage d'un même jeu de données ; les cells sont autonomes |
`pool` | confusion avec un « node pool » *dans* un cluster |

Contraintes retenues : le nom sert de **flag**, de **label Kubernetes**, de
**champ de CRD** et de **segment de chemin** dans fluxprod. Donc minuscules,
court, sûr pour du DNS.

## 4. Conséquences

**Un opérateur par cell.** `crd-vcluster.md` §2.1 avait déjà tranché que `env`
n'est pas un champ du CR : il est déduit du cluster où le CR est appliqué, avec un
opérateur par cluster hôte. C'est exactement le modèle en cells. Une cell = un
cluster hôte = une instance d'opérateur. Le discriminant disparaît **par
construction**, sans refactor manuel.

**Le chantier opérateur est le chemin de migration, pas un chantier parallèle.**
Corollaire pratique : ne pas refactorer les 123 littéraux à la main pour aller
vers les cells — c'est du code que ce chantier fait disparaître.

**Les asymétries deviennent des candidates à la suppression, pas au
paramétrage.** Avec N cells paires, l'état `pending`, la MR permanente, la
relation « contrepartie » et le couple de branches n'ont plus de rôle *si* le
cycle de vie disparaît (§5). Retirer coûte moins cher que généraliser.

**Le flag de l'opérateur s'appelle `--cell`**, pas `--env`, et le manifeste le
pose explicitement. Il étiquette l'audit et les métriques : un opérateur qui
annoncerait le nom d'une autre cell serait activement faux.

**Cluster API : l'inconnue de la sauvegarde etcd tombe.**
`etude-cluster-api.md` §9 listait comme point dur « sauvegarde etcd du
control-plane self-managed (Velero ne couvre pas le control-plane comme pour un
vcluster) » — écrit en supposant un `KubeadmControlPlane` sur des VM. Avec un plan
de contrôle **hébergé sur la cell**, son etcd est un StatefulSet avec un PVC dans
un namespace : exactement ce que la machinerie backup/restore actuelle sauvegarde
et restaure déjà (`detectVClusterTopology` distingue d'ailleurs déjà etcd embarqué
et etcd externe — un plan de contrôle hébergé est un troisième cas de la même
famille). Ça renforce le « à ne pas rater » du §10 de l'étude : un CRD `Cluster`
**multi-backend dès le départ** (`type: vcluster | capi`), pour partager non
seulement l'API mais le **substrat** — même cell, même hébergement, même backup,
même opérateur.

## 5. La question ouverte, volontairement non arbitrée

**Que devient l'axe du cycle de vie ?** Les cells partitionnent la capacité ;
elles ne disent rien de la promotion. Deux issues, et le choix détermine
concrètement le travail :

- **Il disparaît** : chaque cell est autonome, un vcluster de test est un vcluster
  comme un autre. Alors `pending`, la MR permanente, `DeleteCounterpart` et le
  couple `preprod`/`master` partent. Scénario le plus simple, de loin.
- **Il survit sous une autre forme** : une cell dédiée au non-prod, ou un attribut
  porté par le vcluster lui-même. La promotion reste, mais cesse d'être portée par
  le nom de l'environnement — ce qui est déjà un gain.

Deux sous-questions qui en découlent : **une branche fluxprod par cell, ou une
branche unique avec un chemin par cell ?** Et **que signifie « la contrepartie »
avec N cells ?** (décision produit avant d'être du code).

## 6. Ce qui reste bon à faire quel que soit le choix

- **Nommer les deux branches** (`sourceBranch = "preprod"`,
  `deployedBranch = "master"`, ~40 littéraux) : au départ un garde-fou — rien
  aujourd'hui n'empêche quelqu'un de « paramétrer proprement » ces sites par
  environnement et de committer des changements prod sur `master`, en contournant
  la promotion, directement dans ce que Flux prod surveille. Dans le modèle en
  cells, ces constantes deviennent en plus **la carte des sites que la migration
  devra revisiter**.
- **Supprimer les 15 replis `env = "preprod"` dupliqués** (−42 lignes) : le helper
  existe déjà deux fois dans le dépôt, il n'a simplement pas été propagé.

## 7. Ce qu'on ne fait pas

Pas de `map[string]CellConfig`, pas de type `Cell`, pas de refactor de
`internal/config` (0 % de couverture, aucun test, client en production). Tant que
§5 n'est pas tranché, généraliser serait du générique spéculatif : on paierait un
coût certain contre un bénéfice dont on ne connaît pas encore la forme.
