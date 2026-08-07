FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION
RUN VERSION_FILE=$(cat VERSION) && \
    LDFLAGS="-s -w -X github.com/gmalfray/vcluster-manager/internal/version.Version=${VERSION:-$VERSION_FILE}" && \
    CGO_ENABLED=0 go build -ldflags="$LDFLAGS" -o vcluster-manager ./cmd/server && \
    CGO_ENABLED=0 go build -ldflags="$LDFLAGS" -o vcluster-manager-operator ./cmd/operator

# --- Image de l'opérateur -----------------------------------------------------
# Séparée de celle du serveur pour qu'un `kubectl get pods -o wide` dise ce que
# le pod fait. Avec une image unique et deux `command:`, il fallait ouvrir le
# Deployment pour savoir lequel des deux on regardait — mauvais réflexe à imposer
# au moment où quelque chose ne va pas.
#
# Ni templates ni assets web : l'opérateur ne sert aucune page. USER non-root
# dès l'image, pour que ça ne dépende pas du manifeste.
FROM alpine:3.19 AS operator
RUN apk add --no-cache ca-certificates
COPY --from=builder /app/vcluster-manager-operator /usr/local/bin/
USER 65532:65532
ENTRYPOINT ["vcluster-manager-operator"]

# --- Image du serveur ---------------------------------------------------------
# En dernier volontairement : c'est la cible par défaut d'un `docker build .`
# sans `--target`, donc le comportement historique ne change pas.
FROM alpine:3.19 AS server
RUN apk add --no-cache ca-certificates
COPY --from=builder /app/vcluster-manager /usr/local/bin/
COPY --from=builder /app/web /web
ENV TEMPLATE_DIR=/web/templates
ENTRYPOINT ["vcluster-manager"]
