# Contributing

Public bug reports, feature requests, and code reviews happen on GitHub. Open a pull request against `main`. Report a vulnerability by email, as described in [SECURITY.md](SECURITY.md). Do not open a public issue for an unfixed vulnerability.

Do not include tokens, pairing URLs, private hostnames, credentials, or contents of a private repository.

## Install and build

Operators install the published Linux amd64 CLI and Helm chart from the [0.1.0-alpha.1 release](https://github.com/SkippySteve/agent-project-controller/releases/tag/0.1.0-alpha.1). The [quickstart](docs/quickstart.md) is that path. `go install` of this module is not a public install method. Clone this repository and build inside the clone:

```sh
go test -race -count=1 ./...
go vet ./...
```

Use Go 1.27.2 or later. Helm chart changes also run `APC_HELM_TEST=true go test -count=1 ./internal/installation`. API-server tests need the envtest binaries named in [qualification](docs/qualification.md).

## Changes

Use a task branch. Do not commit on `main`. Keep deployment-specific addresses and manifests out of this repository.

The qualified profile is Cilium and gVisor. `NETWORK_PROVIDER=network-policy` is experimental. Do not describe it as a supported dataplane without a run on that dataplane. One controller replica, one Forgejo owner, and one operator group are the supported shape. This is not a hosted multi-tenant service.

Code is Apache-2.0. See [LICENSE](LICENSE).
