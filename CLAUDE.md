# CLAUDE.md — vCluster Manager

Ce fichier est lu par Claude Code à l'ouverture du projet. Les règles
opérationnelles communes (build, conventions, contraintes) vivent dans
[`AGENTS.md`](AGENTS.md) — un format partagé avec d'autres assistants
(Copilot, Cursor, etc.).

## Source de vérité

| Fichier | Contenu |
|---|---|
| [`AGENTS.md`](AGENTS.md) | Règles agent : build/test, contraintes non négociables, conventions |
| [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) | Patterns techniques détaillés (versions, cache, Velero, Rancher, etc.) |
| [`CHANGELOG.md`](CHANGELOG.md) | Historique des releases |
| [`TODO.md`](TODO.md) | Backlog d'évolutions |
| `FORK.md` | Variables d'environnement et portabilité (gitignored) |

## Spécifique Claude

- **Langue** : répondre en français (orthographe accentuée). Les noms de
  symboles, identifiants et commandes restent inchangés.
- **Skills pertinents pour ce repo** :
  [`cc-skills-golang`](https://github.com/samber/cc-skills-golang) —
  golang-error-handling, golang-context, golang-concurrency, golang-observability,
  golang-security, golang-testing, golang-modernize. À mobiliser quand le
  contexte du changement les déclenche.
- **Avant tout commit** : `make check` (build + vet + tests + lint). La règle
  est rappelée dans `AGENTS.md` mais réitérée ici car Claude lit ce fichier
  en premier.

## Repo GitOps associé

Le repo GitOps (« fluxprod » dans la doc interne) contient les manifests
FluxCD pour déployer vcluster-manager et les configurations des vclusters par
environnement (`clusters/{env}/vclusters/{name}/`). Voir `FORK.md` §3 pour la
structure attendue.
