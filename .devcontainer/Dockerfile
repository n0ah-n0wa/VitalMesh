# Reproducible toolchain for VitalMesh. Used by VS Code Dev Containers and for
# running `make verify` in a container on hosts without Go or Rust installed
# (see docs/DEVELOPMENT.md). Versions match go.mod and rust-toolchain.toml.
FROM golang:1.25.14-bookworm AS go

FROM rust:1.89.0-bookworm

COPY --from=go /usr/local/go /usr/local/go
ENV PATH="/usr/local/go/bin:/root/go/bin:${PATH}"

RUN rustup component add rustfmt clippy \
    && git config --global --add safe.directory '*'

WORKDIR /workspace
