package gitops

import (
	_ "embed"
	"fmt"
)

//go:embed assets/argocd.png
var argocdAvatarPNG []byte

// GenerateAppManifestsREADME generates a README.md for an app-manifests repo.
func GenerateAppManifestsREADME(name string, gl *GitLabClient) string {
	vaultAddr := gl.vaultAddr
	if vaultAddr == "" {
		vaultAddr = "https://vault.example.com"
	}
	rootAppsPath := gl.vaultKVArgoCDRootApps
	if rootAppsPath == "" {
		rootAppsPath = "secret/data/vcluster/argocd/rootapps"
	}
	repoPath := gl.vaultKVArgoCDRepo
	if repoPath == "" {
		repoPath = "secret/data/vcluster/argocd/repo"
	}
	return fmt.Sprintf(`# app-manifests-%s

Repo de manifestes ArgoCD pour le vcluster **%s**.

## Pattern App of Apps

Ce repo utilise le pattern [App of Apps](https://argo-cd.readthedocs.io/en/stable/operator-manual/cluster-bootstrapping/#app-of-apps-pattern) :

- Le dossier racine contient les fichiers `+"`Application`"+` ArgoCD de premier niveau
- Chaque `+"`Application`"+` pointe vers un sous-dossier ou un repo externe contenant les manifestes de l'application cible

## Branches

| Branche | Environnement | ArgoCD |
|---------|--------------|--------|
| `+"`master`"+` | Production | [argocd.%s.%s](https://argocd.%s.%s) |
| `+"`preprod`"+` | Preproduction | [argocd.%s.%s](https://argocd.%s.%s) |

## Acces SSH et credentials

### Acces de ce repo par ArgoCD (rootapps)

La cle SSH permettant a ArgoCD d'acceder a ce repo (`+"`app-manifests-%s`"+`) est geree automatiquement
par vault-webhook. Elle est stockee dans Vault KV (`+"`"+rootAppsPath+"`"+`)
et injectee a la creation du Secret ArgoCD dans le vcluster. Aucune action manuelle n'est requise.

La deploy key GitLab (ID #%d) est activee automatiquement par vcluster-manager lors de la creation du repo,
donnant un acces en lecture seule a FluxCD.

### Credentials pour les repos applicatifs

Pour qu'ArgoCD accede a d'autres repos depuis ce vcluster, creer un Secret dans ce repo avec les
annotations vault-webhook. La cle SSH est stockee dans Vault KV (`+"`"+repoPath+"`"+`) :

`+"```yaml"+`
apiVersion: v1
kind: Secret
metadata:
  name: mon-repo
  namespace: argocd
  labels:
    argocd.argoproj.io/secret-type: repository
  annotations:
    vault.security.banzaicloud.io/vault-addr: "%s"
    vault.security.banzaicloud.io/vault-path: "kubernetes-vcluster-%s-<env>"
    vault.security.banzaicloud.io/vault-role: "cert-manager"
    vault.security.banzaicloud.io/vault-skip-verify: "false"
stringData:
  url: %s/ops/mon-repo.git
  sshPrivateKey: "vault:`+repoPath+`#sshPrivateKey"
  insecure: "true"
`+"```"+`

Remplacer `+"`<env>`"+` par `+"`prod`"+` (branche `+"`master`"+`) ou `+"`preprod`"+` (branche `+"`preprod`"+`).

## Usage

1. Creer un fichier YAML `+"`Application`"+` a la racine du repo
2. Committer sur la branche correspondant a l'environnement cible
3. ArgoCD detecte automatiquement le changement et deploie

## Exemple

`+"```yaml"+`
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: mon-app
  namespace: argocd
spec:
  project: default
  source:
    repoURL: %s/ops/mon-app.git
    targetRevision: HEAD
    path: deploy
  destination:
    server: https://kubernetes.default.svc
    namespace: mon-app
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
`+"```"+`
`,
		name, name,
		name, gl.domainProd, name, gl.domainProd,
		name, gl.domainPreprod, name, gl.domainPreprod,
		name,
		gl.fluxDeployKeyID,
		vaultAddr, name, gl.gitlabSSHURL,
		gl.gitlabSSHURL,
	)
}
