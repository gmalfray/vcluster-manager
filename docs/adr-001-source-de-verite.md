# ADR-001 — Source de vérité : le CR `VCluster` versionné (« C allégé »)

- **Statut** : décidé
- **Date** : 2026-08-04
- **Portée** : étape 2 (opérateur/kro) et tout ce qui en dépend — générateur, promotion prod, suppression

## Contexte

Trois modèles étaient sur la table :

- **A — Git de YAML rendu** (l'existant) : l'app génère 9 à 17 fichiers par vcluster et les commite
  dans le repo GitOps. Flux applique. La promotion en prod passe par une MR `preprod → master`.
- **B — CRD in-cluster pur** : l'app crée un objet `VCluster` directement dans l'API Kubernetes,
  l'opérateur l'expand. Plus de Git du tout.
- **C — GitOps du CR** : l'app commite un CR `VCluster` compact dans le repo GitOps, Flux l'applique,
  kro (+ un contrôleur Go) l'expand en ressources.

## Décision

**C, en version allégée** : le CR `VCluster` est la source de vérité, versionné dans le repo GitOps,
appliqué par Flux, expansé côté cluster. On garde l'historique Git ; on supprime la cérémonie de
merge request.

### Ce qu'on retire

La MR permanente `preprod → master` et toute sa machinerie applicative
(`commitProdMRActions`, la détection de MR ouverte, les badges « MR en attente », l'état
« pending prod »).

Motif : ce n'est pas une revue, c'est un tampon. La MR est unique et permanente, elle accumule les
changements de tout le monde, son diff mélange les chemins `preprod/` et `prod/` — au point que sa
description doit avertir que seuls les fichiers `prod/` comptent — et ce qu'elle donne à relire est
du YAML **généré** à partir d'un formulaire. Le relecteur ne peut ni influencer le rendu ni
raisonnablement le vérifier. À plusieurs, le lot devient plus hétérogène, donc le tampon plus
automatique : la cérémonie ne se justifie pas davantage à mesure que l'équipe grandit.

### Ce qu'on garde, et pourquoi

- **L'historique Git** : c'est ce qui distingue C de B. Qui a demandé quoi, quand, et le `revert`
  qui va avec.
- **La possibilité d'un gate**, mais déplacée au bon endroit : une protection de branche sur le repo
  GitOps, qui fait relire **un CR de vingt lignes**. Le diff devient la décision — nom, quotas,
  ArgoCD, TTL — au lieu d'être le rendu. C'est le seul format de revue qui a une chance d'être lu.
  Ce gate est une politique du dépôt, pas une fonctionnalité de vcluster-manager.

## Ce que la MR protégeait vraiment, et par quoi on la remplace

La MR servait deux intentions différentes, traitées avec le même outil : **contrôler la création**
de vclusters, et **se prémunir d'une suppression accidentelle**. Elle s'acquittait mal des deux,
parce qu'un même « approve » sur un lot de YAML généré ne peut pas être à la fois une décision
d'allocation de ressources et un garde-fou de destruction.

Le principe qui guide le remplacement : **pour la création, remplacer la cérémonie par de la
règle ; pour la suppression, remplacer la prévention par de la réversibilité.** Une revue humaine
cherche à empêcher l'erreur ; elle est approuvée par des gens fatigués qui lisent du YAML. Une
sauvegarde et un délai rendent l'erreur survivable — et une restauration fonctionne à trois heures
du matin.

### Contrôler la création — par la règle

| Contrôle | Ce qu'il attrape | Ce que la MR n'attrapait pas |
|---|---|---|
| Schéma OpenAPI du CR (nom, enum d'environnement, bornes de quotas) | Les valeurs hors normes, au moment de la saisie | La MR le voyait après coup, dans du YAML rendu |
| **Budget de ressources par cluster hôte** : l'opérateur refuse un CR qui ferait dépasser un seuil d'allocation | La saturation progressive du cluster | Aucun relecteur ne calcule de tête la somme des quotas déjà alloués |
| RBAC : qui crée, dans quel environnement | L'autorisation | Déjà couvert, mais après le fait |
| Propriétaire obligatoire sur le CR (label/champ `owner`) | Les vclusters orphelins | Rien |

Le budget par cluster est le contrôle qui compte : c'est précisément ce qu'un humain ne sait pas
faire de façon fiable, et c'est le vrai risque de la création libre.

### Se prémunir de la suppression — par la réversibilité

1. **`spec.deletionProtection: true`** sur le CR. Supprimer demande d'abord un commit qui passe le
   champ à `false`. Deux gestes, deux traces Git, aucun humain à attendre. C'est le modèle des
   fournisseurs cloud sur les bases de données, et il vaut mieux qu'un gate : il ne bloque personne
   mais rend l'accident impossible en un seul geste distrait.
2. **Sauvegarde Velero obligatoire avant destruction**, avec attente de la phase `Completed` avant
   de laisser la séquence continuer. Si Velero n'est pas configuré ou si la sauvegarde échoue, la
   suppression exige un champ d'override explicite : on force la personne à écrire qu'elle accepte
   de détruire sans filet.
3. **Délai de grâce (corbeille)** : le CR marqué pour suppression suspend Flux et descend le
   vcluster à zéro réplica, mais la destruction réelle n'a lieu qu'après N jours, annulable d'ici
   là. C'est la mesure la plus efficace contre l'accident, parce qu'elle transforme une erreur
   irréversible en erreur rattrapable — sans jamais faire attendre quelqu'un qui sait ce qu'il fait.
4. **Finalizer** qui orchestre la séquence : dépairage Rancher, sauvegarde, retrait de la protection
   de namespace, puis destruction. C'est la mécanique Kubernetes qui garantit qu'aucune étape n'est
   sautée, y compris si l'opérateur redémarre au milieu.
5. Les garde-fous existants restent : saisie du nom en confirmation, RBAC admin, webhook de
   protection de namespace.

Bonne nouvelle sur le coût : l'application possède déjà la plupart des pièces — le toggle de
protection de namespace, le déclenchement de sauvegarde Velero avec attente de `Completed`
(écrit pour la restauration in-place), le dépairage Rancher avant destruction, la confirmation par
saisie du nom. Il s'agit surtout de les câbler sur le cycle de vie du CR, pas d'écrire une
mécanique nouvelle.

### Et si on veut quand même un gate humain

Il a sa place, mais pas dans l'application : une protection de branche avec CODEOWNERS sur
`clusters/prod/**` du repo GitOps. Elle ne se déclenche que sur la production, elle fait relire un
CR de vingt lignes, et elle ne demande à vcluster-manager d'orchestrer quoi que ce soit.

### Condition de bascule (non négociable)

Tant que le finalizer et la politique de suppression n'existent pas, on ne retire pas la MR du
chemin de suppression. L'ordre de construction est : finalizer et `deletionProtection` d'abord,
retrait de la MR ensuite. Pas l'inverse.

## Conséquences

**Ce qui disparaît, et c'est le vrai gain.** `generator.go` n'a plus à produire l'arborescence
complète : le CR remplace 9 à 17 fichiers. Avec lui disparaît la règle la plus coûteuse du projet —
garder le générateur synchronisé avec les fichiers déjà présents dans le repo GitOps, sous peine
qu'une régénération écrase une configuration ajustée à la main.

**Ce qui reste du code Go.** kro ne matérialise que des ressources Kubernetes. Créer un client
Keycloak, créer un repo GitLab, commiter, importer un cluster dans Rancher, configurer un backend
auth Vault, lire un statut à l'intérieur d'un vcluster : tout cela reste du Go, dans la couche
service extraite à l'étape 1, appelée par un contrôleur. La cible est **hybride**, pas « kro
remplace l'application ».

**Ce que devient l'app.** Elle écrit un CR au lieu d'écrire une arborescence, et elle lit le
`status` du CR au lieu d'interroger HelmRelease et Kustomization. La couche service et l'API REST
ne changent pas de forme : `Create` construit un CR, `Delete` le supprime.

## Risques

- **Migration des vclusters existants** : chacun doit être converti en CR. Tout ce qui a été ajusté
  à la main dans le repo GitOps et que le générateur ignore sera perdu à la première réconciliation.
  Un inventaire des écarts entre le rendu du générateur et les fichiers réels est un **prérequis**,
  pas une étape de nettoyage a posteriori.
- **L'opérateur devient un point de passage unique** pour tous les vclusters. Il lui faut un `status`
  exploitable et des events, sinon un échec silencieux est invisible.
- **Maturité de kro** : à valider par un POC sur un vcluster preprod avant tout engagement. Si le
  POC échoue, la décision de source de vérité tient quand même — c'est l'expansion qui repasserait
  sur du controller-runtime.
- **Dérive** : les ressources expansées ne doivent être modifiables que par l'opérateur (prune Flux
  actif, ownership propre), sinon la source de vérité redevient le cluster.

## Suite

1. Concevoir le CR `VCluster` — avec un discriminant `type: vcluster | capi` dès le départ, pour ne
   pas avoir à casser le schéma quand Cluster API arrivera (voir `docs/etude-cluster-api.md`).
2. POC kro sur un vcluster preprod : expansion du graphe, `status`, prune.
3. Finalizer et politique de suppression, condition du retrait de la MR.
4. Inventaire des écarts sur les vclusters existants, puis migration.
