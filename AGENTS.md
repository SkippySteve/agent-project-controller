# Contribution workflow

Use a task branch. Commit changes, push to Forgejo and open a pull request after relevant checks. Do not merge without the maintainer's approval.

Keep the control backend in Go. Build on upstream Agent Sandbox APIs without forking its controller. Agent orchestrators and telemetry exporters are optional.

Do not include deployment-specific addresses, credentials or source from private infrastructure in reusable code. Do not grant project access or deploy to a live cluster as a side effect of unit tests. Qualification must use disposable environments and repositories.

Security changes need tests for denial and interrupted-operation recovery. Unsupported policies must fail closed. Removal must never report completion before external revocation and owned storage cleanup are verified.
