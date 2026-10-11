# Operator model grants

The operator supplies access-only model grants. The controller does not refresh credentials. Keep owner authentication and its refresh token outside every project worker.

Use `python3 scripts/model-access.py --workspace NAME --record-file PRIVATE.json`. The private file must belong to the operator and deny group and other access. Its JSON has exactly `enabled: true` and an `access` object containing `accessToken`, `chatgptAccountId`, `expiresAt`, and optionally `chatgptPlanType`. Expiry is a Unix timestamp. The tool also accepts `--record-stdin` for an owner-side projection delivered directly through a pipe. Do not put credentials in command arguments, shell history or Git.

The tool pauses the workspace and verifies termination before publishing or renewing the Secret. It restores a previously Running workspace after a successful update. A failure after pausing leaves it paused. Renewal must retain the same account. Supply `--context` and `--control-namespace` when the defaults do not select the intended controller.

Use `--revoke` instead of a record option to remove the grant and leave the workspace paused. The controller rejects unknown fields, nested authentication records, refresh tokens, invalid token/account fields and expiry within 60 seconds. Once it has accepted a grant, its durable ledger requires a valid replacement before the worker can run again. Expiry and removal stop the worker on reconciliation. This is bounded by reconciliation and Kubernetes termination, rather than an instantaneous provider-side revocation. Revoke the token at its owner/provider as well when required.

The expiry and durable revocation changes require the updated controller image. They must pass live qualification before being described as deployed behavior.
