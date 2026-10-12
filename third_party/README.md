# Packaged tools

The workspace bundles T3 Code 0.0.45 (MIT) and Codex 0.159.0 (Apache-2.0). Their upstream license/notice text is preserved in `licenses/` and included in the workspace image and release artifacts. The Dockerfile pins release assets by SHA-256. Go's license remains under `/usr/local/go/LICENSE`; Debian package copyrights remain with their installed packages. CI publishes an SPDX inventory for each image target.

Upstream references: https://github.com/pingdotgg/t3code/tree/v0.0.45 and https://github.com/openai/codex/tree/rust-v0.159.0.
