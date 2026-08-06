# Opérateur — controller-runtime sur le chemin backup/restore

> Le code a quitté `poc/operator/` : il vit maintenant dans l'app
> (`api/v1alpha1`, `internal/controller`, `internal/veleroops`, `cmd/operator`,
> `config/{crd,rbac}`), et le module séparé a disparu avec son `replace`.
> Ce document garde la trace de ce que le POC prouvait.

Ce POC répond à une question et une seule : **une boucle de reconcile
controller-runtime peut-elle porter le chemin backup/restore Velero, en
réutilisant `internal/service` tel quel, et survivre à un redémarrage là où les
goroutines actuelles ne survivent pas ?**

Verdict et preuves : [`../../docs/poc-operator-tech-decision.md`](../../docs/poc-operator-tech-decision.md).
Design de référence : [`../../docs/design-backup-restore-annotation.md`](../../docs/design-backup-restore-annotation.md).

## Ce qu'il y a dedans

| Chemin | Rôle |
|---|---|
| `api/v1alpha1/` | CRD marqueur `VClusterVeleroOps` (design §2 option B) — **sans `spec`** : annotations de déclenchement + sous-ressource `status` |
| `internal/veleroops/ops.go` | Le seam vers `internal/service`, écrit **dans les types du service** |
| `internal/veleroops/seam_assert.go` | `var _ Ops = (*service.Service)(nil)` — le vrai service satisfait tout, prouvé à la compilation |
| `internal/controller/` | Le reconciler + la suite envtest (9 tests, dont 3 sur la reprise après interruption) |
| `config/crd/` | Manifeste CRD généré |

Module Go **séparé** (`replace` vers `../..`) : controller-runtime n'entre pas
dans le `go.mod` de l'app tant que l'opérateur n'est pas un vrai chantier.
L'import de `internal/…` reste légal parce que le chemin du module partage le
préfixe du dépôt.

## Lancer

Pas de Go local sur ce poste — tout passe par un conteneur. Le premier `make
test` télécharge les modules, controller-gen et les binaires envtest
(kube-apiserver + etcd) : compter ~2,5 Go de cache sous `~/.cache/poc-go`.

```sh
make test-operator  # suite envtest (kube-apiserver + etcd)
make manifests      # après modification des types (deepcopy + CRD)
make build          # les deux binaires : serveur et opérateur
```

⚠️ `make test` a besoin de quelques Go libres sur `/`. Un disque plein se
manifeste par des `no space left on device` pendant la compilation, pas par une
erreur claire.

## Ce que le POC ne fait pas

Pas de binaire opérateur déployable, pas de RBAC, pas de finalizer, pas de
chemin de suppression — et, dans les tests, aucun appel réel à Velero : la
couche qui parle au cluster est un `fakeOps` scriptable. Ce qui est testé ici,
c'est la **sémantique de reconcile** (idempotence, concurrence, reprise après redémarrage, borne de
renoncement, sous-ressource `status`) ; la séquence de restauration elle-même est couverte par les
tests de `internal/service`.

Rien n'est persisté sur l'avancement de la séquence destructrice : à la reprise, le contrôleur
**relit l'état du cluster** (`Service.InspectInterruptedRestore`). C'est un choix, et il est motivé
dans `../../docs/poc-operator-tech-decision.md` §5bis — un registre écrit peut mentir, le PVC non. Le seam, lui, n'est pas
fictif : le reconciler consomme l'interface que `*service.Service` satisfait
réellement.
