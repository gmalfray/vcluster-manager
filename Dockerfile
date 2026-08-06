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

FROM alpine:3.19
RUN apk add --no-cache ca-certificates
# Une seule image pour les deux binaires : même version, même chaîne de build, et
# le déploiement de l'opérateur n'a qu'à surcharger la commande.
COPY --from=builder /app/vcluster-manager /usr/local/bin/
COPY --from=builder /app/vcluster-manager-operator /usr/local/bin/
COPY --from=builder /app/web /web
ENV TEMPLATE_DIR=/web/templates
ENTRYPOINT ["vcluster-manager"]
