# syntax=docker/dockerfile:1

# Build stage. Runs on the build host's architecture and cross-compiles, which is
# what makes an arm64 image (a Raspberry Pi home server) buildable from an amd64
# machine without emulation.
#
# The golang:1-alpine tag floats to the current Go 1.x. Pin it to a concrete
# version once you have a build you are happy with, so the image is reproducible.
FROM --platform=$BUILDPLATFORM golang:1-alpine AS build

WORKDIR /src

# Copied first so this layer is cached independently of the source. docKontroler
# has no external dependencies, so there is nothing to download — the step is here
# to keep that true: if a dependency ever creeps in, the build will notice.
COPY go.mod ./
RUN go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

# CGO off gives a fully static binary, which is what lets the runtime image be
# distroless. -trimpath keeps build paths out of the binary, -s -w drop the debug
# tables (roughly a third of the size).
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
	go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
	-o /out/dockontroler .

# Runtime stage. distroless/static has no shell, no package manager and no libc —
# just CA certificates (needed for the Telegram API) and timezone data. The
# templates and stylesheet are embedded in the binary, so this image holds exactly
# one file.
FROM gcr.io/distroless/static-debian12

# Runs as root by default, which is what lets it open /var/run/docker.sock
# (root:docker, mode 660). See the README for running as a non-root user instead.
COPY --from=build /out/dockontroler /dockontroler

EXPOSE 3625
ENTRYPOINT ["/dockontroler"]
