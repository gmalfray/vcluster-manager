FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION
RUN VERSION_FILE=$(cat VERSION) && \
    CGO_ENABLED=0 go build -ldflags="-s -w -X github.com/gmalfray/vcluster-manager/internal/version.Version=${VERSION:-$VERSION_FILE}" -o vcluster-manager ./cmd/server

FROM alpine:3.19
RUN apk add --no-cache ca-certificates
COPY --from=builder /app/vcluster-manager /usr/local/bin/
COPY --from=builder /app/web /web
ENV TEMPLATE_DIR=/web/templates
ENTRYPOINT ["vcluster-manager"]
