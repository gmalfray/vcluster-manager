# Étude (future extension) — provisionner de VRAIS clusters via Cluster API

> ⚠️ **Étude seulement — rien à implémenter.** À garder en tête pour le design de
> l'opérateur (notamment l'abstraction `Cluster`), afin de ne pas se peindre dans
> un coin en ne pensant qu'aux vClusters.

## 1. Motivation

Aujourd'hui vcluster-manager gère des **vClusters** : des clusters *virtuels* dans
un cluster hôte, déployés en `HelmRelease` + FluxCD. La piste future : gérer aussi
de **vrais clusters** (VM / bare-metal / cloud) avec le même socle GitOps.

Une chaîne Terraform provisionne déjà un cluster réel sur **Hetzner** (voir le
dépôt `vcluster-manager-infra` : K3s + FluxCD + Keycloak + Vault + Rancher).
**Cluster API (CAPI)** est l'alternative *déclarative, Kubernetes-native et
self-healing* à cette chaîne Terraform — et elle s'intègre nativement à FluxCD,
donc à notre modèle.

## 2. Cluster API en bref

- **Management cluster** : héberge les contrôleurs CAPI (chez nous : le cluster
  hôte où tourne déjà vcluster-manager).
- **Workload clusters** : les vrais clusters que CAPI provisionne.
- **CRDs cœur** : `Cluster`, `Machine`, `MachineSet`, `MachineDeployment`,
  `MachineHealthCheck`, et **`ClusterClass`** (clusters *templatés* : on définit une
  classe une fois, on l'instancie avec des variables).
- **Providers** (3 rôles composables) :
  - *Infrastructure* : Hetzner (**CAPH**), AWS/Azure/GCP, vSphere, Proxmox,
    Metal3, **Docker (CAPD)** pour les tests.
  - *Bootstrap* : Kubeadm (CABPK).
  - *Control-plane* : `KubeadmControlPlane` (self-managed) ou managé (EKS/AKS/GKE).
- Le kubeconfig d'un cluster est publié par CAPI dans un **secret
  `<cluster>-kubeconfig`**.

## 3. Le provider qui nous concerne : CAPH (Hetzner)

**Cluster API Provider Hetzner** (Syself), **GA v1.0 depuis oct. 2024**. Gère à la
fois **Hetzner Cloud (hcloud)** et **bare-metal (Robot)**, en mode pur-cloud ou
**hybride** (control-plane cloud + workers dédiés). Provisionnement automatisé +
self-healing. → C'est le candidat naturel vu notre infra. (CAPD/Docker pour un POC
sans coût, Proxmox/vSphere si on-prem un jour.)

## 4. Pourquoi ça colle à notre architecture

**CAPI + Flux = exactement notre modèle GitOps.** On commit des manifests CAPI
(`Cluster` + instance de `ClusterClass` + templates infra Hetzner) dans `fluxprod`,
Flux les applique, les contrôleurs CAPI provisionnent le cluster.

