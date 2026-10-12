# agent-project-controller

Helm chart for the controller, embedded dashboard, API, ProjectWorkspace CRD, and RBAC.

The chart does not install Agent Sandbox, Cilium, a runtime class, Forgejo, or an identity provider. The public install path is the [quickstart](../../docs/quickstart.md). Published image digests are in [alpha-values.yaml](../../docs/examples/alpha-values.yaml).

Supply digest-pinned `controllerImage` and `workspaceImage`, plus the Forgejo origin, owner, public origin, and OIDC client. Create Secret `controller-secrets` outside the chart, with `forgejo-token` and `oidc-client-secret`. `proxyImage` defaults to `controllerImage`.

Keep `config.NETWORK_PROVIDER` at `cilium` for the qualified profile. `network-policy` is an unvalidated experiment. The `0.1.0-alpha.1` images do not contain that provider or workspace-image replacement.
