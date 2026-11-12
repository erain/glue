# Copyright (c) 2026 Yu YI. Licensed under the MIT License.
# Glue AX Task image: extends digest-pinned ax-task-runner with Go 1.27.3,
# Node.js LTS, Python tooling (pip, venv, uv), a C toolchain, git, make,
# ripgrep, jq, and the glue CLI. Keeps ENTRYPOINT untouched.

ARG BASE_IMAGE=localhost:5001/ax-task-runner@sha256:93c00918863ab142806ea1e97c3318eb8983324a9973b8419231b67d8fcaee22
FROM ${BASE_IMAGE} AS base

ARG GO_VERSION=1.27.3
ARG TARGETARCH=amd64
ARG GO_SHA256_AMD64=65efbc80fdc1fa2cd2631cad5b481777a68e8725f34e89f50fc6ac714e379b40
ARG GO_SHA256_ARM64=a06e99309dbcbdca1a8ccd2282c3ee146e55930886e26b4f1193269587165f49
ARG NODE_VERSION=26.13.0
ARG NODE_SHA256_AMD64=d28a96af6c38596f6d9d97c0ee531c13d238d757742387d1928de822d959e997
ARG NODE_SHA256_ARM64=406fe3fbde1c054adff8b297c16fdb5af6659db327c1ac8acd46b56ef1af9a92
ARG UV_VERSION=0.13.6

RUN apt-get update && apt-get install -y --no-install-recommends bash ca-certificates git make \
        gcc g++ libc6-dev jq ripgrep xz-utils \
    && rm -rf /var/lib/apt/lists/* \
    && case "${TARGETARCH}" in amd64) sha="${GO_SHA256_AMD64}" ;; arm64) sha="${GO_SHA256_ARM64}" ;; *) exit 1 ;; esac \
    && curl -fsSL -o /tmp/go.tar.gz "https://dl.google.com/go/go${GO_VERSION}.linux-${TARGETARCH}.tar.gz" \
    && echo "${sha}  /tmp/go.tar.gz" | sha256sum -c - \
    && rm -rf /usr/local/go && tar -C /usr/local -xzf /tmp/go.tar.gz && rm -f /tmp/go.tar.gz \
    && case "${TARGETARCH}" in amd64) narch=x64; nsha="${NODE_SHA256_AMD64}" ;; arm64) narch=arm64; nsha="${NODE_SHA256_ARM64}" ;; *) exit 1 ;; esac \
    && curl -fsSL -o /tmp/node.tar.xz "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-${narch}.tar.xz" \
    && echo "${nsha}  /tmp/node.tar.xz" | sha256sum -c - \
    && tar -C /usr/local --strip-components=1 -xJf /tmp/node.tar.xz && rm -f /tmp/node.tar.xz \
    && python3 -m pip install --no-cache-dir "uv==${UV_VERSION}"

ENV GOPATH=/go GOMODCACHE=/go/pkg/mod GOTOOLCHAIN=local PATH="/usr/local/go/bin:/go/bin:${PATH}"

FROM base AS build
WORKDIR /src/glue
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /out/glue ./cmd/glue

FROM base
ARG PREWARM_GOMOD_REPO=""
ARG PREWARM_GOMOD_REF="HEAD"
RUN if [ -n "${PREWARM_GOMOD_REPO}" ]; then \
      git clone --depth 1 "${PREWARM_GOMOD_REPO}" /tmp/prewarm && \
      if [ "${PREWARM_GOMOD_REF}" != "HEAD" ]; then git -C /tmp/prewarm fetch --depth 1 origin "${PREWARM_GOMOD_REF}" && git -C /tmp/prewarm checkout FETCH_HEAD; fi && \
      (cd /tmp/prewarm && go mod download) && rm -rf /tmp/prewarm; \
    fi
COPY --from=build /out/glue /usr/local/bin/glue
RUN printf '#!/bin/sh\nexec /usr/local/bin/glue git-askpass "$@"\n' >/usr/local/bin/glue-git-askpass \
    && chmod 0755 /usr/local/bin/glue-git-askpass \
    && git config --system core.askPass /usr/local/bin/glue-git-askpass
