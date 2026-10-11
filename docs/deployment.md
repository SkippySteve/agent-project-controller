# Deployment profiles

## In-cluster HTTPS gateway translation

Some Git hosts resolve to a Kubernetes gateway service. Service translation can
change the destination to a gateway pod and its TLS listener port. Cilium's
hostname rule on port 443 alone then does not permit that backend connection.
Set `PROXY_GATEWAY_JSON` to an explicit gateway namespace, pod selector and backend
TLS port, for example:

```json
{"namespace":"gateway-system","selector":{"app.kubernetes.io/name":"gateway"},"port":8443}
```

This adds gateway access only to the project inspecting proxy. By default it also restricts TLS
server names to that project's permitted hosts. Workers continue to have only
CoreDNS queries for their proxy and TCP access to their own proxy. Upstream TLS
verification and proxy host, method and path checks still apply. Qualify service
translation and direct-worker denials against your installed Cilium dataplane.
Changing this operator configuration requires removing and recreating existing
workspaces because their enforced network policies are immutable.

## Optional coding image

The Dockerfile's default `runtime` target contains the controller, Git and the
SSH client. Build `--target workspace` for the initial Linux amd64 coding
profile, which adds Go, Python, Node, build tools and the checksum-verified Codex
CLI and app-server package. Its pinned Codex release is 0.159.0. No model
credentials are included. Authenticate through an independently provisioned
identity or an operator-controlled access-only bridge. Never copy an existing
owner's refresh credential into a project image or workspace.

The proxy Service is headless so workers connect directly to the selected
proxy pod. If your sandbox runtime bypasses Cilium's socket service translation,
set `DNS_DIRECT_ENDPOINTS=true`. The provider discovers up to three ready
CoreDNS EndpointSlices and verifies every chosen address against its referenced
CoreDNS pod. DNS policy still selects only CoreDNS pods and permits the same
exact query names. The Helm default is `false`, which uses the CoreDNS Service
IP. That default failed on the qualified gVisor cluster. Use
`docs/examples/gvisor-values.yaml` for the profile that passed.

When the only difference is the discovered nameserver list, the controller
patches the Sandbox pod template and the proxy Deployment, then deletes worker
pods whose resolvers are stale. The project volume, namespace and Forgejo
identity stay. The proxy Deployment rolls its own pods. A later reconcile
still restarts a worker that is running the old resolver after a crash between
the template patch and the pod deletion. If no ready CoreDNS endpoint is
found, reconciliation stops and the existing pods are left in place. Any other
Sandbox, proxy, storage or network change is still rejected and the workspace
is suspended.

The worker template mounts an optional `model-access` Secret at `/run/model`.
That mount is part of the pod template. Upgrading the controller while a
workspace is still Running is treated as template drift and suspends that
workspace. Save local work through Git, then remove and recreate it, or start
the new controller only when no workspace is Running or Paused. The Secret is
not copied into the image, and the controller does not copy an owner refresh
token.

If Cilium's SNI interception resets connections to a gateway on the proxy's own
node, the operator can add `"disableSNIFilter":true` to `PROXY_GATEWAY_JSON`.
This disables Cilium's extra TLS interception for that explicit gateway only.
The selected gateway pods and backend port remain restricted, workers retain
no gateway access, and the inspecting proxy still verifies upstream TLS and
enforces HTTPS host, method and path rules. A compromised proxy process has
broader gateway access under this option; qualify and record the choice rather
than claiming Cilium enforces SNI in this profile.

`PROXY_IMAGE` optionally selects a separate digest-pinned proxy runtime image.
It defaults to `WORKSPACE_IMAGE` outside Helm. The Helm chart defaults it to the
controller image so coding tools need not be included in the proxy container.
