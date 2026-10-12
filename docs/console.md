# Projects and sandbox controls

The project dashboard includes lifecycle status, repository access, outbound HTTPS
rules and access history. An optional trusted backend brings pre-existing sandbox
controls into the same origin at `/sandboxes/`, with the same operator login and
navigation. This keeps existing policies, package approvals, previews and history
with the service that owns them; no worker or credential migration is required.

## Preparation errors

Each project shows the current Ready condition next to its phase. A preparation,
policy or cleanup error appears as an attention panel with the actual message.
Condition changes are also retained in access history. Repeated retries of the
same condition do not create duplicate records.

For example, an existing Forgejo `main` rule with zero required approvals produces
`require at least 1 approval (currently 0)` and directs the operator to Settings
> Branches. Fix that rule on Forgejo. The controller retries the same request;
deleting and recreating the project is unnecessary. The dashboard never rewrites
existing branch protection to make preparation pass.

## Project access

Select **Access and egress** on a project to inspect its HTTPS grants. Add initial
destinations when opening a project, or pause an existing project and wait for
**Paused** before changing them. Each destination specifies an exact lowercase
hostname, supported methods and literal path prefixes. Path `/` allows the whole
host. Registry presets expand to download hosts and do not restrict package
names or versions. Removing every optional rule denies additional HTTPS access.

Saving leaves the project paused. The API verifies that all worker and proxy
pods, including terminating pods, are gone. The controller applies network and
proxy rules, records the applied policy, and then reports Paused. Choose Resume
after that update finishes. Existing streams cannot continue under retired
permissions because the proxy also stops during pause.

Repository grants are separate, controller-managed rules. Optional HTTPS rules
cannot widen Git-host or repository API access. Repository identity, inspection
CA, namespace and project volume are preserved when updating optional access.
Interrupted updates retry from the saved policy, accepting only resources matching
the old or requested policy. Unexpected resource drift remains an error.

## Include pre-existing sandboxes

The legacy control backend contract is a server-rendered control service with
`X-authentik-username` operator authentication, same-origin POST forms, a signed
`__Host-sandbox-csrf` cookie and a matching form token. It retains its existing
authorization, policy validation and audit behavior.

Configure these controller environment values together, or set them under
`config` in the Helm values:

```yaml
SANDBOX_CONTROL_URL: http://sandbox-control.sandbox-system.svc.cluster.local:8080
SANDBOX_CONTROL_ORIGIN: https://sandboxes.example.com
SANDBOX_CONTROL_OPERATOR: operator
```

`URL` is the trusted internal control service, never a worker, preview listener,
Git broker or caller-supplied URL. `ORIGIN` is the backend's configured HTTPS
origin, which its existing POST check expects. `OPERATOR` is an explicit delegated
backend identity. Every authenticated console operator receives that identity's
sandbox-control privileges, so qualify the console's operator allowlist for both
services before enabling it. End users and agents must not be console operators.

Allow the controller to reach only the backend's control port, and permit that
source in the backend's ingress policy. The header-authenticated backend must
remain inaccessible to workers and untrusted clients. The project controller
does not add broad network access or discover backend URLs automatically.

The console strips incoming authentication/forwarded identity headers and inserts
the configured identity only after verifying its own login. It forwards only the
backend CSRF cookie, never console cookies or automation credentials. Form POSTs
must originate from the console; both backend CSRF checks remain in effect. HTML
links, form actions, polling and redirects are mounted under `/sandboxes/`.
Redirects to other origins and unrecognized control routes are rejected.

The **Existing sandboxes** section exposes the backend's registered shared and
provider sandboxes, including their access, activity, previews and settings.
Project environments continue to use their own controller and inspecting proxy.
The combined UI does not imply those environments share a network policy or a
trust boundary.

## Rollout

Apply the reviewed CRD from `config/crd` before updating the controller. Helm
does not update an installed CRD automatically. The new schema permits HTTPS
changes only when both the old and new desired state are Paused; repository
changes and reopening Removed projects remain forbidden.

Use a digest-pinned controller image while retaining the existing workspace and
proxy image pins. Configure the backend connection and network route, then
verify login, project status, a disposable paused access update and an existing
sandbox form through the console. Merely opening a project or viewing a sandbox
does not approve a new grant. Pause now stops both worker and proxy processes.

After qualification, redirect the old dashboard hostname to the console's
`/sandboxes/` route while keeping its service available internally. Keep preview
and Git endpoints on their current routes. The chart does not retire existing
ingress routes or deploy the backend for you. A backend outage affects the
existing-sandbox section; project lifecycle controls continue to work.

Rollback to an older controller requires preserving compatible access policies
and the applied ledger. Do not restore the older immutable-policy CRD over
changed project requests or delete storage to recover the old UI.
