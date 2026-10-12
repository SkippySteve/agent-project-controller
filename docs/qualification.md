# Qualification

## Local checks

```sh
make test vet build
kubectl kustomize config/deploy > rendered.yaml
```

The test suite checks inspected TLS host/method/path denial, unsafe destination addresses, controller recovery, removal failure, retained volumes, ownership refusal, added network/pod privileges, browser CSRF and OIDC verification. Bootstrap tests use real Git with disposable local repositories to verify staging recovery, branch selection, remote fetch and preservation of local edits. Git is required. Tests needing Kubernetes API-server binaries, a Forgejo server or Chromium require explicit configuration.

To test CRD transition rules and resource retries against a local API server:

```sh
go install sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.0.0-20260125163108-a19ec76a3c5d
export KUBEBUILDER_ASSETS="$(setup-envtest use 1.34.1 -p path)"
go test -race -count=1 ./...
```

The controller API test uses the unmodified upstream Agent Sandbox v1.0.4 CRD, with provenance and checksum in `internal/controller/testdata`. It verifies that the schema retains isolation, DNS, resource and readiness fields, and accepts suspend/retry operations. The Cilium CRD is still a minimal stand-in. The test exercises Kubernetes API validation and defaulting but does not start upstream controllers, schedule workloads, enforce Cilium policy or erase storage. Those require the cluster qualification below.

Download and verify a Forgejo 15.0.9 binary using its official release instructions. Set its path and run:

```sh
FORGEJO_BINARY=/path/to/verified/forgejo make qualify-forgejo
```

The script starts a fresh loopback-only SQLite Forgejo instance in a private temporary directory and removes it afterward. It uses no production credentials. Real Git and PR operations run through this project's inspecting proxy with independent upstream TLS verification. Tests cover clone, ordinary branch pushes, rejection of main push/force-push/deletion, PR creation, merge denial, repository token scope, repeatable revocation and refusal to reopen a disabled identity.

OIDC tests use a local TLS discovery/JWKS/token fixture and RSA-signed identity tokens. They check valid login, PKCE, replay rejection, nonce, audience, issuer, expiry, group authorization, cookie flags and local logout. Authentik/Keycloak configurations still need real-provider qualification.

Run `govulncheck ./...` with a current scanner and the project's Go toolchain. Dependency upgrades need the same meaningful qualification, not just compilation.

To exercise the dashboard in Chromium, install Playwright in a separate tooling directory and run:

```sh
APC_BROWSER_TEST=true PLAYWRIGHT_MODULE_PATH=/path/to/node_modules/playwright go test -count=1 -v ./internal/web -run TestBrowserDashboard
```

This uses the real Go HTTP handler with a fake Kubernetes client. It checks project creation, pause, resume, removal and audit rendering, including JavaScript errors. It does not provision a running sandbox. Node and Chromium are test tools only.

The inspected HTTP/2 streaming regression uses a local TLS upstream that sends a small server-sent event and keeps the response open. The client must receive the event before upstream completion. This checks streaming delivery and cancellation without model credentials; it does not qualify authenticated inference.

## Required cluster qualification before release

Use disposable namespaces and repositories. Do not remove or replace an existing broker until these checks pass.

1. Verify the selected runtime actually provides isolation, all workloads are non-root and workers cannot access Kubernetes API credentials or inspection private keys.
2. Open a private repository from the dashboard. Confirm cloning, branch selection, safe fetch on resume and project readiness. Confirm `/etc/resolv.conf` uses the configured CoreDNS service and its traffic reaches managed CoreDNS endpoints, including in a cluster running node-local DNS. Verify direct Forgejo HTTPS, another namespace's proxy, arbitrary DNS queries, direct node-local DNS, IP-literal connections, QUIC and unapproved TLS destinations fail.
3. Prove inspected methods and paths with the actual clients, including their redirects and TLS trust. Unsupported clients must stay blocked.
4. Verify main and any additional protected references reject Git and API mutations by project identities. Confirm approved human PR review and merge still work.
5. Revoke while Git traffic is active. Confirm credentials become unusable, project processes and proxy sessions stop, the namespace disappears and the backing data is deleted by the provisioner.
6. Interrupt the controller after external user creation, grant, token issuance, namespace creation, revocation and volume deletion. Confirm restart recovery without regranting a removed project.
7. Make Forgejo and the telemetry destination unavailable independently. Local execution must stop during removal; cleanup must remain pending if revocation cannot be verified. Telemetry failure must not block enforcement.
8. Verify OIDC with the chosen provider, exact redirect URI and mapped groups. Reject unauthorized users and test group removal, session expiry, logout and controller restarts.
9. Document snapshot/backup retention, disabled-user retention, audit retention, inspection-CA rotation and recovery after loss of the control namespace.

