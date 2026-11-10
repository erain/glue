# Copyright (c) 2026 Yu YI. Licensed under the MIT License.
# Multi-stage build for glue-gateway (Go 1.27.3).
FROM debian:bookworm-slim AS build
ARG GO_VERSION=1.27.3
ARG TARGETARCH=amd64
ARG GO_SHA256_AMD64=65efbc80fdc1fa2cd2631cad5b481777a68e8725f34e89f50fc6ac714e379b40
ARG GO_SHA256_ARM64=a06e99309dbcbdca1a8ccd2282c3ee146e55930886e26b4f1193269587165f49
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl git \
    && rm -rf /var/lib/apt/lists/* \
    && case "${TARGETARCH}" in amd64) sha="${GO_SHA256_AMD64}" ;; arm64) sha="${GO_SHA256_ARM64}" ;; *) exit 1 ;; esac \
    && curl -fsSL -o /tmp/go.tar.gz "https://dl.google.com/go/go${GO_VERSION}.linux-${TARGETARCH}.tar.gz" \
    && echo "${sha}  /tmp/go.tar.gz" | sha256sum -c - \
    && tar -C /usr/local -xzf /tmp/go.tar.gz && rm -f /tmp/go.tar.gz
ENV PATH="/usr/local/go/bin:${PATH}" GOTOOLCHAIN=local
WORKDIR /src/glue
COPY go.mod go.sum ./
COPY gateway/go.mod gateway/go.sum ./gateway/
RUN cd gateway && go mod download
COPY . .
ARG TARGETOS=linux
RUN cd gateway && CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /out/glue-gateway ./cmd/glue-gateway

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /var/lib/glue-gateway
COPY --from=build /out/glue-gateway /usr/local/bin/glue-gateway
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/glue-gateway"]
CMD ["--config", "/etc/glue-gateway/config.yaml"]
