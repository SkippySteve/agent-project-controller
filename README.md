# Agent project controller

A Go controller that gives selected Forgejo repositories isolated project environments on Kubernetes Agent Sandbox. Open a project from the dashboard, work on a dedicated branch, then pause it or revoke access and remove its environment.

## Published alpha

Release [0.1.0-alpha.1](https://github.com/SkippySteve/agent-project-controller/releases/tag/0.1.0-alpha.1) is the public starting point. The [quickstart](docs/quickstart.md) downloads its Linux amd64 CLI and Helm chart, checks `SHA256SUMS`, and installs the published images. The source archive does not contain container images.

These images were built from commit `6d754c47f223edbe5bfff399405fb1450abb0c4b` and can be pulled without a token:

- `ghcr.io/skippysteve/agent-project-controller@sha256:084fdfa09f04a279d6ea11bf168222e388f9f5230ddda38a62dcade5f9b9b9d3`
- `ghcr.io/skippysteve/agent-project-workspace@sha256:caf59a3dac5b216aa670f02cc4db857cf7b214d346fc844722f7a7c602ba9892`

Deploy those digests. The tags `0.1.0-alpha.1` and `6d754c47f223edbe5bfff399405fb1450abb0c4b` point at the same images. [docs/examples/alpha-values.yaml](docs/examples/alpha-values.yaml) is a Helm values file with those pins. Replace the Forgejo origin, owner, runtime class, storage class, public origin and OIDC client, and create the `controller-secrets` Secret yourself. The chart does not install Agent Sandbox, Cilium, gVisor, Forgejo or an identity provider.

Current `main` also contains workspace-image replacement, an experimental standard NetworkPolicy provider, and an optional GitHub project host. None of those behaviors are in the `0.1.0-alpha.1` images. Rebuild from the commit you intend to run before relying on it. The NetworkPolicy provider is opt-in and is not a qualified dataplane. GitHub replaces Forgejo as the place one controller stores projects; it is not part of the qualified profile, and one controller uses one host. The qualified profile remains Cilium, gVisor, and Forgejo.

An alpha pilot has run on a live Cilium and gVisor cluster with upstream Agent Sandbox. The qualified combination is Kubernetes 1.36.2, Agent Sandbox 1.0.4, Cilium 1.19.5, gVisor, Forgejo 15.0.9 and Authentik 2026.5.6. See [installation](docs/installation.md), [security boundaries](docs/security.md), [API](docs/api.md) and [qualification](docs/qualification.md) for the evidence and the remaining limits. Deploy digest-pinned images. The moving `:pilot` tag is not the published release.

The public source is https://github.com/SkippySteve/agent-project-controller. Older Forgejo issues are not copied here. Report vulnerabilities privately, as described in [SECURITY.md](SECURITY.md).

## Relationship to Agent Sandbox

This project consumes the upstream Kubernetes Agent Sandbox API. It does not fork its controller or replace its workload lifecycle machinery.

Upstream: https://github.com/kubernetes-sigs/agent-sandbox

Agent Sandbox manages the sandbox workloads and supported lifecycle operations. This project coordinates repository authorization, credentials, cloning, project readiness, revocation and cleanup. The configured runtime, such as gVisor or Kata, provides workload isolation.

See [the combined console](docs/console.md) for preparation diagnostics, project
egress controls and integration of existing shared/provider sandboxes.

## Intended workflow

1. An authorized user selects a repository and requests a project environment through the combined project and sandbox console.
2. The controller grants repository access and provisions narrowly scoped credentials.
3. It requests a Sandbox, prepares storage and clones the project.
4. A dashboard or API client receives readiness. A Ready workspace is entered with `kubectl exec` into the `workspace` container, `agent-project-controller connect`, or `agent-project-controller exec <name> -- <command>`.
5. Removal revokes credentials and repository grants, stops the environment, removes storage according to policy and verifies cleanup.

Pause preserves project data. Removal has an explicit retention policy. A failed cleanup must remain visible and retryable; access revoked is not equivalent to data removed.

## Backend design

- Implement the control backend in Go using Kubernetes controller conventions.
- The ProjectWorkspace custom resource records repository, paused-editable HTTPS policy and desired lifecycle. This is this project's API, not an upstream Agent Sandbox resource.
- Start with core Sandbox resources. Add optional upstream templates, claims or warm pools only when needed.
- Build a documented HTTP API with a small dashboard as one client.
- Forgejo and GitHub share one Git-host interface. A controller process selects one of them. GitHub is optional and is not the qualified profile.
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

The core design permits other qualified network and Git-host providers, but those are not supported merely because an interface exists. Publish exact supported versions and end-to-end qualification results before release. T3 is included in the alpha release scope. OpenRig, native Orca registration, OTel export and remote mirrors are optional. See [T3](docs/t3.md) and [release criteria](docs/release.md).

## Portability and release

Keep cluster addresses, storage classes, authentication settings and provider configuration outside reusable code. Do not copy private deployment secrets into this repository.

The public source and the `0.1.0-alpha.1` release are on GitHub. Forgejo remains a private development remote and does not provide the public issue tracker. Copying Git history to another host does not copy issues, pull requests or host settings.

Code is licensed under Apache-2.0. The published alpha records one Cilium and gVisor qualification. Another cluster still needs its own runtime isolation, bypass prevention, volume deletion, client trust and identity-provider checks. Passing that pilot does not establish those properties elsewhere.

## OIDC login

OIDC discovery and authorization-code login with PKCE are implemented. Configure an issuer, confidential client and an explicit operator group or subject allowlist. Identity tokens are verified for signature, issuer, audience, expiry and nonce. The included deployment uses OIDC and disables static browser login.

Sessions use secure, HTTP-only cookies and expire after at most 15 minutes. A controller restart invalidates sessions. The first release uses one replica because session and login-transaction state are local to that process. Sign-out invalidates the application session; it does not log the user out of the identity provider.

Authentik and Keycloak can supply the standard protocol and mapped group claims, but their deployment-specific configurations still require qualification. The backend does not trust unsigned identity headers from a reverse proxy.

## Development

Use Go 1.27.2 or later, Git and kubectl for manifest rendering. Run `make test vet build render`. Install the published CLI from the release rather than `go install` of this module. Contribution rules are in [CONTRIBUTING.md](CONTRIBUTING.md). Optional local Kubernetes API and disposable Forgejo qualification are described in [qualification](docs/qualification.md).

The container image includes the Go controller, inspecting proxy, clone bootstrap and Git credential helper. No frontend build server or Node runtime is required. Optional OpenRig would run separately within a project environment.
