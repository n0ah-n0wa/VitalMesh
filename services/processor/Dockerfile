# VitalMesh processing service.
#
#   docker build -t vitalmesh/processor:dev services/processor
#
# Two stages: a builder that has a Rust toolchain, and a runtime that has
# the binary and the C runtime it links against, and nothing else. No shell,
# no package manager, no build tools.
#
# Both bases are pinned by digest rather than by tag. A tag moves; a digest
# is the image it names, so a rebuild months from now uses the same bytes
# and a scan result stays meaningful.

# ---------------------------------------------------------------- builder
FROM rust:1.89.0-bookworm@sha256:948f9b08a66e7fe01b03a98ef1c7568292e07ec2e4fe90d88c07bb14563c84ff AS builder

WORKDIR /src

# Dependencies first, from the lock file alone, in a layer that is rebuilt
# only when the manifest or the lock file changes. `--locked` makes the
# build fail rather than quietly update Cargo.lock: a build that can edit
# its own lock file is not deterministic.
# Cargo refuses to parse a manifest whose declared targets are missing, and
# Cargo.toml declares the two benchmarks, so the placeholder tree has to
# include them as well as the binary. They are thrown away immediately; the
# only thing this layer keeps is the fetched dependency cache.
COPY Cargo.toml Cargo.lock rust-toolchain.toml ./
RUN mkdir -p src benches \
    && echo 'fn main() {}' > src/main.rs \
    && echo 'fn main() {}' > benches/stats.rs \
    && echo 'fn main() {}' > benches/pipeline.rs \
    && cargo fetch --locked \
    && rm -rf src benches

COPY . .

# VERSION carries a git description rather than anything secret, and it is
# the only build argument. The build needs no credential, so none is passed
# and there is none to leak into a layer.
ARG VERSION=dev
ENV VITALMESH_VERSION=${VERSION}

# --locked again, because the second invocation could otherwise resolve
# differently from the first. --offline proves the point: everything the
# build needs was fetched in the layer above, so the compile itself reaches
# the network for nothing.
RUN --mount=type=cache,target=/src/target,sharing=locked \
    cargo build --release --locked --offline \
    && cp target/release/processor /out-processor

# ---------------------------------------------------------------- runtime
# distroless/cc is the smallest published base that runs a dynamically
# linked Rust binary. There is no shell and no package manager.
#
# It is a fixed bundle, not a package list, so it is not free of things
# this binary never loads: `ldd` on the built binary names libgcc_s, libm
# and libc and nothing else, while the base also ships libstdc++, libgomp
# and libssl. They are scanned along with everything else and carry no
# known high or critical vulnerability; if one appears, the musl route
# below is the way out from under it.
#
# The alternative, a musl build on distroless/static, would drop libc from
# the image entirely. It is not used here because musl's allocator is
# markedly slower under the concurrent allocation this service does, and the
# pipeline's throughput was measured and tuned against glibc. A smaller
# image is not worth a slower engine.
FROM gcr.io/distroless/cc-debian12:nonroot@sha256:9dac0a79194e45a7da0158a9c6da57b217585af0786db3845d1f0ec1a0dd182f

# Restated rather than inherited: the base's default user is a property of
# the base, and this line is what makes the image's own behaviour explicit
# and survives a base change.
USER 65532:65532
WORKDIR /

COPY --from=builder --chown=65532:65532 /out-processor /usr/local/bin/processor

EXPOSE 8081

# The image has no shell to run a probe with, so the binary probes itself.
# It checks liveness, not readiness: readiness turns false the moment
# shutdown begins, and a draining container is not one Docker should
# restart. Kubernetes uses its own probes against /health and /ready.
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
    CMD ["/usr/local/bin/processor", "healthcheck"]

ENTRYPOINT ["/usr/local/bin/processor"]
