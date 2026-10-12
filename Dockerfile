# Update these pinned references as part of reviewed dependency upgrades.
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
FROM runtime AS workspace
USER 0:0
RUN apk add --no-cache build-base python3 nodejs npm curl jq ripgrep bash
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
USER 1000:1000
CMD ["controller"]
