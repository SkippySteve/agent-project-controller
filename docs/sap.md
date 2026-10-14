# Sandbox Access Protocol

SAP is the operator-facing contract for finding a sandbox and attaching to it. A Herdr, Roamgate, T3, Orca, or browser client speaks SAP once. The access plane watches sandboxes, authenticates the caller, and opens the session. The client does not pair, invent an SSH config, or choose the process that runs inside the sandbox.

The protocol is version `sandboxaccess.agents.x-k8s.io/v1alpha1`. The HTTP surface and WebSocket frames are in [api/sap/v1alpha1/openapi.yaml](../api/sap/v1alpha1/openapi.yaml). The Go client and reference plane are `pkg/sap`. A dependency-free TypeScript client is `clients/sap/index.ts`.

This is a prototype on the current controller. It is not a qualified release boundary. Upstream standardization belongs in a Kubernetes Enhancement Proposal for `kubernetes-sigs/agent-sandbox`, numbered from a tracking issue, because that project already has the Sandbox CR, `sandbox-router`, and the sandboxd runtime in KEP-539.2.

## What a client does

1. `GET /sap/v1/sandboxes` with the operator credential.
2. Read `phase` and `activity` (`idle`, `working`, `waiting`, `blocked`).
3. When `phase` is `Ready`, open `GET /sap/v1/sandboxes/{namespace}/{name}/attach` and upgrade to WebSocket subprotocol `sap.v1`.

The first server frame is `status`. The client sends `input` and `resize`. The server sends `output`, then `exit`. `data` is standard base64 of at most 32 KiB. The terminal channel starts the access plane's fixed shell. A `command` query is rejected.

`activity` here means the access plane saw terminal input in the last few seconds. It is not the coding agent's own idle, working, or waiting state. `waiting` is reserved for a later reporter. `blocked` comes from the workspace.

## How this controller wires it

The dashboard listener serves SAP beside the existing API and uses the same authentication. Browser and static-login attaches must come from the configured origin. A bearer client may omit `Origin`. The optional `/sandboxes/` console remains a mount of a pre-existing control service. SAP does not replace that service or its policies.

Discovery lists `ProjectWorkspace` resources in the control namespace. A workspace appears as soon as the object exists. Its SAP namespace is the sandbox namespace once the controller has recorded one. Attach is allowed only for a Ready workspace with exactly one worker pod. The stream is a Kubernetes exec of `/bin/sh` in the `workspace` container, the same boundary as `agent-project-controller connect`. No worker Service or ingress is added. gVisor loopback stays unexposed; the API server is the path in.

One terminal attach is held at a time. A second attach receives 409 until the first socket closes.

## Local prototype

`sap serve` is a loopback access plane with one sandbox, `local/local`, backed by a local PTY. It proves the client against the same frames. It is not an isolation boundary and it refuses a non-loopback listen address.

```sh
umask 077
openssl rand -hex 32 > "$HOME/sap.token"
agent-project-controller sap serve
```

The process prints `{"listen":"127.0.0.1:8090","mode":"local-pty",...}`. In another terminal:

```sh
export SAP_TOKEN_FILE="$HOME/sap.token"
agent-project-controller sap list
agent-project-controller sap attach local local
```

`SAP_URL` defaults to `http://127.0.0.1:8090`. `SAP_LISTEN` defaults to `127.0.0.1:8090`.

## What is intentionally absent

The control channel, file operations, worktree edits, annotations, and an MCP bridge are extension points in the proposal. This plane does not advertise them. It also does not change egress policy: workers remain limited to DNS and their inspecting proxy, and the terminal stream does not create a network path out of the sandbox.
