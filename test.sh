#!/usr/bin/env bash
# Check the working tree: formatting, vet, and the full test suite.
#
# Nothing needs to be installed — the toolchain runs in a throwaway container, which
# is the same way the README describes doing it by hand. Run it anywhere the source
# is checked out, including the home server that runs the image.
#
# It needs no Docker daemon of its own and no network: internal/manager runs against
# an in-process simulator of the Engine API, so even the recreate logic and every
# rollback path is exercised without a container being created anywhere.
set -euo pipefail
cd "$(dirname "$0")"

# golang:1 rather than golang:1-alpine, which is what the image itself is built with:
# -race needs cgo, and Alpine ships no C compiler, so the detector fails there with
# "-race requires cgo".
#
# RACE=0 gives up the detector for the small image — a couple of hundred megabytes
# instead of a gigabyte, and a noticeably faster run on a Raspberry Pi. Worth it for
# a quick check; not worth it as the default, because the races this catches are the
# ones the refresh loop and the concurrent actions could plausibly have.
IMAGE=golang:1
RACE_FLAG=-race
if [[ "${RACE:-1}" == 0 ]]; then
	IMAGE=golang:1-alpine
	RACE_FLAG=
fi

# The compiler cache lives in a named volume rather than in the container, or every
# run rebuilds the standard library from scratch. Nothing else is written outside
# /src: the module has no dependencies to download.
docker run --rm \
	-v "$PWD":/src -w /src \
	-v dockontroler-gocache:/root/.cache/go-build \
	-e "RACE_FLAG=${RACE_FLAG}" \
	"$IMAGE" sh -c '
	set -e

	# gofmt -l, not -w: this script reports, it does not edit. A gate that quietly
	# rewrites your working tree is one that hides what it found — and it would do so
	# on a machine you are only deploying from. Use the README'"'"'s gofmt -w while
	# you are working on the code.
	unformatted="$(gofmt -l .)"
	if [ -n "$unformatted" ]; then
		echo "not gofmt-clean:" >&2
		echo "$unformatted" >&2
		exit 1
	fi

	go vet ./...
	go test $RACE_FLAG ./...
'

echo
echo "Formatting, vet and tests are clean."
