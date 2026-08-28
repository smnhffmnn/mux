# mux container image.
#
# The image knows nothing about who runs it: no config.toml, no secrets, no
# connection definitions. Everything arrives at runtime — see docs/container.md.
#
# No NET_ADMIN, no /dev/net/tun. mux carries gvisor-netstack and wireguard-go
# and does WireGuard in userspace, so the container needs no network privileges
# beyond an ordinary bridge.

# --- Build ---------------------------------------------------------------
FROM golang:1.25-alpine AS build

WORKDIR /src

# Dependencies resolve from go.mod/go.sum alone, so they cache independently
# of the source tree.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# notray drops the Wails/systray desktop build; CGO_ENABLED=0 makes the binary
# static so it runs on a distroless base with no libc.
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -tags notray \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/mux .

# An empty directory to copy in as the config dir. Distroless has no shell, so
# there is no RUN mkdir in the runtime stage — the ownership has to be set by
# the COPY that puts it there, or mux starts as nonroot against a root-owned
# directory and cannot even open its log file.
RUN mkdir -p /out/config

# --- Runtime -------------------------------------------------------------
# base-debian12 rather than static: mux resolves hostnames through the
# platform resolver, and the distroless base carries the CA bundle and
# /etc/nsswitch.conf that makes DNS behave like it does everywhere else.
FROM gcr.io/distroless/base-debian12:nonroot

COPY --from=build /out/mux /mux
COPY --from=build --chown=nonroot:nonroot /out/config /config

# The config directory is the one writable path mux needs — logs, and a
# config.toml if one is mounted or written. Declaring it as a volume keeps an
# unconfigured container from writing into the image layer.
ENV XDG_CONFIG_HOME=/config
VOLUME ["/config"]

EXPOSE 7700

# Distroless has no shell and no curl, so the binary probes itself. Exits 0
# only when every provisioned connection is actually registered — the point
# of the check is to catch the start-before-DNS case where the port answers
# but the tools are missing.
#
# start-period covers provisioning and tunnel setup on a cold start; without
# it the first few probes fail on a container that is merely still starting.
HEALTHCHECK --interval=30s --timeout=10s --start-period=60s --retries=3 \
    CMD ["/mux", "--health-check"]

USER nonroot:nonroot
ENTRYPOINT ["/mux"]
