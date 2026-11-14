# Default image for server-mode jobs of projects without .remote-claude/Dockerfile.
# Claude Code, remote-claude and gh are mounted from the host at run time; an
# image only needs glibc, git and the project's toolchain.
FROM ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive LANG=C.UTF-8
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates curl git openssh-client build-essential pkg-config \
        python3 python3-pip python3-venv nodejs npm \
        jq ripgrep unzip zip xz-utils less procps \
    && rm -rf /var/lib/apt/lists/*
ARG GO_VERSION=1.26.8
RUN curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-$(dpkg --print-architecture).tar.gz" | tar -C /usr/local -xz
ENV PATH=/usr/local/go/bin:$PATH
