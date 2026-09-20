# syntax=docker/dockerfile:1

ARG NODE_VERSION=24.16.0
ARG GO_VERSION=1.27.1
ARG SEANIME_RUNTIME_IMAGE=docker.io/umagistr/seanime:latest-rootless@sha256:7752f1544ffaaab054804835cf64a0041970d7fbdfebfe920f81cb569ce2af8b

FROM --platform=$BUILDPLATFORM docker.io/library/node:${NODE_VERSION}-bookworm-slim AS frontend
WORKDIR /src/seanime-web
COPY seanime-web/package.json seanime-web/package-lock.json ./
COPY seanime-web/patches ./patches
RUN --mount=type=cache,target=/root/.npm npm ci --no-audit --no-fund
COPY seanime-web/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM docker.io/library/golang:${GO_VERSION}-bookworm AS server
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY main.go ./
COPY internal/ ./internal/
COPY --from=frontend /src/seanime-web/out ./web
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/seanime .

# Keep the rootless runtime's user, config/media paths, command, and healthcheck.
FROM ${SEANIME_RUNTIME_IMAGE}
ARG VCS_REF=unknown
LABEL org.opencontainers.image.source="https://github.com/Cufee/seanime" \
      org.opencontainers.image.revision="${VCS_REF}" \
      org.opencontainers.image.description="Seanime with native browser torrent playback"
COPY --from=server --chown=1000:1000 --chmod=755 /out/seanime /app/seanime
