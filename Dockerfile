# Update these pinned references as part of reviewed dependency upgrades.
ARG WORKSPACE_IMAGE=debian:trixie-slim@sha256:a29215f6a35e51e22adffa17f89e9d2ef06214e64a2bad10d765c46aea49f11f
ARG GO_IMAGE=golang:1.27.2-alpine@sha256:f92b6ef800e499660581efdabdf25d9d817a9d124eaf900924f0504e7e27e12d
ARG RUNTIME_IMAGE=alpine:3.23@sha256:85fe1e81d6758c208f3e1eed4338a1997e19d4be002d4dd32d3100c9a8c010a0
FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /agent-project-controller ./cmd/agent-project-controller

FROM ${RUNTIME_IMAGE} AS runtime
RUN apk add --no-cache ca-certificates git openssh-client && addgroup -g 1000 agent && adduser -D -u 1000 -G agent agent
COPY --from=build /agent-project-controller /usr/local/bin/agent-project-controller
USER 1000:1000
ENTRYPOINT ["/usr/local/bin/agent-project-controller"]
CMD ["controller"]

# Optional coding environment. The controller and proxy can use the smaller runtime target.
FROM ${GO_IMAGE} AS toolchain
FROM ${WORKSPACE_IMAGE} AS workspace
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates git openssh-client build-essential python3 nodejs npm curl jq ripgrep bash \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd -g 1000 agent && useradd -m -u 1000 -g 1000 agent
COPY --from=build /agent-project-controller /usr/local/bin/agent-project-controller
ENTRYPOINT ["/usr/local/bin/agent-project-controller"]
COPY --from=toolchain /usr/local/go /usr/local/go
ENV PATH="/usr/local/go/bin:${PATH}" CGO_ENABLED=0
# Official musl package, verified against the release asset's SHA-256.
RUN curl -fsSL https://github.com/openai/codex/releases/download/rust-v0.159.0/codex-app-server-package-x86_64-unknown-linux-musl.tar.gz -o /tmp/codex.tar.gz \
    && echo 'de4f0638334bfe0b99ae1872983c574c5795fe709ce72a642d780befb46178ca  /tmp/codex.tar.gz' | sha256sum -c - \
    && mkdir /opt/codex && tar xzf /tmp/codex.tar.gz -C /opt/codex \
    && rm /tmp/codex.tar.gz \
    && ln -s /opt/codex/bin/codex-app-server /usr/local/bin/codex-app-server
# The CLI is a separate release asset; retain the complete app-server helpers alongside it.
RUN curl -fsSL https://github.com/openai/codex/releases/download/rust-v0.159.0/codex-x86_64-unknown-linux-musl.tar.gz -o /tmp/codex-cli.tar.gz \
    && echo '6e587a08cb39599816c598b07e17b4bcbf9d41b5c1c0793a4b632b6741e9cbc0  /tmp/codex-cli.tar.gz' | sha256sum -c - \
    && tar xzf /tmp/codex-cli.tar.gz -C /tmp \
    && mv /tmp/codex-x86_64-unknown-linux-musl /opt/codex/bin/codex \
    && ln -s /opt/codex/bin/codex /usr/local/bin/codex \
    && ln -s /opt/codex/bin/codex-code-mode-host /usr/local/bin/codex-code-mode-host \
    && rm /tmp/codex-cli.tar.gz \
    && codex --version
RUN curl -fsSL https://github.com/pingdotgg/t3code/releases/download/v0.0.45/t3-0.0.45-linux-x64.tar.gz -o /tmp/t3.tar.gz \
    && echo '10505ae74bc6a43cfab0fde0bf06a0e0d6862f74030591be994a640d48a0d6bd  /tmp/t3.tar.gz' | sha256sum -c - \
    && mkdir /opt/t3 && tar xzf /tmp/t3.tar.gz -C /opt/t3 --strip-components=1 \
    && rm /tmp/t3.tar.gz
COPY scripts/apc-codex /usr/local/bin/apc-codex
USER 1000:1000
CMD ["controller"]
