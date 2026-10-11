# Installation

Deploy first in a disposable qualification cluster. This initial implementation is not a qualified production replacement for an existing broker.

## Prerequisites

- Kubernetes. Local API-server tests use 1.34.1. Exact supported cluster versions depend on the qualified upstream Agent Sandbox and Cilium releases.
- Upstream Agent Sandbox with the `agents.x-k8s.io/v1beta1` Sandbox API. The adapter targets v1.0.4 operating modes. Templates, claims and pools are unnecessary.
- Cilium with DNS/FQDN policy enforcement enabled, CoreDNS using the `k8s-app=kube-dns` label, and a standard cluster DNS domain. The current adapter does not support node-local DNS. No cluster-wide rule may broaden project egress around the inspection proxy.
- A qualified gVisor or Kata RuntimeClass. Checking that the resource exists does not prove runtime isolation.
- Forgejo. The local Git/API qualification uses 15.0.9. Repository-scoped personal access tokens and protected `main` are required. Git over HTTPS on port 443 must use a server certificate trusted by the container.
- A storage class and provisioner with `Delete` reclaim behavior. The controller checks both the class and bound volume. The provisioner must actually delete its backing data. Snapshots and backups need a separate operator retention policy.
- An HTTPS dashboard origin and OIDC provider. Register a confidential client with the exact redirect URI `<PUBLIC_ORIGIN>/auth/callback`. Request group claims or explicitly allow issuer-local subject IDs.

This release manages one configured Forgejo owner and one operator trust boundary. It is not a multi-tenant hosted service. Approved operators can manage all projects under that owner.

## Build and configure

Build and push the image to your registry. For release builds pass digest-pinned base references with `--build-arg GO_IMAGE=...` and `--build-arg RUNTIME_IMAGE=...`. Deploy the resulting image by its registry digest, not a mutable tag.

```sh
docker build -t registry.example.com/agent-project-controller:dev .
docker push registry.example.com/agent-project-controller:dev
```

Prepare a Kustomize overlay over `config/deploy`. Set the controller Deployment image and `WORKSPACE_IMAGE` to the built digest. Replace example hosts, Forgejo owner, runtime class, storage class and OIDC settings. Set `CLUSTER_DOMAIN` if it differs from `cluster.local`. `WORKSPACE_IMAGE` is empty in the sample, so unconfigured installations fail rather than launching an arbitrary image.

Create the control namespace, then supply a Secret named `controller-secrets` there. Use `--from-file` with protected files so credentials do not enter shell arguments or Git:

```sh
kubectl create namespace agent-project-controller
kubectl -n agent-project-controller create secret generic controller-secrets \
  --from-file=forgejo-token=/secure/path/forgejo-token \
  --from-file=oidc-client-secret=/secure/path/oidc-client-secret
```

The Forgejo credential must be an operator-controlled administrative account capable of creating and disabling workspace users, managing repository collaborators and branch protection, and reading users and repositories. Never reuse a worker credential here. Qualification uses an `all`-scoped token on a disposable administrator. A minimally scoped production credential must be verified against the installed Forgejo version before deployment. This administrative capability is a material trust boundary.

OIDC environment variables:

- `OIDC_ISSUER`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET_FILE`.
- `OIDC_ALLOWED_GROUPS`, comma-separated exact group names, or `OIDC_ALLOWED_SUBJECTS`, comma-separated subject IDs. An empty authorization allowlist is rejected.
- `OIDC_GROUPS_CLAIM`, default `groups`. Supply a top-level array claim through your provider's mapper.
- `OIDC_SCOPES`, default `profile,groups`. Adjust scopes to the provider; `openid` is always included.
- `PUBLIC_ORIGIN`, the exact HTTPS origin without a trailing slash or path.

For automation, optionally mount an additional authentication secret and set `API_SECRET_FILE` to its path. Requests use `Authorization: Bearer <secret>`. The secret must contain at least 32 bytes. Static browser login additionally requires `ALLOW_STATIC_LOGIN=true`; it is disabled in the sample.

Expose the ClusterIP Service through your existing TLS reverse proxy. Do not expose the project namespace or proxy directly. Standard OIDC verification handles application login; forwarded identity headers are ignored. The proxy must preserve browser Origin headers, and the application origin must match exactly.

Render your overlay, inspect its RBAC and apply it to the qualification cluster. The controller has cross-namespace resource permissions to provision projects, so its credential and service account must be treated as privileged infrastructure.

## Using a project

Sign in, select a repository and enter a project name. A ready project provides a kubectl connection command. Initial cloning creates a dedicated `agent/apc-<workspace-uid>` local branch. Agents can push ordinary non-main branches and open PRs using the project token. No SSH server, coding-agent binary, provider credential or unattended task is enabled automatically.

The repository is cloned into `/workspace/repo`. Home, caches and configuration live under the same project volume. Pause preserves that volume. On resume, bootstrap fetches remote updates without resetting the current branch or overwriting local edits. Agents use normal Git commands to incorporate fetched changes and push reviewed work.

Add HTTPS destinations at project creation through the API or custom resource. The initial policy is immutable: broadening it requires a new environment. Extra host access is never inferred from package files. Package registries often redirect to additional hosts, which must be explicitly authorized.

## Optional telemetry

Set `OTEL_ENABLED=true` and `OTEL_EXPORTER_OTLP_ENDPOINT` to a trusted endpoint. Standard OTLP HTTP exporter environment variables configure authentication and TLS. No Collector, OpenTelemetry Operator or particular monitoring vendor is required. The control backend exports reconciliation traces and metrics. Structured process logs go to stderr; direct OTLP log export is not implemented.

Telemetry outages do not authorize access or prevent revocation. Workspace applications are not instrumented automatically. An optional workspace exporter needs a separate authorized network path and data policy.

## Upgrades and uninstall

Keep one controller replica. OIDC sessions are invalidated on restart; credentials, requests and audit records persist in Kubernetes. Back up control-plane state through an operator-controlled encrypted backup procedure. If the credential ledger is lost, the controller deliberately leaves cleanup incomplete rather than guessing at external access.

Before uninstalling, set every workspace's desired state to `Removed` and verify its status, disabled Forgejo account and deleted backing volume. Do not remove the CRD, force finalizers, uninstall the controller or prune its control namespace while active workspaces remain. Those actions can strand grants and data. The controller retains scrubbed credential tombstones and immutable audit ConfigMaps; their retention is an operator decision.
