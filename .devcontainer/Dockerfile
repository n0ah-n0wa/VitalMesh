# Reproducible toolchain for VitalMesh. Used by VS Code Dev Containers and for
# running `make verify` in a container on hosts without Go or Rust installed
# (see docs/DEVELOPMENT.md). Versions match go.mod and rust-toolchain.toml.
FROM golang:1.25.14-bookworm AS go

FROM rust:1.89.0-bookworm

COPY --from=go /usr/local/go /usr/local/go
ENV PATH="/usr/local/go/bin:/root/go/bin:${PATH}"

RUN rustup component add rustfmt clippy \
    && git config --global --add safe.directory '*'

# The Docker CLI and the compose plugin, from Docker's signed repository,
# so that the stack test suite (make stack-test, docs/DEVELOPMENT.md) can
# stop and start the compose services from inside this container when the
# host's Docker socket is mounted. No daemon is installed: the container
# talks to the host's.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl gnupg \
    && install -m 0755 -d /etc/apt/keyrings \
    && curl -fsSL https://download.docker.com/linux/debian/gpg -o /etc/apt/keyrings/docker.asc \
    && echo "deb [arch=amd64 signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/debian bookworm stable" > /etc/apt/sources.list.d/docker.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
        docker-ce-cli=5:29.8.1-1~debian.12~bookworm \
        docker-compose-plugin=5.5.1-1~debian.12~bookworm \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /workspace
