# Alpha release criteria

The first release is `0.1.0-alpha.1`: Linux amd64 controller/operator CLI, Helm chart, dashboard, manual terminal workflow, and T3/Codex project interface. Deploy immutable image digests. It is a single-controller deployment with the qualified Cilium, inspecting proxy, gVisor, Forgejo and OIDC configuration described in [qualification](qualification.md). The experimental `NETWORK_PROVIDER=network-policy` profile is not part of this alpha claim.

A candidate is publishable only after all of these checks pass:

- Go race tests, vet, real Kubernetes API integration tests and Helm install/render tests.
- Operator credential denial, interruption and encrypted recovery tests; T3 account, path and interruption denial tests.
- Dependency vulnerability scan, reproducible binary/chart packaging, checksums and exact source/image provenance.
- Live T3 authenticated pairing, model task, repository change with independently passing tests, scoped Git push and PR.
- Anonymous/session denial, duplicate-launch rejection, reconnect persistence, model grant revocation and removal with external identity/owned-volume cleanup.
- Maintainer review of the candidate PR and approval to merge/tag/publish.

`make release VERSION=0.1.0-alpha.1` creates artifacts in an empty `dist/` directory, including source metadata, Go dependency inventory, operator scripts and SHA256SUMS. CI uploads those artifacts for review. Release image build/scanning and live qualification evidence must refer to the same reviewed source revision; an earlier successful pilot is insufficient.

`.github/workflows/publish-images.yml` publishes the `runtime` and `workspace` image targets to GHCR as `agent-project-controller` and `agent-project-workspace`. Each image is tagged with the release version and the full source revision. A published GitHub release runs it. Building an existing tag requires dispatch from the default branch, because GitHub reads this workflow from that branch. The source release archive does not contain those images. A new package stays private until its visibility is public.

Full control-namespace and lost-volume recovery remain unqualified. Backblaze S3-compatible storage transport has been exercised, but scheduled encrypted backup, retention and complete restore are not part of the current release claim. Do not describe this alpha as production-ready. Native Orca provider registration and Roamgate are optional; T3 is included in the alpha scope.
