# Agent project controller

A Go controller that gives selected Forgejo repositories isolated project environments on Kubernetes Agent Sandbox. Open a project from the dashboard, work on a dedicated branch, then pause it or revoke access and remove its environment.

An alpha pilot has run on a live Cilium and gVisor cluster with upstream Agent Sandbox. The qualified combination is Kubernetes 1.36.2, Agent Sandbox 1.0.4, Cilium 1.19.5, gVisor, Forgejo 15.0.9 and Authentik 2026.5.6. See [installation](docs/installation.md), [security boundaries](docs/security.md), [API](docs/api.md) and [qualification](docs/qualification.md) for the evidence and the remaining limits. Deploy digest-pinned images. The moving `:pilot` tag is not a release.

The public source mirror is https://github.com/SkippySteve/agent-project-controller. It does not carry Forgejo issues or pull requests. Report vulnerabilities privately, as described in [SECURITY.md](SECURITY.md).

## Relationship to Agent Sandbox

This project consumes the upstream Kubernetes Agent Sandbox API. It does not fork its controller or replace its workload lifecycle machinery.

Upstream: https://github.com/kubernetes-sigs/agent-sandbox

Agent Sandbox manages the sandbox workloads and supported lifecycle operations. This project coordinates repository authorization, credentials, cloning, project readiness, revocation and cleanup. The configured runtime, such as gVisor or Kata, provides workload isolation.

## Intended workflow

1. An authorized user selects a repository and requests a project environment.
2. The controller grants repository access and provisions narrowly scoped credentials.
3. It requests a Sandbox, prepares storage and clones the project.
4. A dashboard or API client receives readiness. A Ready workspace is entered with `kubectl exec` into the `workspace` container, or with `agent-project-controller connect`.
5. Removal revokes credentials and repository grants, stops the environment, removes storage according to policy and verifies cleanup.

Pause preserves project data. Removal has an explicit retention policy. A failed cleanup must remain visible and retryable; access revoked is not equivalent to data removed.

## Backend design

- Implement the control backend in Go using Kubernetes controller conventions.
- The ProjectWorkspace custom resource records repository, immutable HTTPS policy and desired lifecycle. This is this project's API, not an upstream Agent Sandbox resource.
- Start with core Sandbox resources. Add optional upstream templates, claims or warm pools only when needed.
- Build a documented HTTP API with a small dashboard as one client.
- Integrate Forgejo first through a Git-host interface that can support other hosts later.
- Reconcile interrupted operations safely and record auditable status without logging secrets.

## Security boundaries

- Use a separate environment per project by default. Agents sharing an environment share its trust boundary.
- Keep Git-host administrative credentials in the trusted control plane, outside project environments.
- Deliver credentials whose effective access is limited to the authorized project. Credentials inside an environment are available to processes there.
- Use Git-host branch protections to protect main and require reviewed pull requests. Validate the permissions against the supported Forgejo version.
- Keep network policy, resource limits, provider identity delivery and storage retention explicit.
- Track the environment's storage and credentials so deletion can be verified. Backups and external resources need their own retention policies.
- Treat repository and agent execution as untrusted. Project workloads must not administer the controller or cluster.

## Outbound HTTPS policy

Project-scoped outbound policy uses separate interfaces for network enforcement and HTTPS inspection. Cilium is the implemented network provider and is required by the initial deployment profile. Other providers need an implementation and qualification.

A network provider must prevent direct outbound connections that bypass the inspecting proxy. The inspecting proxy can enforce permitted HTTPS hosts, methods and paths when clients trust its inspection certificate authority. Request-body controls are a separate capability and must not be assumed from path filtering.

Providers must report their enforcement capabilities. Reject requested policies that the selected providers cannot enforce; do not silently replace inspected access with unrestricted encrypted connections. Unsupported or certificate-pinned clients remain blocked under an inspection-required policy.

Keep inspection keys outside agent environments, install only the necessary public trust certificates, and redact credentials and sensitive payloads from telemetry. Proxy environment variables alone are not a security boundary.

The initial release should document a qualified Cilium plus inspecting-proxy combination. Additional network or proxy providers require the same bypass, revocation and failure-mode qualification.

## Optional agent integrations

