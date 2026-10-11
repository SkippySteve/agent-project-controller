# Agent project controller

A planned Go controller that gives selected Git repositories isolated project environments on Kubernetes Agent Sandbox.

This repository contains the initial project scope. There is no implemented controller or deployable release yet.

## Relationship to Agent Sandbox

This project consumes the upstream Kubernetes Agent Sandbox API. It does not fork its controller or replace its workload lifecycle machinery.

Upstream: https://github.com/kubernetes-sigs/agent-sandbox

Agent Sandbox manages the sandbox workloads and supported lifecycle operations. This project coordinates repository authorization, credentials, cloning, project readiness, revocation and cleanup. The configured runtime, such as gVisor or Kata, provides workload isolation.

## Intended workflow

1. An authorized user selects a repository and requests a project environment.
2. The controller grants repository access and provisions narrowly scoped credentials.
3. It requests a Sandbox, prepares storage and clones the project.
4. A dashboard or API client receives readiness and connection information.
5. Removal revokes credentials and repository grants, stops the environment, removes storage according to policy and verifies cleanup.

Pause preserves project data. Removal has an explicit retention policy. A failed cleanup must remain visible and retryable; access revoked is not equivalent to data removed.

## Backend design

- Implement the control backend in Go using Kubernetes controller conventions.
- Explore a ProjectWorkspace custom resource for repository, environment template, access policy and desired lifecycle. This is a proposed API, not an existing upstream resource.
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

Include project-scoped outbound policy with separate interfaces for network enforcement and HTTPS inspection. Cilium is the first proposed network provider for the existing deployment, not a requirement of the core controller.

A network provider must prevent direct outbound connections that bypass the inspecting proxy. The inspecting proxy can enforce permitted HTTPS hosts, methods and paths when clients trust its inspection certificate authority. Request-body controls are a separate capability and must not be assumed from path filtering.

Providers must report their enforcement capabilities. Reject requested policies that the selected providers cannot enforce; do not silently replace inspected access with unrestricted encrypted connections. Unsupported or certificate-pinned clients remain blocked under an inspection-required policy.

Keep inspection keys outside agent environments, install only the necessary public trust certificates, and redact credentials and sensitive payloads from telemetry. Proxy environment variables alone are not a security boundary.

The initial release should document a qualified Cilium plus inspecting-proxy combination. Additional network or proxy providers require the same bypass, revocation and failure-mode qualification.

## Optional agent integrations

OpenRig, Orca and other tools can operate within prepared project environments. They are optional and must not hold cluster or Git-host administrative credentials.

OpenRig can provide agent roles, queues, messaging and session coordination. Its Node/TypeScript runtime would be a separate optional component inside the environment; this project's backend remains Go. Integration has not been implemented or qualified.

## Portability and release

Keep cluster addresses, storage classes, authentication settings and provider configuration outside reusable code. Do not copy private deployment secrets into this repository.

Forgejo is the source repository. GitHub and GitLab can be added as push mirrors after destination repositories and credentials are configured. Git mirroring transfers repository history; issues, pull requests and host settings need separate migration or integration.

Before a public release, select a license, publish installation and upgrade documentation, and qualify authorization, revocation, restart recovery and storage cleanup end to end.

## Initial milestones

1. Specify ProjectWorkspace state transitions, credential scope and resource ownership.
2. Build the Forgejo adapter and verify branch protection and independent revocation.
3. Implement Sandbox provisioning, cloning and project readiness.
4. Implement removal, retry recovery and cleanup verification.
5. Add the API and dashboard, followed by optional agent adapters.

The existing private sandbox-control implementation can inform the design. Any extracted code must be reviewed for deployment-specific values, secrets and licensing before inclusion.
