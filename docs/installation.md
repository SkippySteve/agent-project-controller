# Installation

Deploy first in a disposable qualification cluster. This initial implementation is not a qualified production replacement for an existing broker.

## Prerequisites

- Kubernetes 1.36.2 is the qualified cluster. Local API-server tests use 1.34.1. Other versions need the same qualification.
- Upstream Agent Sandbox 1.0.4 with the `agents.x-k8s.io/v1beta1` Sandbox API. Templates, claims and pools are unnecessary.
- Cilium 1.19.5 with DNS/FQDN policy enforcement enabled and CoreDNS using the `k8s-app=kube-dns` label. Project pods use explicit CoreDNS addresses, not the node's default resolver. No cluster-wide rule may broaden project egress around the inspection proxy. The qualified gVisor profile sets `DNS_DIRECT_ENDPOINTS=true`.
- The qualified isolation runtime is gVisor, via a RuntimeClass such as `agent-gvisor`. Checking that the resource exists does not prove runtime isolation. Kata is not part of this qualification.
- Forgejo 15.0.9. Repository-scoped personal access tokens and protected `main` are required. Git over HTTPS on port 443 must use a server certificate trusted by the container. The qualified identity provider is Authentik 2026.5.6.
- One Forgejo owner, one operator group and one controller replica. This is not a multi-tenant hosted service.
- A storage class and provisioner with `Delete` reclaim behavior. The controller checks both the class and bound volume. The provisioner must actually delete its backing data. Snapshots and backups need a separate operator retention policy.
- An HTTPS dashboard origin and OIDC provider. Register a confidential client with the exact redirect URI `<PUBLIC_ORIGIN>/auth/callback`. Request group claims or explicitly allow issuer-local subject IDs.

Approved operators share one trust boundary and can manage every project under that owner.

## Build and configure

Build and push the image to your registry. For release builds pass digest-pinned base references with `--build-arg GO_IMAGE=...` and `--build-arg RUNTIME_IMAGE=...`. Deploy the resulting image by its registry digest, not a mutable tag.

```sh
docker build -t registry.example.com/agent-project-controller:dev .
docker push registry.example.com/agent-project-controller:dev
```

Prepare a Kustomize overlay over `config/deploy`. Set the controller Deployment image and `WORKSPACE_IMAGE` to the built digest. Replace example hosts, Forgejo owner, runtime class, storage class and OIDC settings. Set `CLUSTER_DOMAIN` if it differs from `cluster.local`. `WORKSPACE_IMAGE` is empty in the sample, so unconfigured installations fail rather than launching an arbitrary image.

The controller discovers the address of `DNS_SERVICE_NAMESPACE` / `DNS_SERVICE_NAME`, default `kube-system` / `kube-dns`. The service must select CoreDNS pods labeled `k8s-app=kube-dns` and expose TCP and UDP port 53. Workers and proxies use `dnsPolicy: None`, that address and the configured cluster search domain. DNS policy still permits only the names required by each role. Invalid or missing resolver configuration blocks project grants.

Clusters can keep node-local DNS for other workloads. For this profile, project DNS traffic must reach CoreDNS through Cilium's DNS enforcement. Some node-local configurations intercept the kube-dns service address or use local redirect policies. In those clusters, configure a separate ClusterIP service selecting CoreDNS and qualify its routing. Do not replace the endpoint restriction with unrestricted node/host DNS access. Explicit pod DNS configuration alone does not prove that interception is absent.

Workspace resource defaults are a `500m` CPU and `512Mi` memory request with limits of `4` CPUs and `4Gi`. The clone init container uses the same profile. The HTTPS proxy has separate defaults of `100m` / `128Mi` requests and `1` CPU / `512Mi` limits. Configure these with `WORKSPACE_CPU_REQUEST`, `WORKSPACE_MEMORY_REQUEST`, `WORKSPACE_CPU_LIMIT`, `WORKSPACE_MEMORY_LIMIT` and the equivalent `PROXY_` variables. Values must be positive and requests must not exceed limits. Requests reserve scheduling capacity; limits bound use, so a memory-heavy build may still need a larger profile.

Resource profiles and resolver settings are immutable for already provisioned environments. Configuration changes apply to new projects. Existing projects report configuration drift and remain suspended rather than silently changing their security settings. Remove and recreate them after saving local work when changing these settings.

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

Before trusting a cluster, run `agent-project-controller preflight` with the same `RUNTIME_CLASS`, `STORAGE_CLASS`, DNS and `PROXY_GATEWAY_JSON` settings the controller will use. It checks the Agent Sandbox CRD, Cilium and CoreDNS, the RuntimeClass, and Delete reclaim on the storage class. A passing preflight is not the qualification in [qualification](qualification.md). Applying the CRD is not qualification either.

## Client limit

Inspection is HTTP/1.1 through an HTTP CONNECT to port 443. HTTP/2-only clients, WebSocket upgrades and certificate-pinned clients stay blocked. There is no opaque tunnel and no HTTP/2 inspection. This is the same class of limit that blocks clients which speak only HTTP/2 to their model endpoint. Allowing a registry host does not change that limit.

## Using a project

