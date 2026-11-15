# Copyright (c) 2026 Yu YI. Licensed under the MIT License.
FROM debian:bookworm-slim AS build
ARG GO_VERSION=1.27.3 KUBECTL_VERSION=v1.35.2 TARGETOS=linux TARGETARCH=amd64
ARG GO_SHA256_AMD64=65efbc80fdc1fa2cd2631cad5b481777a68e8725f34e89f50fc6ac714e379b40 GO_SHA256_ARM64=a06e99309dbcbdca1a8ccd2282c3ee146e55930886e26b4f1193269587165f49
ARG KUBECTL_SHA256_AMD64=924eb50779153f20cb668117d141440b95df2f325a64452d78dff9469145e277 KUBECTL_SHA256_ARM64=cd859449f54ad2cb05b491c490c13bb836cdd0886ae013c0aed3dd67ff747467
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl && rm -rf /var/lib/apt/lists/* \
    && case "${TARGETARCH}" in amd64) gsha="${GO_SHA256_AMD64}"; ksha="${KUBECTL_SHA256_AMD64}" ;; arm64) gsha="${GO_SHA256_ARM64}"; ksha="${KUBECTL_SHA256_ARM64}" ;; *) exit 1 ;; esac \
    && curl -fsSL -o /tmp/go.tar.gz "https://dl.google.com/go/go${GO_VERSION}.linux-${TARGETARCH}.tar.gz" && echo "${gsha}  /tmp/go.tar.gz" | sha256sum -c - && tar -C /usr/local -xzf /tmp/go.tar.gz && rm -f /tmp/go.tar.gz \
    && curl -fsSL -o /out/kubectl --create-dirs "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl" && echo "${ksha}  /out/kubectl" | sha256sum -c - && chmod 0755 /out/kubectl
WORKDIR /src/glue
COPY . .
RUN cd gateway && PATH="/usr/local/go/bin:${PATH}" GOTOOLCHAIN=local CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /out/glue-gateway ./cmd/glue-gateway
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates git && rm -rf /var/lib/apt/lists/* && mkdir -p /var/lib/glue-gateway
COPY --from=build /out/kubectl /out/glue-gateway /usr/local/bin/
ENTRYPOINT ["/usr/local/bin/glue-gateway"]
CMD ["--config", "/etc/glue-gateway/gateway.yaml"]
