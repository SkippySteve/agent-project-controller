# Security boundaries

## Trusted control plane

The controller holds a Forgejo administrative credential and cross-namespace Kubernetes permissions. A compromise of this component is more serious than compromise of one project environment. Run it separately from agents, limit administrative access to its namespace and protect Secret storage and backups. The dashboard never returns credential material.

Each workspace gets a unique non-admin Forgejo user and repository-scoped token. Only that token and username are mounted in the worker. Its administrative password stays in the control namespace. Inspection private keys are mounted in the trusted proxy, while workers receive only the public CA certificate. Agent containers have no Kubernetes token and use an operator-qualified runtime.

## Authorization

OIDC login requires a verified token and an explicit operator allowlist. Operators share one administration boundary. Email addresses and proxy-injected identity headers do not confer authority. Group changes at the identity provider take effect on the next login; existing application sessions expire within 15 minutes. Local sign-out does not terminate the provider's own session. Login transaction and application sessions live in memory, so rolling multi-replica OIDC operation is unsupported.

API mutations from browser credentials require the configured Origin and JSON content type. Automation bearer credentials are independently configured and have operator-level authority. They are not OIDC machine tokens or project credentials.

Requested-by identity is copied into immutable audit records. Kubernetes clients can supply that annotation themselves, so it is attribution from the submitted request, not cryptographic proof of the Kubernetes caller. Use Kubernetes API audit logs for direct-client attribution. The controller's own broad RBAC and cluster administrators can delete audit objects; export or back them up if independent audit retention is required.

## Main protection

When main protection is absent, the controller creates a rule that forbids direct pushes and restricts reviewed merges to the configured owner. It refuses existing insufficient protection instead of weakening or replacing it. Ambiguous wildcard rules that might overlap main require operator review. Qualification checks actual Git push, force-push and deletion denial and API merge denial.

Agents can perform operations supported by their Forgejo repository write permission. This is broader than the old broker's branch-ownership model. Protect additional release branches or tags explicitly if they matter. Main protection does not stop an agent from damaging its working branches, creating misleading PRs or disclosing source through permitted writes.

## HTTPS enforcement

Cilium restricts workers to their proxy and necessary DNS resolution. The proxy allows exact configured hosts, HTTP methods and literal path prefixes. DNS policy permits necessary exact names rather than arbitrary queries. Upstream TLS certificates are verified normally. Private upstream addresses are prohibited except for the operator-configured Forgejo host.

Project pods use an explicit CoreDNS service rather than inheriting the node-local resolver. This keeps their resolver selection consistent with endpoint-scoped DNS policy. Node-local interception and service routing still require dataplane qualification. The controller rejects unsupported resolver settings and does not grant unrestricted host DNS access.

The current implementation supports CONNECT on port 443. Negotiated HTTP/2 and HTTP/1.1 are both inspected with the same host, method and path rules. It rejects escaped paths, alternate authorities, protocol upgrades and requests larger than 128 MiB. WebSocket and certificate-pinned clients are not supported. There is no opaque tunnel fallback.

Request bodies are forwarded, not classified or filtered. Allowed API requests, Git pushes and other permitted writes can carry source code or secrets. HTTPS filtering does not provide a source-code confidentiality guarantee. Telemetry endpoints are also possible outbound data channels.

Policy creation and a Cilium CRD check do not prove enforcement. Broad cluster policies, host networking, node-local DNS and runtime differences must be checked in an actual cluster. Creating a pod before network enforcement is ready must not allow unfiltered bootstrap traffic. The release qualification requires an independent bypass test.

Each project has an inspection CA with a one-year lifetime. Leaf certificates renew in the running proxy. Set the annotation `projects.agentwork.dev/ca-generation` to the next integer to mint a new CA, update the worker trust bundle, and restart the proxy and worker. The private key stays in the control ledger and the proxy. The generation cannot skip or go backwards. Removal still requires a new project before access can be granted again.

## Removal and recovery

The controller persists the external identity before any grant. If it crashes after token creation but before saving the token value, the next attempt removes orphaned tokens before issuing another. Deleting the credential ledger manually breaks this recovery guarantee.

Removal requests suspend the Sandbox and terminate owned pods, revoke collaborator access, delete tokens and disable the Forgejo identity. An unavailable Forgejo instance leaves removal pending. Kubernetes resources and storage are retained until external revocation succeeds. The namespace is then deleted, and any matching persistent volume keeps cleanup pending until the storage provisioner removes it. Finalizers prevent normal object deletion from skipping this process.

After verified cleanup the controller removes stored token, password and private CA material, keeping a small revoked tombstone. Disabled Forgejo users preserve PR attribution and are intentionally retained. Immutable audit records have separate retention. Removing a project does not delete the original Forgejo repository or its branches and PRs.

Pause stops the worker but preserves repository credentials and files. It is not revocation. Removal is terminal; regranting access requires a new workspace and identity. Requests are asynchronous: only an observed `Removed` state confirms this controller's cleanup checks finished.

Volume and namespace deletion are infrastructure checks, not proof of physical secure erasure. Snapshots, backups, external resources and information already copied elsewhere are outside automatic cleanup. Restrict these separately and use encryption or storage-specific erasure where required.

## Reporting a vulnerability

Report a vulnerability privately to steve@aimingupward.com. Do not include credentials, tokens or a proof-of-concept exploit in a public issue. The same contact is in the repository `SECURITY.md`. GitHub private vulnerability reporting applies when this commit is mirrored there. The Git mirror does not carry Forgejo issues or pull requests.
