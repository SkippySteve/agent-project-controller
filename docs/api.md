# API

The dashboard and HTTP API use the same project requests as direct Kubernetes clients. The API currently has one operator role, one configured repository owner and no tenant subdivision.

## Authentication

Browser users sign in at `GET /auth/login`. The OIDC callback is `GET /auth/callback`. `POST /auth/logout` invalidates the local session and requires a same-origin JSON request. Sessions are opaque cookies, not exposed OIDC tokens.

Automation can use an explicitly configured bearer secret. OIDC access tokens are not accepted as arbitrary bearer credentials. API responses do not include Forgejo or provider credentials.

## Routes

- `GET /api/v1/session` reports authentication mode and caller identity.
- `GET /api/v1/repositories` discovers repositories under the configured Forgejo owner.
- `GET /api/v1/workspaces` returns requests and observed status in the control namespace.
- `POST /api/v1/workspaces` creates a request with `name`, `repository`, optional `registries` (`go`, `npm`, `python`, `rust`) and optional `https` rules. Names are lowercase DNS labels up to 40 characters. An unknown or repeated registry, or a host listed twice, returns 400.
- `PATCH /api/v1/workspaces/{name}` sets `desiredState` to `Running`, `Paused` or `Removed`. Include the current `resourceVersion`; a stale request returns 409.
- `PATCH /api/v1/workspaces/{name}/access` replaces optional HTTPS policy with `https`, optional `registries` and current `resourceVersion`. It requires observed Paused state, an applied previous policy and no project pods. Saving leaves the project paused. Stale, running or unconverged requests return 409.
- `GET /api/v1/workspaces/{name}/audit` returns persisted lifecycle and condition records, including diagnostic `reason` and `message` when available.
- `GET /sap/v1/sandboxes` and `GET /sap/v1/sandboxes/{namespace}/{name}` discover project sandboxes for an authenticated operator. `GET /sap/v1/sandboxes/{namespace}/{name}/attach` upgrades to the Sandbox Access Protocol terminal stream. See [SAP](sap.md).

```json
{
  "name": "example",
  "repository": "operator/example",
  "registries": ["go"],
  "https": [{"host": "example.com", "methods": ["GET", "HEAD"], "paths": ["/"]}]
}
```

`registries` expands to the reviewed hosts before the custom resource is stored. `go` becomes GET and HEAD for `proxy.golang.org` and `sum.golang.org`, each with path `/`. The stored `https` list is the expanded registry rules followed by the explicit rules. Path `/` allows the whole host. It does not approve package versions.

No creation request can select arbitrary container images, runtime classes, storage classes, secret mounts or Git origins. Operators configure those on the controller. Repository is immutable after creation. HTTPS policy may change only while the workspace stays Paused. The API checks stopped processes, and the controller checks and records the applied policy before resume.

A Ready workspace is entered with `kubectl exec` into container `workspace`, `agent-project-controller connect <name>`, `agent-project-controller exec <name> -- <command>`, or a SAP attach on this same listener. SAP starts `/bin/sh` in that container. It does not accept a client-supplied command and it does not open a worker Service.

## Custom resource

`ProjectWorkspace` is namespaced at `projects.agentwork.dev/v1alpha1`. See `config/samples/project.yaml`. The controller processes only resources in its configured control namespace and derives project namespace and Forgejo identity from the resource UID.

Observed phases are `Preparing`, `Ready`, `Pausing`, `Paused`, `Blocked`, `Removing` and `Removed`. The Ready condition distinguishes desired state from observed readiness. Sandbox readiness includes the init clone and a worker readiness marker. It does not establish that an optional coding provider is authenticated or every requested external endpoint works.

Normal Kubernetes deletion triggers the cleanup finalizer. The dashboard uses the terminal `Removed` desired state so history remains visible. Removed project data cannot be reopened through a state update.

## Extending providers

`internal/forgejo.Host` defines discovery, grant and revocation. The controller selects one implementation for the process, Forgejo or GitHub, and a workspace does not choose the host. `internal/network.Provider` declares capabilities, produces enforcement objects and supplies pod DNS settings that agree with its DNS enforcement. The inspecting proxy is a separate Go component with a policy document. Provider implementations must qualify their actual behavior before being advertised as supported.

The first implementation uses an operator-selected worker image as its environment template. It does not expose multiple named templates. SSH and OpenRig are not installed. A tool that can run `kubectl exec` can use a Ready workspace. That command is not an authenticated model session. Model access is an optional Secret named `model-access` in the project namespace, mounted read-only at `/run/model`. The controller rejects a refresh token in that Secret. Model hosts must be approved in the HTTPS policy before running a model session. Add them at creation or through a paused access update.

## Combined console

See [the console workflow](console.md) for visible preparation errors, paused
egress changes and optional integration of existing sandbox controls. The
`/sandboxes/` routes use the console login and retain the backend's form CSRF
checks; they do not accept arbitrary upstream targets or proxy credentials.
