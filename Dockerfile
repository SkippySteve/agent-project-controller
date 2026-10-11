# Update these pinned references as part of reviewed dependency upgrades.
ARG GO_IMAGE=golang:1.27.2-alpine@sha256:f92b6ef800e499660581efdabdf25d9d817a9d124eaf900924f0504e7e27e12d
ARG RUNTIME_IMAGE=alpine:3.23@sha256:85fe1e81d6758c208f3e1eed4338a1997e19d4be002d4dd32d3100c9a8c010a0
FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /agent-project-controller ./cmd/agent-project-controller

FROM ${RUNTIME_IMAGE}
RUN apk add --no-cache ca-certificates git openssh-client && addgroup -g 1000 agent && adduser -D -u 1000 -G agent agent
COPY --from=build /agent-project-controller /usr/local/bin/agent-project-controller
USER 1000:1000
ENTRYPOINT ["/usr/local/bin/agent-project-controller"]
CMD ["controller"]
