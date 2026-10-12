# Quickstart

Install public release `0.1.0-alpha.1` on a cluster you already administer. These commands use the Linux amd64 CLI and Helm chart from that release, and the images built from commit `6d754c47f223edbe5bfff399405fb1450abb0c4b`.

The chart does not install Kubernetes, Agent Sandbox, Cilium, gVisor, Forgejo, or an identity provider. The qualified combination is recorded in [installation](installation.md). Another cluster still needs its own qualification. Workspace-image replacement, the experimental NetworkPolicy provider, and the optional GitHub project host are on current `main` and are not in this release. Leave `NETWORK_PROVIDER` at `cilium` and leave the project host on Forgejo.

## Download

```sh
curl -fsSL -O https://github.com/SkippySteve/agent-project-controller/releases/download/0.1.0-alpha.1/SHA256SUMS
curl -fsSL -O https://github.com/SkippySteve/agent-project-controller/releases/download/0.1.0-alpha.1/agent-project-controller-0.1.0-alpha.1-linux-amd64
curl -fsSL -O https://github.com/SkippySteve/agent-project-controller/releases/download/0.1.0-alpha.1/agent-project-controller-0.1.0-alpha.1.tgz
sha256sum -c SHA256SUMS --ignore-missing
install -m 0755 agent-project-controller-0.1.0-alpha.1-linux-amd64 "$HOME/.local/bin/agent-project-controller"
```

Run the downloads in one directory. `SHA256SUMS` records the other release files as paths in that directory.

## Configure

Copy [examples/alpha-values.yaml](examples/alpha-values.yaml) and replace the Forgejo origin, owner, runtime class, storage class, public origin, and OIDC client. Register the OIDC redirect URI `<PUBLIC_ORIGIN>/auth/callback`. Create the control Secret from files, not from command-line arguments:

```sh
kubectl create namespace agent-project-controller
kubectl -n agent-project-controller create secret generic controller-secrets \
  --from-file=forgejo-token=/secure/path/forgejo-token \
  --from-file=oidc-client-secret=/secure/path/oidc-client-secret
```

The Forgejo token is an operator credential. It is not a credential for a project workspace.

## Check and install

`preflight` uses your current kubeconfig. A passing result means the Agent Sandbox CRD, RuntimeClass, Delete-reclaim StorageClass, and Cilium DNS objects were found. It does not prove runtime isolation or network enforcement.

```sh
RUNTIME_CLASS=agent-gvisor STORAGE_CLASS=local-path agent-project-controller preflight
helm upgrade --install project-controller agent-project-controller-0.1.0-alpha.1.tgz \
  --namespace agent-project-controller \
  --values alpha-values.yaml
```

Use the runtime class and storage class you put in the values file. Render first with `helm template` when you want to inspect the manifest before applying it. Expose the controller Service with your own HTTPS proxy. The chart does not install a Gateway.

## Open a project

Sign in to the dashboard, choose one repository, and create a project. Its HTTPS allow list is fixed at creation. A Ready project is entered from a machine with kubeconfig:

```sh
agent-project-controller connect <workspace>
agent-project-controller exec <workspace> -- <command>
```

`connect` runs `/bin/sh` in the `workspace` container. The worker has no SSH service. Pause keeps the volume and Git identity. Removal is a separate requested state; uninstalling Helm does not revoke repository access or delete project storage. Follow [installation](installation.md) before removing the controller.
