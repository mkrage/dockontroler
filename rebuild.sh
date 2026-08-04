#!/usr/bin/env bash
# Rebuild the image, then stop and remove the running container.
# (It does NOT start anything again — your deployment does that: "Update the stack"
# in Portainer, or `docker compose up -d` from the CLI.)
#
# Run this on the host that runs Docker. Portainer cannot build the image itself:
# its stack editor has no build context, which is why the compose file refers to a
# prebuilt dockontroler:latest.
set -euo pipefail
cd "$(dirname "$0")"

# Goes into the binary through -ldflags and shows up in the first log line. Without
# git, or without tags, it stays "dev" rather than failing the build.
VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"

# BuildKit knows which platform it is building on and for; the legacy builder does
# not, and leaves BUILDPLATFORM/TARGETOS/TARGETARCH empty — which the Dockerfile
# cannot recover from on its own. So without buildx the host's own platform is
# passed in, i.e. a native build, which is all the legacy builder can do anyway.
# For an arm64 image on an amd64 machine you need buildx:
#   docker buildx build --platform linux/arm64 --load -t dockontroler:latest .
if docker buildx version >/dev/null 2>&1; then
	docker buildx build --load --build-arg "VERSION=${VERSION}" -t dockontroler:latest .
else
	OS="$(docker version --format '{{.Server.Os}}')"
	ARCH="$(docker version --format '{{.Server.Arch}}')"
	docker build \
		--build-arg "BUILDPLATFORM=${OS}/${ARCH}" \
		--build-arg "TARGETOS=${OS}" \
		--build-arg "TARGETARCH=${ARCH}" \
		--build-arg "VERSION=${VERSION}" \
		-t dockontroler:latest .
fi

# Only after the build succeeded: a broken build must not take the running
# container down with it. On a first build there is nothing here to remove yet,
# which is not an error either.
docker stop dockontroler >/dev/null 2>&1 || true
docker rm dockontroler >/dev/null 2>&1 || true

echo
echo "Built dockontroler:latest, version ${VERSION}."
echo "Now redeploy the stack in Portainer, with \"Pull latest image\" off."
