# Rootless Docker image

This fork builds its web interface and server from source, then replaces
`/app/seanime` in a pinned `umagistr/seanime:latest-rootless` runtime. The base
image digest is a multiarchitecture manifest, so the same Dockerfile works on
Intel/AMD hosts and Apple Silicon Macs running Docker/OrbStack.

## Use the published package

Publishing a GitHub release runs `.github/workflows/docker.yml`. It builds and
checks an AMD64 image, then publishes AMD64 and ARM64 images to GitHub Container
Registry under these tags:

- `ghcr.io/cufee/seanime:latest-rootless`
- `ghcr.io/cufee/seanime:<release-tag>-rootless`
- `ghcr.io/cufee/seanime:sha-<full-commit>-rootless`

`latest-rootless` follows the most recently built published release, including
prereleases. Pushes to `main`, bare tags, and draft releases do not publish an
image. The workflow uses `GITHUB_TOKEN` with package write permission; no Docker
Hub secret is needed. GHCR package visibility is separate from repository
visibility; a private package requires `docker login ghcr.io` with read access.

In an existing Compose service, change only its image reference:

```yaml
image: ghcr.io/cufee/seanime:latest-rootless
```

Keep the existing volumes, network configuration, environment, user override,
and command override. Once the release workflow succeeds, pull and recreate
the service using your usual Compose deployment process.

For a new standalone deployment:

```yaml
services:
  seanime:
    image: ghcr.io/cufee/seanime:latest-rootless
    user: "1000:1000"
    environment:
      SEANIME_SERVER_HOST: 0.0.0.0
      SEANIME_SERVER_PORT: "43211"
    ports:
      - "3211:43211"
    volumes:
      - ./seanime-config:/home/seanime/.config/Seanime
      - ./anime:/anime
      - ./downloads:/downloads
    restart: unless-stopped
```

Prepare writable host directories for the configured UID/GID. There is no
PUID/PGID entrypoint or automatic ownership repair. Configure Seanime's password
and access settings as usual. Browser torrent playback additionally requires
server media transcoding and the per-device **Play torrents in this browser**
preference. FFmpeg and FFprobe are available at `/usr/bin/ffmpeg` and
`/usr/bin/ffprobe`; use software transcoding for the tested Docker configuration.

## Compatibility with the existing rootless image

The inherited runtime retains:

| Setting | Value |
| --- | --- |
| User/group | `seanime`, UID/GID `1000:1000` |
| Configuration | `/home/seanime/.config/Seanime` |
| Working directory | `/app` |
| Default command | `/app/seanime` |
| Entrypoint | None; existing Compose `command:` overrides still work |
| Exposed HTTP port | `43211/tcp` |
| Health check | `curl -f http://localhost:43211 \|\| exit 1` |
| Runtime tools | FFmpeg, FFprobe, curl, CA certificates, timezone data |

The image does not set `SEANIME_SERVER_HOST`; preserve the existing environment
or set it to `0.0.0.0` for access through container networking. The inherited
health check uses port 43211; overriding the internal port also requires an
appropriate Compose health check. `/anime` and `/downloads` are conventional
mount paths and can retain the existing deployment's layout.

The base was checked against the [image's pinned source Dockerfile](https://github.com/umag/seanime-docker/blob/9bbf6c3bb2172da3269f3575eccdaa264f95c635/Dockerfile)
and its live registry configuration. The static Go build (`CGO_ENABLED=0`) runs
on the base's Alpine runtime even though the builders use Debian. No application
entrypoint or production configuration is rewritten by this Dockerfile.

## Build locally

From the repository root, with Docker BuildKit enabled:

```sh
docker build -t seanime:browser-rootless .
```

The frontend is built inside the container, including type checking, patched
dependencies, and the subtitle worker/WASM assets. Local `web/`, `node_modules/`,
test artifacts, and environment files are excluded from the build context.
Nothing needs to be built on the host first.

For both published architectures, use Buildx with a registry destination:

```sh
docker buildx build --platform linux/amd64,linux/arm64 \
  -t ghcr.io/cufee/seanime:manual-rootless --push .
```

The builder stages run on the build host's architecture and Go cross-compiles
the server; the runtime stage only copies the binary, so QEMU is not required.
`GO_VERSION`, `NODE_VERSION`, `SEANIME_RUNTIME_IMAGE`, and `VCS_REF` are optional
build arguments. Keep the Go version aligned with `go.mod`. To update the
runtime, review the new image's configuration and replace its pinned manifest
digest; a floating `latest-rootless` tag does not change the current pin.

## Release a new image

Merge changes to this fork's `main`, create a unique fork release tag such as
`browser-v3.10.3.1`, and publish its GitHub release. The `browser-` prefix avoids
the inherited upstream desktop-release workflows that trigger on `v*` tags.
The published release event
builds that tag's commit, not a later `main` revision. The workflow must pass its
rootless startup and media-tool checks before publishing the multiarchitecture
package. Use a versioned image tag or digest when pinning a deployment.

## Validation

On 2026-09-20, both AMD64 and ARM64 images built from source with Podman 5.7.
The ARM64 image's executable was checked as an AArch64 ELF binary. The AMD64
image passed UID/GID and home-path checks, HTTP startup, subtitle worker and
WASM serving, its inherited health check, H.264 High/AAC software conversion,
and configuration persistence across container recreation. All test containers
and their temporary volumes were removed. `actionlint` and `hadolint` passed.
The Mac deployment has not been changed; ARM64 runtime playback still requires
validation there.
