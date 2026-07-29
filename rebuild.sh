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

docker build --build-arg "VERSION=${VERSION}" -t dockontroler:latest .

# Only after the build succeeded: a broken build must not take the running
# container down with it. On a first build there is nothing here to remove yet,
# which is not an error either.
docker stop dockontroler >/dev/null 2>&1 || true
docker rm dockontroler >/dev/null 2>&1 || true

echo
echo "Built dockontroler:latest, version ${VERSION}."
echo "Now redeploy the stack in Portainer, with \"Pull latest image\" off."
