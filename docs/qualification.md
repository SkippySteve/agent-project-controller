# Qualification

## Local checks

```sh
make test vet build
kubectl kustomize config/deploy > rendered.yaml
```

The test suite checks inspected TLS host/method/path denial, unsafe destination addresses, controller recovery, removal failure, retained volumes, ownership refusal, browser CSRF and OIDC verification. Default unit runs skip tests that need external binaries explicitly.

To test CRD transition rules and resource retries against a local API server:

```sh
go install sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.0.0-20260125163108-a19ec76a3c5d
export KUBEBUILDER_ASSETS="$(setup-envtest use 1.34.1 -p path)"
go test -race -count=1 ./...
```

The controller API test uses minimal stand-in Sandbox/Cilium CRDs. It exercises native Kubernetes object validation and defaulting but does not start upstream controllers, schedule workloads, enforce Cilium policy or erase storage. Those require the cluster qualification below.

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

## Required cluster qualification before release

Use disposable namespaces and repositories. Do not remove or replace an existing broker until these checks pass.

1. Verify the selected runtime actually provides isolation, all workloads are non-root and workers cannot access Kubernetes API credentials or inspection private keys.
2. Open a private repository from the dashboard. Confirm cloning, branch selection, safe fetch on resume and project readiness. Verify direct Forgejo HTTPS, another namespace's proxy, arbitrary DNS queries, IP-literal connections, QUIC and unapproved TLS destinations fail.
3. Prove inspected methods and paths with the actual clients, including their redirects and TLS trust. Unsupported clients must stay blocked.
4. Verify main and any additional protected references reject Git and API mutations by project identities. Confirm approved human PR review and merge still work.
5. Revoke while Git traffic is active. Confirm credentials become unusable, project processes and proxy sessions stop, the namespace disappears and the backing data is deleted by the provisioner.
6. Interrupt the controller after external user creation, grant, token issuance, namespace creation, revocation and volume deletion. Confirm restart recovery without regranting a removed project.
7. Make Forgejo and the telemetry destination unavailable independently. Local execution must stop during removal; cleanup must remain pending if revocation cannot be verified. Telemetry failure must not block enforcement.
8. Verify OIDC with the chosen provider, exact redirect URI and mapped groups. Reject unauthorized users and test group removal, session expiry, logout and controller restarts.
9. Document snapshot/backup retention, disabled-user retention, audit retention, inspection-CA rotation and recovery after loss of the control namespace.

Results must identify image digests and exact Kubernetes, Agent Sandbox, Cilium, runtime, Forgejo, DNS, storage and identity-provider versions. A CRD's presence or a successful manifest apply is not qualification.
