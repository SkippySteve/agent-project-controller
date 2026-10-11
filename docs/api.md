# API

The dashboard and HTTP API use the same project requests as direct Kubernetes clients. The API currently has one operator role, one configured repository owner and no tenant subdivision.

## Authentication

Browser users sign in at `GET /auth/login`. The OIDC callback is `GET /auth/callback`. `POST /auth/logout` invalidates the local session and requires a same-origin JSON request. Sessions are opaque cookies, not exposed OIDC tokens.

Automation can use an explicitly configured bearer secret. OIDC access tokens are not accepted as arbitrary bearer credentials. API responses do not include Forgejo or provider credentials.

## Routes

- `GET /api/v1/session` reports authentication mode and caller identity.
- `GET /api/v1/repositories` discovers repositories under the configured Forgejo owner.
- `GET /api/v1/workspaces` returns requests and observed status in the control namespace.
- `POST /api/v1/workspaces` creates a request with `name`, `repository` and optional `https` rules. Names are lowercase DNS labels up to 40 characters.
- `PATCH /api/v1/workspaces/{name}` sets `desiredState` to `Running`, `Paused` or `Removed`. Include the current `resourceVersion`; a stale request returns 409.
- `GET /api/v1/workspaces/{name}/audit` returns persisted lifecycle records.

```json
{
  "name": "example",
  "repository": "operator/example",
  "https": [{"host": "proxy.golang.org", "methods": ["GET", "HEAD"], "paths": ["/"]}]
}
```

No creation request can select arbitrary container images, runtime classes, storage classes, secret mounts or Git origins. Operators configure those on the controller. Repository and HTTPS policy are immutable after creation.

## Custom resource

`ProjectWorkspace` is namespaced at `projects.agentwork.dev/v1alpha1`. See `config/samples/project.yaml`. The controller processes only resources in its configured control namespace and derives project namespace and Forgejo identity from the resource UID.

Observed phases are `Preparing`, `Ready`, `Pausing`, `Paused`, `Blocked`, `Removing` and `Removed`. The Ready condition distinguishes desired state from observed readiness. Sandbox readiness includes the init clone and a worker readiness marker. It does not establish that an optional coding provider is authenticated or every requested external endpoint works.

Normal Kubernetes deletion triggers the cleanup finalizer. The dashboard uses the terminal `Removed` desired state so history remains visible. Removed project data cannot be reopened through a state update.

## Extending providers

`internal/forgejo.Host` defines discovery, grant and revocation. `internal/network.Provider` declares capabilities, produces enforcement objects and supplies pod DNS settings that agree with its DNS enforcement. The inspecting proxy is a separate Go component with a policy document. Provider implementations must qualify their actual behavior before being advertised as supported.

The first implementation uses an operator-selected worker image as its environment template. It does not yet expose multiple named templates or an agent-orchestrator adapter API. SSH, Orca and OpenRig integration are future additions to the prepared environment, not current tested features.
