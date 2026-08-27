#!/usr/bin/env sh
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
dist_dir="$repo_dir/dist"
version="${TASKIAN_VERSION:-dev}"
mkdir -p "$dist_dir"

cd "$repo_dir"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags "-s -w -X main.version=$version" \
  -o "$dist_dir/taskian-linux-amd64" ./cmd/taskian

CGO_ENABLED=0 GOOS=windows GOARCH=amd64 \
  go build -trimpath -ldflags "-s -w -X main.version=$version" \
  -o "$dist_dir/taskian-windows-amd64.exe" ./cmd/taskian

CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 \
  go build -trimpath -ldflags "-s -w -X main.version=$version" \
  -o "$dist_dir/taskian-darwin-amd64" ./cmd/taskian

CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 \
  go build -trimpath -ldflags "-s -w -X main.version=$version" \
  -o "$dist_dir/taskian-darwin-arm64" ./cmd/taskian

ls -lh "$dist_dir"
