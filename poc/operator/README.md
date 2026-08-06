# POC opérateur — controller-runtime sur le chemin backup/restore

Ce POC répond à une question et une seule : **une boucle de reconcile
controller-runtime peut-elle porter le chemin backup/restore Velero, en
réutilisant `internal/service` tel quel, et survivre à un redémarrage là où les
goroutines actuelles ne survivent pas ?**

Verdict et preuves : [`../../docs/poc-operator-tech-decision.md`](../../docs/poc-operator-tech-decision.md).
Design de référence : [`../../docs/design-backup-restore-annotation.md`](../../docs/design-backup-restore-annotation.md).

## Ce qu'il y a dedans

| Chemin | Rôle |
|---|---|
| `api/v1alpha1/` | CRD marqueur `VClusterVeleroOps` (design §2 option B) — annotations de déclenchement + sous-ressource `status` |
| `internal/veleroops/ops.go` | Le seam vers `internal/service`, écrit **dans les types du service** |
| `internal/veleroops/seam_assert.go` | Assertions à la compilation : ce que `*service.Service` satisfait déjà, et ce qui manque |
| `internal/controller/` | Le reconciler + la suite envtest (8 tests) |
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
make test      # suite envtest complète
make generate  # après modification des types
make build
make clean-cache
```

⚠️ `make test` a besoin de quelques Go libres sur `/`. Un disque plein se
manifeste par des `no space left on device` pendant la compilation, pas par une
erreur claire.

## Ce que le POC ne fait pas

Pas de binaire opérateur déployable, pas de RBAC, pas de finalizer, pas de
chemin de suppression — et aucun appel réel à Velero : la couche qui parle au
cluster est un `fakeOps` scriptable. Ce qui est testé, c'est la **sémantique de
reconcile** (idempotence, concurrence, reprise après redémarrage,
sous-ressource `status`), pas l'intégration Velero, déjà couverte par les tests
de `internal/service`.