Sign in, select a repository and enter a project name. The dashboard can also select the reviewed package registries `go`, `npm`, `python` and `rust`. Each selection allows GET and HEAD for the whole host, including the redirect hosts below. It does not approve package versions, and lockfiles are not read. The resulting HTTPS rules are immutable.

| Selection | Hosts |
| --- | --- |
| go | `proxy.golang.org`, `sum.golang.org` |
| npm | `registry.npmjs.org` |
| python | `pypi.org`, `files.pythonhosted.org` |
| rust | `index.crates.io`, `static.crates.io` |

A Ready project shows a `kubectl exec` command for the `workspace` container. `agent-project-controller connect <workspace>` runs that shell with the local kubeconfig. The worker does not open an SSH port or any other ingress. Orca, T3 and Roamgate can use the workspace when they can start that command. The command requires phase `Ready`, a recorded namespace and exactly one worker pod.

Initial cloning creates a dedicated `agent/apc-<workspace-uid>` local branch. Agents can push ordinary non-main branches and open PRs using the project token. No provider credential or unattended task is enabled automatically.

The repository is cloned into `/workspace/repo`. Home, caches and configuration live under the same project volume. Pause preserves that volume and the Git identity. On resume, bootstrap fetches remote updates without resetting the current branch or overwriting local edits. Agents use normal Git commands to incorporate fetched changes and push reviewed work.

Model credentials stay out of the image. Create a Secret named `model-access` in the project namespace when a tool needs them. The worker mounts it read-only at `/run/model` and tolerates its absence. Do not copy an owner refresh token into the image or that Secret from the controller. Authenticated model calls are a separate qualification.

Add any other HTTPS destinations at project creation through the API or custom resource. The initial policy is immutable: broadening it requires a new environment. Extra host access is never inferred from package files. Package registries often redirect to additional hosts, which must be explicitly authorized. List every host before creation.

## Optional telemetry

Set `OTEL_ENABLED=true` and `OTEL_EXPORTER_OTLP_ENDPOINT` to a trusted endpoint. Standard OTLP HTTP exporter environment variables configure authentication and TLS. No Collector, OpenTelemetry Operator or particular monitoring vendor is required. The control backend exports reconciliation traces and metrics. Structured process logs go to stderr; direct OTLP log export is not implemented.

Telemetry outages do not authorize access or prevent revocation. Workspace applications are not instrumented automatically. An optional workspace exporter needs a separate authorized network path and data policy.

## Upgrades and uninstall

Keep one controller replica. OIDC sessions are invalidated on restart; credentials, requests and audit records persist in Kubernetes. Back up control-plane state through an operator-controlled encrypted backup procedure. If the credential ledger is lost, the controller deliberately leaves cleanup incomplete rather than guessing at external access.

Before uninstalling, set every workspace's desired state to `Removed` and verify its status, disabled Forgejo account and deleted backing volume. Do not remove the CRD, force finalizers, uninstall the controller or prune its control namespace while active workspaces remain. Those actions can strand grants and data. The controller retains scrubbed credential tombstones and immutable audit ConfigMaps; their retention is an operator decision.

## Single-package installation

The alpha Helm chart in `charts/agent-project-controller` installs the Go
controller, embedded dashboard, API, ProjectWorkspace CRD and Kubernetes RBAC.
The dashboard is a client of the same documented API. A separate UI deployment
is optional future work, not an installation requirement.

Provide digest-pinned `controllerImage` and `workspaceImage`, and fill the
required Forgejo, public-origin and OIDC settings in `values.yaml`. Provision
`existingSecret` independently with `forgejo-token` and `oidc-client-secret`.
If `automationAPI.enabled` is true it also needs `api-secret`, at least 32
characters. The chart neither generates nor embeds these credentials.

```sh
helm upgrade --install project-controller ./charts/agent-project-controller \
  --namespace agent-project-controller --create-namespace --values operator-values.yaml
```

Install only one release per control namespace. Existing Agent Sandbox, Cilium,
a qualified runtime and a Delete-reclaim storage class are prerequisites.
Expose the controller Service through your existing HTTPS ingress or Gateway.
The chart does not replace the cluster's network, ingress or authentication
platform. Helm manages CRDs conservatively: apply reviewed CRD schema upgrades
separately before upgrading the controller. Uninstalling the chart is not a
workspace revocation workflow. Request and verify removal of every workspace
first so external repository access and project storage are cleaned up.

The coding image is optional and separately published so adopters can choose
other tools. Publish both images by digest. See [deployment profiles](deployment.md)
and `docs/examples/gvisor-values.yaml`. The qualified gVisor profile sets
`DNS_DIRECT_ENDPOINTS` to `true` and supplies the gateway JSON. The chart
default of `false` is not that profile. This remains an alpha package. The
pilot evidence is summarized in [qualification](qualification.md). Live
deployment pins for one operator stay in that operator's private
infrastructure repository and are not required to install the chart.

Configuration changes roll the controller automatically through a pod-template checksum.
After rotating an externally managed control Secret, restart the controller so
it reloads the new credentials. Use a dedicated control namespace. The controller
pod is hardened; project namespaces additionally enforce restricted Pod Security.

Dashboard logout invalidates the local session and opens a signed-out page.
It does not sign you out of the identity provider or your other applications.
Select Sign in to start a new dashboard session.
