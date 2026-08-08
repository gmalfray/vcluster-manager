FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION
RUN VERSION_FILE=$(cat VERSION) && \
    LDFLAGS="-s -w -X github.com/gmalfray/vcluster-manager/internal/version.Version=${VERSION:-$VERSION_FILE}" && \
    CGO_ENABLED=0 go build -ldflags="$LDFLAGS" -o vcluster-manager ./cmd/server && \
    CGO_ENABLED=0 go build -ldflags="$LDFLAGS" -o vcluster-manager-operator ./cmd/operator && \
    CGO_ENABLED=0 go build -ldflags="$LDFLAGS" -o vcluster-manager-veleroops-operator ./cmd/veleroops-operator

# --- Image de l'opérateur VCluster ---------------------------------------------
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

# --- Image de l'opérateur VeleroOps --------------------------------------------
# Même raison de séparation que ci-dessus, un cran plus loin : ce n'est pas
# seulement "quel pod fait quoi", c'est "quel pod PEUT quoi". Il tournait dans
# le même manager que VClusterReconciler, donc sous le même ServiceAccount —
# celui qui a `delete namespaces` cluster-wide et les identifiants Vault/
# Keycloak/Rancher. Ce binaire-ci n'appelle aucun des deux (voir
# cmd/veleroops-operator/main.go) : son image et son ClusterRole
# (deploy/base/veleroops-operator-rbac.yaml) le reflètent.
FROM alpine:3.19 AS veleroops-operator
RUN apk add --no-cache ca-certificates
COPY --from=builder /app/vcluster-manager-veleroops-operator /usr/local/bin/
USER 65532:65532
ENTRYPOINT ["vcluster-manager-veleroops-operator"]

# --- Image du serveur ---------------------------------------------------------
# En dernier volontairement : c'est la cible par défaut d'un `docker build .`
# sans `--target`, donc le comportement historique ne change pas.
FROM alpine:3.19 AS server
RUN apk add --no-cache ca-certificates
COPY --from=builder /app/vcluster-manager /usr/local/bin/
COPY --from=builder /app/web /web
ENV TEMPLATE_DIR=/web/templates
# Non-root, comme les étages opérateur. C'est ce pod qui détient le token GitLab,
# le secret client Keycloak et JWT_SECRET, et c'est le seul exposé par un Ingress —
# il tournait en root avec toutes les capabilities.
USER 65532:65532
ENTRYPOINT ["vcluster-manager"]
