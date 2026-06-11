#!/usr/bin/env bash
# Cross-compiles the cmd/ binaries for Linux and Windows into bin/.
#
# Builds every program under cmd/ (currently codereview and codereview-eval)
# for linux/amd64 and windows/amd64. Output layout:
#
#     bin/linux/<name>
#     bin/windows/<name>.exe
#
# Usage: ./scripts/build.sh
set -euo pipefail

# Repo root is the parent of this script's directory.
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
bin_dir="$root/bin"

# Each cmd/<name> directory becomes one binary.
cmds=()
for d in "$root"/cmd/*/; do
    cmds+=("$(basename "$d")")
done

# "GOOS GOARCH ext" per target.
targets=(
    "linux amd64 "
    "windows amd64 .exe"
)

for target in "${targets[@]}"; do
    read -r goos goarch ext <<<"$target"
    out_dir="$bin_dir/$goos"
    mkdir -p "$out_dir"

    for cmd in "${cmds[@]}"; do
        out="$out_dir/$cmd$ext"
        echo "building $cmd -> $goos/$goarch"
        GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 \
            go build -trimpath -o "$out" "$root/cmd/$cmd"
    done
done

echo "done -> $bin_dir"