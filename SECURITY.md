# Security policy

Report a vulnerability privately to steve@aimingupward.com. Do not open a public issue for an unfixed vulnerability, and do not include credentials, tokens or a copy of a private repository.

The supported shape is one Forgejo owner, one operator group and one controller replica. Inspection is HTTP/1.1 CONNECT on port 443. HTTP/2-only, WebSocket and certificate-pinned clients remain blocked.

A lost credential ledger is left for the operator. Removal does not delete snapshots or backups. See `docs/security.md` for the control-plane boundaries.