OpenRig, Orca and other tools can operate within prepared project environments. They are optional and must not hold cluster or Git-host administrative credentials.

OpenRig can provide agent roles, queues, messaging and session coordination. Its Node/TypeScript runtime would be a separate optional component inside the environment; this project's backend remains Go. Integration has not been implemented or qualified.

## Optional observability

Instrument the Go control backend with OpenTelemetry. Optional OTLP export should cover lifecycle traces, operation latency, failures, active environments and pending revocations or cleanup. Correlate structured logs with operation IDs and trace IDs. Keep metric dimensions bounded; project IDs belong in access-controlled logs and traces rather than unbounded metric labels.

Export to an operator-selected OTLP endpoint, directly or through an OpenTelemetry Collector. No specific observability backend, Collector, Hubble or OpenTelemetry Operator is required for core operation. Disable export by default and use bounded buffering and timeouts so an unavailable telemetry service cannot block revocation or cleanup.

Persistent access audit records and lifecycle status are core features even when telemetry export is disabled. Exported telemetry is not the authoritative access ledger or proof of enforcement. Protect audit records in the trusted control plane and report failures to persist security decisions.

Do not capture tokens, authorization headers, source code, prompts, responses or inspected request bodies by default. Treat repository names and URLs as sensitive metadata. Agent and project-application instrumentation is a separate opt-in capability, not automatic collection of agent conversations. Any workspace telemetry endpoint needs explicit network authorization, authentication, tenant separation and volume limits; it is another potential outbound data channel.

## Initial adoption requirements

The initial deployment profile requires:

- A Kubernetes cluster with a qualified upstream Agent Sandbox controller and core Sandbox CRD. Optional template, claim and warm-pool extensions are unnecessary initially.
- Cilium as the initially qualified network enforcement provider, plus a separately deployed inspecting proxy when HTTPS method or path filtering is requested. Cilium alone does not establish the complete inspection workflow.
- A qualified isolation runtime such as gVisor or Kata. Ordinary containers may be useful for development but are not equivalent to the supported agent isolation profile.
- A Forgejo instance with qualified repository grants, credential revocation and branch protections, and an operator credential held outside project environments.
- Persistent volume provisioning with a documented retention and deletion policy. Snapshot and backup cleanup must be specified separately.
- Installation permissions for this controller's CRDs and RBAC, authenticated dashboard/API access, and a secure configuration path for control-plane credentials.

The core design permits other qualified network and Git-host providers, but those are not supported merely because an interface exists. Publish exact supported versions and end-to-end qualification results before release. OpenRig, Orca, T3, OTel export and remote mirrors are optional.

## Portability and release

Keep cluster addresses, storage classes, authentication settings and provider configuration outside reusable code. Do not copy private deployment secrets into this repository.

Forgejo is the source repository. GitHub and GitLab can be added as push mirrors after destination repositories and credentials are configured. Git mirroring transfers repository history; issues, pull requests and host settings need separate migration or integration.

Code is licensed under Apache-2.0. Before a public release, qualify the actual cluster's runtime isolation, Cilium bypass prevention, volume deletion, client trust and identity-provider integration end to end. Passing local qualification does not establish those properties in another deployment.

## OIDC login

OIDC discovery and authorization-code login with PKCE are implemented. Configure an issuer, confidential client and an explicit operator group or subject allowlist. Identity tokens are verified for signature, issuer, audience, expiry and nonce. The included deployment uses OIDC and disables static browser login.

Sessions use secure, HTTP-only cookies and expire after at most 15 minutes. A controller restart invalidates sessions. The first release uses one replica because session and login-transaction state are local to that process. Sign-out invalidates the application session; it does not log the user out of the identity provider.

Authentik and Keycloak can supply the standard protocol and mapped group claims, but their deployment-specific configurations still require qualification. The backend does not trust unsigned identity headers from a reverse proxy.

## Development

Use Go 1.27.2 or later, Git and kubectl for manifest rendering. Run `make test vet build render`. Optional local Kubernetes API and disposable Forgejo qualification are described in [qualification](docs/qualification.md).

The container image includes the Go controller, inspecting proxy, clone bootstrap and Git credential helper. No frontend build server or Node runtime is required. Optional OpenRig would run separately within a project environment.
