#!/usr/bin/env bash
set -euo pipefail
version=${VERSION:?Set VERSION to a release version}
if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-alpha\.[0-9]+)?$ ]]; then
  echo 'Unsupported release version' >&2
  exit 1
fi
revision=$(git rev-parse HEAD)
if [[ -d dist ]] && [[ -n "$(find dist -mindepth 1 -maxdepth 1 -print -quit)" ]]; then
  echo "dist must be empty for a reproducible release package" >&2
  exit 1
fi
mkdir -p dist
for platform in linux/amd64; do
  target_os=${platform%/*}
  target_arch=${platform#*/}
  suffix=''
  if [[ "$target_os" == windows ]]; then suffix=.exe; fi
  output="dist/agent-project-controller-${version}-${target_os}-${target_arch}${suffix}"
  CGO_ENABLED=0 GOOS="$target_os" GOARCH="$target_arch" go build -trimpath \
    -ldflags="-s -w -X main.version=$version -X main.commit=$revision" -o "$output" ./cmd/agent-project-controller
  go version -m "$output" > "$output.buildinfo.txt"
done
helm package charts/agent-project-controller --version "$version" --app-version "$version" --destination dist
# Dependency inventories accompany the binaries.
go list -m -json all > "dist/agent-project-controller-${version}-go-modules.json"
cp LICENSE dist/
cp third_party/licenses/*.txt dist/
cp scripts/model-access.py scripts/qualify-recovery.py scripts/requirements-recovery.txt dist/
cat > "dist/agent-project-controller-${version}-source.json" <<META
{"version":"$version","source_commit":"$revision","workspace_platform":"linux/amd64","t3_version":"0.0.45","codex_version":"0.159.0","release_stage":"alpha"}
META
(cd dist && find . -maxdepth 1 -type f ! -name SHA256SUMS -print0 | sort -z | xargs -0 sha256sum > SHA256SUMS)