Results must identify image digests and exact Kubernetes, Agent Sandbox, Cilium, runtime, Forgejo, DNS, storage and identity-provider versions. A CRD's presence or a successful manifest apply is not qualification.

## Alpha pilot observations

A disposable Linux amd64 pilot exercised Kubernetes 1.36.2, upstream Agent
Sandbox 1.0.4, Cilium 1.19.5, Forgejo 15.0.9 and Authentik 2026.5.6. Its gVisor
worker cloned a private repository, pushed an agent branch and opened a PR.
Forgejo rejected main pushes, forced main pushes, main deletion, main content
API writes and agent PR merges. The same scoped token could not read another
private repository independently of the proxy.

The live dashboard passed OIDC group allow/deny and project create, Ready,
pause, resume and removal. Logout ended the local session, left a signed-out
page and returned 401 for subsequent API access. A local untracked edit survived
pause/resume. Direct Forgejo, public HTTPS, Kubernetes API, metadata and
node-local DNS connections were denied, as was another project's proxy.
An active proxy connection ended during removal; the old token returned 401,
the namespace and PV disappeared, and the backing directory was absent on the
storage node.

The cluster required direct inspected CoreDNS endpoints, a headless project
proxy service, and an explicit gateway route. Cilium SNI interception reset
connections to the same-node gateway. With that extra gateway interception
explicitly disabled, the inspecting proxy still enforced HTTPS rules. A fresh
profile passed 100 reads and 20 merge denials without a 502 response. These
results qualify that specific compatibility choice, not other dataplanes.

Local tests also exercise upstream Sandbox CRD defaults with a real API server,
the Helm package, Git credential array fields, and nonblocking telemetry
recording with an unavailable collector. Controller tests cover a Forgejo grant
outage that keeps the credential ledger, a revocation outage that suspends the
workload and leaves removal pending, and a CoreDNS nameserver change that
patches the Sandbox and proxy without deleting the volume or Git identity.

On the pilot cluster, controller image `sha256:1adb1360640b9cff0b00488a2cc0f5365486ab7eb0e737b949ad563cbe8a10c1` was qualified against the existing workspace image `sha256:0a57a20dd09ceb7d7066fd070c29047fbe3921819013a8464e263fcf82c8cb66` and the existing proxy image `sha256:a5755c5db0849a117eee974b4bae4a7bee31cb3f1110de27a435d48215d21b0f`. `agent-project-controller preflight` passed there. A disposable `qualification-session` workspace cloned `steve/qualification-recovery`, mounted optional `model-access` at `/run/model`, and kept the Go registry hosts. Restarting the controller preserved the ledger and the volume. Deleting only the worker pod preserved a file on the volume and the Git identity. Replacing the Sandbox nameserver with the documentation address `192.0.2.10` while the controller was stopped, then starting it, restored the inspected CoreDNS address, restarted the worker, and kept the volume, Git identity and file. CoreDNS pods were not modified. Stopping the controller, deleting the ledger and requesting removal left the namespace in place with `credential ledger missing`. Restoring the ledger let removal finish. A second disposable workspace, `qualification-outage`, stayed in `Removing` with `Forgejo request failed` and its namespace intact while the controller's Forgejo origin was unreachable. Restoring the origin let removal finish. `scripts/qualify-recovery.py` repeats the restart, volume and ledger checks and refuses any workspace or repository that does not start with `qualification-`.

An Orca terminal on the operator machine then ran `kubectl exec` in disposable workspace `qualification-tool`. The shell started in `/workspace/repo` on its `agent/apc-<uid>` branch, with `HTTPS_PROXY` set to the project proxy. The optional `model-access` mount at `/run/model` contained no credential files, and the worker environment had no token, password, secret, or key variables. With `GOPROXY=https://proxy.golang.org,off` and `GOSUMDB=sum.golang.org`, `go get rsc.io/quote@v1.5.2` succeeded. The workspace was then removed. The repository was left in place. T3 and Roamgate were not started.

Private repository `steve/workspace-pilot` was created with default branch `main` and no branch rule. The controller installed main protection that rejects direct pushes, applies to administrators, and allows merges only by the configured owner after one approval. Workspace `workspace-pilot` reached Ready and cloned that repository. Restarting the controller preserved the credential ledger and the project volume. While the controller was stopped, the Sandbox nameserver was replaced with the documentation address `192.0.2.10`; the worker could not finish cloning. Starting the controller restored the inspected CoreDNS address, replaced that worker, and kept the volume, Git identity, and a file on the volume. The CoreDNS pod was not modified. The workspace remained Ready. The existing broker was left in place.

