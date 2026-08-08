# Le thème de login Keycloak ne vit pas dans ce dépôt

Il existe, il tourne, et sa source est **ailleurs** — c'est la seule chose que ce
document a besoin de dire, mais elle mérite d'être écrite parce que l'inverse a
déjà été tenté.

## Où il est

`vcluster-manager-infra`, dans `terraform/02-platform/keycloak-theme/vcluster-manager/login/` :

| Fichier | Rôle |
|---|---|
| `theme.properties` | déclare le thème, `parent=keycloak` |
| `resources/css/login.css` | remplace entièrement le CSS PatternFly du parent |
| `resources/img/logo.svg` | le mark Rebuild IT + le lockup produit |

`terraform/02-platform/keycloak.tf` lit ces trois fichiers avec `file()`, en fait un
ConfigMap monté sur le pod Keycloak, et le realm est configuré avec
`login_theme=vcluster-manager`.

## Pourquoi on n'en garde pas une copie ici

Parce qu'elle serait **inerte**. Terraform lit sa propre copie : un thème modifié
dans ce dépôt-ci n'aurait aucun effet sur la page de login, tout en ayant l'air
d'être la source. Les deux divergeraient en silence, et c'est précisément ce que
l'ADR-001 refuse — une seconde source de vérité qui a l'air d'en être une.

La tentation est réelle : le thème habille *ce* produit, sa place *semble* être ici.
Mais ce qui décide, c'est qui l'applique. C'est Terraform, donc c'est chez lui.

Le jour où ce raisonnement change — par exemple si le thème est packagé dans une
image plutôt que monté en ConfigMap — c'est le mécanisme de déploiement qu'il
faudra déplacer d'abord, pas les fichiers.

## Modifier le thème

Éditer les fichiers dans le repo d'infra, puis `make platform` (ou
`terraform apply` dans `02-platform`). Le ConfigMap est recalculé, le pod Keycloak
redémarré par le hash de son contenu.

Ce qui n'est pas vérifiable automatiquement, et qui demande donc un coup d'œil réel
après chaque changement : le rendu (proportions du logo, alignement), l'écran
d'erreur d'authentification, et les contrastes. Le reste — validité du
`theme.properties`, chemins des ressources, CSS équilibré — se vérifie sans
navigateur.
