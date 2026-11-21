# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/dockwarden ./cmd/dockwarden

# The docker CLI image ships the compose and buildx plugins, which lets
# dockwarden rebuild compose services with `docker compose build` (BuildKit)
# and recreate them with `docker compose up -d`.
FROM docker:28-cli
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/dockwarden /usr/local/bin/dockwarden
ENV DOCKWARDEN_STATE_FILE=/var/lib/dockwarden/state.json
VOLUME ["/var/lib/dockwarden"]
LABEL org.opencontainers.image.source="https://github.com/dansimau/dockwarden" \
      org.opencontainers.image.description="Watchtower-style updater that also rebuilds compose services from local Dockerfiles"
ENTRYPOINT ["dockwarden"]
