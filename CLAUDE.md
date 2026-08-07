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
- **Avant tout commit** : `make check` — c'est `vet` + `test` + `lint`, **sans
  build**. ⚠️ Mais il n'y a **pas de Go sur cette machine** : aucune cible du
  Makefile n'est exécutable en l'état, tout passe par Docker. L'invocation exacte
  (avec les caches partagés) est dans `.claude/hooks/go-fmt-vet.sh`.
- **Le harnais s'en charge en partie** : un hook `PostToolUse` reformate chaque
  `.go` écrit et `vet` son paquet (~2-4 s). Le lint complet et
  `go test -race ./...` tournent en CI, job `check` de
  `.github/workflows/build.yaml`, qui conditionne la publication des images.
  Ce qui reste à ta charge et que rien n'automatise : la **vérification par
  mutation** — annuler chaque décision et confirmer qu'un test tombe.

## Équipe d'agents

Les fiches vivent dans `.claude/agents/` et sont **versionnées** : ce sont des
règles de contribution, pas des préférences de poste. Chacune porte deux choses à
respecter :

- ses **`tools`** — `code-reviewer`, `security-auditor` et `simplificateur` sont
  en **lecture seule** (un auditeur qui corrige lui-même n'a plus personne pour
  relire son correctif), et **aucun agent n'a `Agent`** : pas de cascade, celui
  qu'on lance fait le travail ;
- ses **frontières de fichiers**, avec la liste des fichiers carrefour à ne pas
  toucher sans demande explicite. Cette règle existe parce que cinq chantiers
  parallèles se sont accrochés au même fichier et se sont écrasés.

## Repo GitOps associé

Le repo GitOps (« fluxprod » dans la doc interne) contient les manifests
FluxCD pour déployer vcluster-manager et les configurations des vclusters par
environnement (`clusters/{env}/vclusters/{name}/`). Voir `FORK.md` §3 pour la
structure attendue.
