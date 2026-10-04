#!/usr/bin/env bash
set -euo pipefail
mcp_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
output_dir="${1:-$mcp_dir/dist}"
mkdir -p "$output_dir"
output_dir="$(cd "$output_dir" && pwd)"
cd "$mcp_dir"

for target in linux_amd64 linux_arm64 darwin_amd64 darwin_arm64 windows_amd64 windows_arm64; do
  target_os="${target%_*}"
  target_arch="${target#*_}"
  stage_dir="$(mktemp -d)"
  trap 'rm -rf "$stage_dir"' EXIT
  binary_name=zappy-mcp
  if [ "$target_os" = windows ]; then binary_name=zappy-mcp.exe; fi
  env GOOS="$target_os" GOARCH="$target_arch" CGO_ENABLED=0 \
    go build -trimpath -ldflags "-s -w -X github.com/firmo-tecnologia/zappy-mcp/internal/zappymcp.Version=${RELEASE_VERSION:-2.0.0}" -o "$stage_dir/$binary_name" ./cmd/zappy-mcp
  if [ "$target_os" = windows ]; then
    (cd "$stage_dir" && python3 -m zipfile -c "$output_dir/zappy-mcp_$target.zip" "$binary_name")
  else
    tar -czf "$output_dir/zappy-mcp_$target.tar.gz" -C "$stage_dir" "$binary_name"
  fi
  rm -rf "$stage_dir"
  trap - EXIT
  printf 'Built zappy-mcp_%s\n' "$target"
done

python3 - "$output_dir" <<'PY'
import hashlib,sys
from pathlib import Path
root=Path(sys.argv[1])
archives=sorted([*root.glob('*.tar.gz'),*root.glob('*.zip')])
with (root/'checksums.txt').open('w') as output:
    for archive in archives:
        output.write(hashlib.sha256(archive.read_bytes()).hexdigest()+'  '+archive.name+'\n')
PY
