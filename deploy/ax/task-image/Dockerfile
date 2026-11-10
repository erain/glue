# Copyright (c) 2026 Yu YI. Licensed under the MIT License.
# Glue AX Task image: extends digest-pinned ax-task-runner with Go 1.27.3,
# git/make/ca-certificates/bash, and the glue CLI. Keeps ENTRYPOINT untouched.

ARG BASE_IMAGE=localhost:5001/ax-task-runner@sha256:93c00918863ab142806ea1e97c3318eb8983324a9973b8419231b67d8fcaee22
FROM ${BASE_IMAGE} AS base

ARG GO_VERSION=1.27.3
ARG TARGETARCH=amd64
ARG GO_SHA256_AMD64=65efbc80fdc1fa2cd2631cad5b481777a68e8725f34e89f50fc6ac714e379b40
ARG GO_SHA256_ARM64=a06e99309dbcbdca1a8ccd2282c3ee146e55930886e26b4f1193269587165f49

RUN apt-get update && apt-get install -y --no-install-recommends bash ca-certificates git make \
    && rm -rf /var/lib/apt/lists/* \
    && case "${TARGETARCH}" in amd64) sha="${GO_SHA256_AMD64}" ;; arm64) sha="${GO_SHA256_ARM64}" ;; *) exit 1 ;; esac \
    && curl -fsSL -o /tmp/go.tar.gz "https://dl.google.com/go/go${GO_VERSION}.linux-${TARGETARCH}.tar.gz" \
    && echo "${sha}  /tmp/go.tar.gz" | sha256sum -c - \
    && rm -rf /usr/local/go && tar -C /usr/local -xzf /tmp/go.tar.gz && rm -f /tmp/go.tar.gz

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