Mieux : **`ClusterClass` = un spec compact instancié avec des variables** — c'est le
miroir exact de notre `generator.go` et du **Modèle C** (commit d'un petit CR →
Flux l'applique → l'opérateur/**kro** l'expand). kro pourrait matérialiser le graphe
CAPI depuis un spec compact, comme pour les vClusters → ça **renforce le choix kro**.

## 5. Insertion dans vcluster-manager (cible opérateur)

Introduire une **abstraction unifiée `Cluster`** avec un `spec.type` :

```
type: vcluster   →  génère HelmRelease + tenant (l'existant)
type: capi       →  génère Cluster + ClusterClass instance + templates Hetzner
```

Le futur CRD `Cluster` de l'opérateur porterait ce `type`. **Réutilisable presque
tel quel** : appairage **Rancher** (import du cluster CAPI), **Velero**, **Vault**,
tenant **ArgoCD**, polling de status. L'**API/UI** gagnent juste une notion de
« backend ». → **À prévoir dès le design du CRD** pour rester multi-backend.

## 6. Différences de cycle de vie vs vcluster (les vrais points durs)

| Sujet | vCluster (aujourd'hui) | Cluster réel (CAPI) |
|---|---|---|
| Provisionnement | secondes, gratuit | **minutes, facturé** (vraie infra) |
| Kubeconfig | secret `vc-…-ext` | secret CAPI `<cluster>-kubeconfig` |
| Scaling | ResourceQuota CPU/mem | `MachineDeployment.replicas` (nœuds) |
| Upgrade K8s | bump de chart | rolling CAPI (KCP + MD), plus complexe |
| Santé | HelmRelease Ready | `Machine` / `MachineHealthCheck` + remediation |
| Quotas | CPU/mem/stockage | **nb de VM / coût** |
| Suppression | delete HelmRelease | **drain + déprovisionnement infra (€)** |

→ La **protection contre la suppression** (déjà en place) devient encore plus
critique : une suppression accidentelle détruit de vraies machines facturées.

## 7. Prérequis day-0

Sur le management cluster (l'hôte) : installer **CAPI core + CAPH** + secrets
Hetzner (token hcloud, creds Robot, clés SSH). Via `clusterctl` **ou** le
**Cluster API Operator** (déclaratif, Flux-friendly — cohérent avec notre GitOps).
Analogue au bootstrap Terraform actuel (`vcluster-manager-infra`).

## 8. Relation avec l'infra Terraform existante

CAPI **remplace/complète** la chaîne Terraform de `vcluster-manager-infra`
(provisioning du cluster K3s Hetzner) : déclaratif, self-healing, une seule
source Git. **Coexistence à penser** (ne pas casser l'existant) ; **Rancher
reste la console** (import des clusters CAPI).

## 9. Inconnues à lever (POC futur, quand on décidera)

- Maturité **CAPH bare-metal (Robot)** sur nos usages.
- Réseau Hetzner : LB hcloud, floating IPs, CNI retenu.
- ~~**Sauvegarde etcd** du control-plane self-managed (Velero ne couvre pas le
  control-plane comme pour un vcluster).~~ → **levée par ADR-002** : le plan de
  contrôle est **hébergé sur la cell** (etcd + contrôleurs sur le cluster hôte
  kubeadm, workers ailleurs), donc son etcd est un StatefulSet avec un PVC dans un
  namespace — exactement ce que la machinerie backup/restore actuelle sait faire.
  `detectVClusterTopology` distingue déjà etcd embarqué et etcd externe ; un plan
  de contrôle hébergé est un troisième cas de la même famille. Cette topologie
  n'était pas envisagée ici : §3 supposait un `KubeadmControlPlane` sur des VM.
- Jour-2 : upgrades, remediation, coût/quotas Hetzner.
- Un **POC CAPD (Docker)** d'abord (zéro coût) pour valider le flux
  `spec compact → (kro?) → CAPI → Flux`, avant Hetzner.

## 9bis. Ce que le modèle en cells change (ADR-002)

Ce document a été écrit en supposant deux environnements et un control-plane sur
des VM Hetzner. La cible retenue depuis est différente : **N cells** (clusters
hôtes kubeadm, pairs) qui portent leurs vClusters **et** les plans de contrôle
managés des clusters CAPI. Conséquences sur cette étude :

- Le §9 « sauvegarde etcd » tombe (ci-dessus).
- Le choix « CRD `Cluster` multi-backend dès le départ » (§10) devient **plus**
  important, pas moins : les deux backends partagent la cell, l'hébergement du
  plan de contrôle, la machinerie de backup et l'opérateur. Ce n'est plus
  seulement une API commune, c'est un substrat commun.
- Le §5 (« insertion dans vcluster-manager ») garde sa forme, mais la dimension
  `env` y devient `cell`, et elle est déduite du cluster où le CR est appliqué au
  lieu d'être un champ.

## 10. Verdict

Extension **naturelle et cohérente** avec la cible `UI → API → opérateur → Flux` :
mêmes briques GitOps, mêmes intégrations, seul le « backend » de matérialisation
change. **Rien à faire maintenant** — la seule décision à ne pas rater : concevoir
le futur CRD `Cluster` **multi-backend dès le départ** (`type: vcluster | capi`),
pour que l'ajout de CAPI soit un provider de plus, pas une refonte.

## Sources

- [Cluster API Provider Hetzner (Syself) — GitHub](https://github.com/syself/cluster-api-provider-hetzner)
- [CAPH — Introduction & versions compatibles](https://syself.com/docs/caph/getting-started/introduction)
- [CAPH — bare metal (Robot)](https://syself.com/docs/caph/topics/baremetal/introduction)
- [Provisionner des clusters avec Cluster API + Flux](https://oneuptime.com/blog/post/2026-03-06-use-cluster-api-flux-cd/view)
- [Deploy Cluster API Operator & providers with Flux](https://wieland.tech/blog/deploy-cluster-api-operator-providers-with-flux)
- [Kubernetes at scale with GitOps and Cluster API — Microsoft](https://opensource.microsoft.com/blog/2023/04/20/kubernetes-at-scale-with-gitops-and-cluster-api/)