Controller and proxy image `sha256:6b40c5ea7df752339cdc463655c7342770a312958bdb7fdd93ddd3a2df31f0a4` then replaced both the controller process and `PROXY_IMAGE` together. The workspace image stayed `sha256:0a57a20dd09ceb7d7066fd070c29047fbe3921819013a8464e263fcf82c8cb66`. On `workspace-pilot`, that rollout restored the proxy Deployment to the new digest and left the ledger, volume, Git identity, worker pod, and `/workspace/pilot-marker` in place. Setting annotation `projects.agentwork.dev/ca-generation` to `2` minted a new inspection CA, updated ConfigMap `trust`, restarted that workspace's proxy and worker, and kept the same ledger, volume, Git identity, and marker. A Go client in the worker, with `ForceAttemptHTTP2` and the project `HTTPS_PROXY`, received HTTP 200 from `https://proxy.golang.org/rsc.io/quote/@v/v1.5.2.info` with negotiated protocol `h2`. `agent-project-controller exec workspace-pilot -- echo exec-ok` ran that command in the worker. WebSocket and certificate-pinned clients remain blocked. There is no opaque tunnel.

Disposable repository `steve/qualification-model` was created private, with default branch `main`. The controller installed the same main protection used for `workspace-pilot`. Workspace `qualification-model` included only three `chatgpt.com` GET prefixes: `/backend-api/wham/accounts/check`, `/backend-api/codex/models`, and `/backend-api/wham/settings/user`. After it reached Ready, an operator-supplied access-only record was mounted at `/run/model` as `access-token` and `chatgpt-account-id`. The record contained no refresh token. A GET to `https://chatgpt.com/backend-api/codex/models?client_version=0.159.0` through the project proxy returned HTTP 200 and a models list of 10 entries. The response body was not recorded. The Secret was deleted, the workspace was set to `Removed`, and its namespace disappeared. The private repository remains. `workspace-pilot` stayed Ready and its policy was not changed. The controller still rejects a refresh token and does not copy an owner refresh token. T3 and Roamgate were not installed. They still need their existing broker preview path because the worker has no ingress.

A Ready session is `kubectl exec`, `agent-project-controller connect`, or `agent-project-controller exec`. Orca can start that command as a local process. That is not a qualified model login. The pilot did not retire an existing broker or enable unattended work. The pilot also did not change branch protection on any repository that the controller would reject. The mutable `:pilot`, `:recovery`, and `:inspection` tags are not release pins. Ledger loss still leaves removal incomplete on purpose, and removal does not delete snapshots.

### Streaming and authenticated coding continuation

The HTTP/2 inspected response path now flushes each copied chunk. A regression test requires a small SSE event to arrive before the upstream response completes and verifies upstream cancellation. A disposable gVisor worker also passed the installed proxy streaming fixture and four method/path denial checks with external egress denied.

A fresh immutable coding worker passed an authenticated coding task with the packaged Codex 0.159.0 client. The task created `add.py` and `test_add.py`, completed three independently rerun tests, pushed its controller-issued branch and opened a pull request in a disposable repository. The built-in provider fell back from a denied WebSocket attempt to inspected HTTP POST. Streaming produced 20 text deltas. No refresh token or raw protocol events were retained. The worker used the corrected package layout and bootstrap Git author defaults, without manual package copying or author configuration.

This qualifies the coding path in the existing cluster. It does not qualify installation on a second cluster, encrypted disaster recovery, a public release, or unattended operation.

### Credential lifecycle and manual terminal qualification

A disposable Helm controller passed renewal with worker termination, expiry stopping the worker, grant removal, refusal to run after removal across a controller restart, and recovery with a valid replacement grant. Its volume survived. The updated controller rolled onto the pilot without replacing its worker, ledger, Git identity or volume. A later explicit worker-image upgrade paused the pilot, replaced its immutable Sandbox template, resumed it, and preserved its volume, identity, ledger and saved file.

The manual `model` command passed an authenticated turn in an Orca terminal. It ran the three existing fixture tests, returned their success and returned to its task prompt. The terminal client now puts the local TTY in raw mode and restores it when streaming ends. This is a text terminal connection, rather than a registered native Orca provider.

### Encrypted disposable recovery check

The recovery script now encrypts its credential-ledger record with AES-GCM before writing it to a private file. A separate private operator key is required. Tampered ciphertext, the wrong key, a symlink key and unsafe key permissions fail local tests. On the disposable Helm controller, a worker restart preserved its volume marker. Removing the ledger while reconciliation was stopped, then requesting workspace removal, left cleanup incomplete. Restoring the authenticated encrypted ledger allowed external revocation and owned namespace/storage removal to finish. The temporary encrypted record was then removed.

This is a credential-ledger recovery check with the original request UID and project volume retained. It is not a complete controller/workspace backup system or a recovery test for loss of the control namespace or volume. External backup destination and retention are deferred by the operator. Keep those recovery gates open.
