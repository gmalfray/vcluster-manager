#!/usr/bin/env bash
# Reformate le fichier Go qui vient d'être écrit, puis vet son paquet.
#
# Déterministe : exécuté par le harnais après chaque Edit/Write, donc impossible
# à oublier. C'est la seule garantie qui ne dépende pas de la bonne volonté du
# modèle — et cette session a montré ce que ça vaut : un `go vet` cassé par un
# mutex passé par valeur est passé inaperçu jusqu'à ce qu'une campagne de
# mutation le déterre, alors qu'il aurait fait échouer la CI au push.
#
# Tout passe par Docker : il n'y a PAS de Go sur cette machine (ni golangci-lint,
# ni controller-gen, ni setup-envtest). Aucune cible du Makefile n'est exécutable
# en l'état, donc un hook qui appellerait `gofmt` directement ne ferait rien.
#
# Périmètre volontairement étroit — gofmt sur le fichier, vet sur son seul
# paquet, ~2 à 4 s. Le lint complet et `go test -race ./...` vivent dans la CI
# (.github/workflows/build.yaml, job `check`), où la latence ne coûte à personne.
# Les mettre ici ferait payer 60 à 90 s à chaque fin de tour, sur chacun des
# agents lancés en parallèle.
set -euo pipefail

# Le harnais passe le contexte de l'outil en JSON sur stdin.
payload=$(cat)
file=$(printf '%s' "$payload" | jq -r '.tool_input.file_path // empty' 2>/dev/null || true)

# Rien à faire si ce n'est pas un fichier Go de ce dépôt.
[ -n "$file" ] || exit 0
case "$file" in
  *.go) ;;
  *) exit 0 ;;
esac
[ -f "$file" ] || exit 0

repo=$(git rev-parse --show-toplevel 2>/dev/null) || exit 0

# realpath des DEUX côtés avant de comparer.
#
# Ce dépôt est atteignable par deux chemins — ~/GIT/github/vcluster-manager est un
# symlink vers ~/Outils/workspaces/GIT/github/vcluster-manager. Le harnais peut
# donc passer un chemin qui ne commence pas par ce que rend `git rev-parse`, et
# une comparaison de préfixe brute échoue sans rien dire. C'est exactement le
# piège des deux clones, un cran plus bas.
file=$(realpath "$file" 2>/dev/null) || exit 0
repo=$(realpath "$repo" 2>/dev/null) || exit 0

case "$file" in
  "$repo"/*) rel="${file#"$repo"/}" ;;
  *)
    # Hors du dépôt : légitime (un .go ailleurs sur la machine), on ne fait rien.
    # Mais on le DIT, plutôt que de disparaître en silence.
    echo "hook go-fmt-vet : $file est hors de $repo, ignoré" >&2
    exit 0
    ;;
esac

pkg=$(dirname "$rel")

# --user : sans ça le conteneur écrirait des fichiers root dans le dépôt.
# Les caches sont partagés avec les autres invocations, sinon chaque appel
# recompilerait le monde.
# La sortie est CAPTURÉE, pas passée dans un pipe à l'intérieur du conteneur.
#
# `go vet … | head -20` donnerait au pipeline le statut de `head`, qui réussit
# toujours : le hook rendait donc 0 sur un paquet cassé. Le `sh` de l'image est
# dash, qui ne connaît pas `pipefail`, donc le `set -o pipefail` d'ici n'y change
# rien. On borne l'affichage dehors, une fois le vrai code retour connu.
if out=$(docker run --rm \
  -v "$repo":/src \
  -v "$HOME/.cache/poc-go/gopath":/gopath \
  -v "$HOME/.cache/poc-go/build":/gocache \
  -e GOPATH=/gopath -e GOCACHE=/gocache -e HOME=/gopath -e GOFLAGS=-mod=mod \
  --user "$(id -u):$(id -g)" \
  -w /src golang:1.25 \
  sh -c "gofmt -w '$rel' && go vet './$pkg/' 2>&1"); then
  exit 0
fi

# Sortie non nulle : vet a trouvé quelque chose, ou l'outillage a échoué. Les deux
# méritent d'être dits — un paquet cassé qu'on laisse passer en silence fait
# échouer la CI plus tard, sur du code qu'on croyait vert.
printf '%s\n' "$out" | head -20 >&2
echo "hook go-fmt-vet : go vet a échoué sur ./$pkg/ — à corriger avant de continuer" >&2
exit 2
