# VitalMesh API gateway.
#
#   docker build -t vitalmesh/api-gateway:dev services/api-gateway
#
# Two stages: a builder that has a Go toolchain, and a runtime that has the
# binary and nothing else. The runtime image contains no shell, no package
# manager, no libc and no build tools, so most of what a scanner or an
# attacker would look for is not there to find.
#
# Both bases are pinned by digest rather than by tag. A tag moves; a digest
# is the image it names, so a rebuild months from now uses the same bytes
# and a scan result stays meaningful.

# ---------------------------------------------------------------- builder
FROM golang:1.25.14-bookworm@sha256:3b4a11519ad929d1e1d261a12cff056f0c85b735253d7d861346b9c6f8b36437 AS builder

WORKDIR /src

# Dependencies first, in their own layer, from the lock file alone. The
# layer is rebuilt only when go.mod or go.sum change, and `-mod=readonly`
# makes the build fail rather than quietly amend them: a build that can edit
# its own lock file is not deterministic.
ENV GOFLAGS=-mod=readonly CGO_ENABLED=0 GOOS=linux
COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY . .

# VERSION is the only build argument, and it carries a git description
# rather than anything secret. Nothing here reads a credential: the build
# needs none, so none is passed, and there is no secret to leak into a
# layer.
ARG VERSION=dev
ARG TARGETARCH

# -trimpath removes the build machine's paths from the binary and
# -buildvcs=false keeps the local git state out of it, so the same source
# produces the same bytes on any machine. -s -w drop the symbol table and
# DWARF, which shrinks the binary and leaves less for a reader to work with.
RUN --mount=type=cache,target=/root/.cache/go-build \
    GOARCH=${TARGETARCH:-amd64} go build \
        -trimpath \
        -buildvcs=false \
        -ldflags="-s -w -X github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/buildinfo.Version=${VERSION}" \
        -o /out/api-gateway ./cmd/api-gateway

# ---------------------------------------------------------------- runtime
# distroless/static carries a CA bundle, timezone data and /etc/passwd, and
# nothing else: no shell, no libc, no package manager. The binary is static
# (CGO is off), so nothing more is needed. `nonroot` is uid 65532, and the
# image sets it as the default user.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

# Restated rather than inherited: the base's default user is a property of
# the base, and this line is what makes the image's own behaviour explicit
# and survives a base change.
USER 65532:65532
WORKDIR /

COPY --from=builder --chown=65532:65532 /out/api-gateway /usr/local/bin/api-gateway

EXPOSE 8080

# The image has no shell to run a probe with, so the binary probes itself.
# It checks liveness, not readiness: a container waiting for its database is
# not one Docker should restart. Kubernetes uses its own probes against
# /health and /ready instead of this.
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
    CMD ["/usr/local/bin/api-gateway", "healthcheck"]

ENTRYPOINT ["/usr/local/bin/api-gateway"]
CMD ["serve"]
