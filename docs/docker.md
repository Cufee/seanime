# Rootless Docker image

This image builds the fork from source and replaces `/app/seanime` in the pinned
`umagistr/seanime:latest-rootless` runtime.

## Existing deployment

Change the image reference in your Compose service:

```yaml
image: ghcr.io/cufee/seanime:latest-rootless
```

Keep your existing volumes, network, environment, user, and command overrides.
Pull the image and recreate the service through your usual deployment process.

The runtime preserves UID/GID `1000:1000`, working directory `/app`, configuration
at `/home/seanime/.config/Seanime`, and port `43211`. FFmpeg and FFprobe are included.
Writable mounts must support the configured UID/GID; PUID/PGID variables are not
supported. Set `SEANIME_SERVER_HOST=0.0.0.0` for container network access. Changing
`SEANIME_SERVER_PORT` also requires updating the health check, which uses 43211.

Enable server transcoding and the per-device browser playback preference as
shown in [Browser playback](browser-playback.md).

## Releases

Publishing a GitHub release runs `.github/workflows/docker.yml`, checks rootless
startup, and publishes AMD64 and ARM64 images to GHCR:

- `ghcr.io/cufee/seanime:latest-rootless`
- `ghcr.io/cufee/seanime:<release-tag>-rootless`
- `ghcr.io/cufee/seanime:sha-<full-commit>-rootless`

Use fork tags such as `browser-v3.10.3.1`; plain `v*` tags trigger inherited
upstream desktop-release workflows. Pushes to `main`, bare tags, and draft
releases do not publish containers. `latest-rootless` follows published releases,
including prereleases. Use a versioned tag or digest to pin a deployment.

The workflow uses `GITHUB_TOKEN`; no Docker Hub credentials are needed. If the
GHCR package is private, pulling it requires `docker login ghcr.io` with read
access. Package visibility is configured separately from repository visibility.

## Local build

```sh
docker build -t seanime:browser-rootless .
```

The frontend and static server are built inside the container. Host build output,
local environment files, and test artifacts are excluded. BuildKit is required.

For a multiarchitecture build, use Buildx with
`--platform linux/amd64,linux/arm64` and a registry destination. Go cross-compiles
the server, so QEMU is not needed. Optional build arguments are `GO_VERSION`,
`NODE_VERSION`, `SEANIME_RUNTIME_IMAGE`, and `VCS_REF`. Keep Go aligned with
`go.mod`; update the runtime digest explicitly when adopting a new base image.
