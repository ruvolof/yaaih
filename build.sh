#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
IMAGE="aibitat-harness-runtime:latest"
export GOPATH="$root/.go"
export GOMODCACHE="$GOPATH/pkg/mod"
export GOCACHE="$GOPATH/cache"
export GOBIN="$GOPATH/bin"

mkdir -p "$GOMODCACHE" "$GOCACHE" "$GOBIN"

resolve_docker_host() {
  if [ -n "${DOCKER_HOST:-}" ]; then
    return
  fi
  if ! command -v docker >/dev/null 2>&1; then
    return
  fi

  ctx=$(docker context show 2>/dev/null || true)
  if [ -z "$ctx" ]; then
    return
  fi

  host=$(docker context inspect --format '{{json .Endpoints.docker.Host}}' "$ctx" 2>/dev/null || true)
  if [ -z "$host" ] || [ "$host" = "null" ]; then
    return
  fi
  host="${host#\"}"
  host="${host%\"}"
  export DOCKER_HOST="$host"
}

target="${1:-build}"

usage() {
  cat <<EOF
Usage: $0 {tidy|vendor|build|image|run|clean}

Targets:
  tidy    - download dependencies into ./.go/pkg/mod
  vendor  - tidy + go mod vendor
  build   - build binary using vendored deps
  image   - build the runtime Docker image from the repo root Dockerfile
  run     - build binary, build runtime image if missing, and run the harness with ./agents/chatter
  clean   - remove vendor/, .go/, and the binary
EOF
}

case "$target" in
  tidy)
    go mod tidy
    ;;
  vendor)
    go mod tidy
    go mod vendor
    ;;
  build)
    if [ ! -d "$root/vendor" ]; then
      go mod tidy
      go mod vendor
    fi
    go build -mod=vendor -o "$root/aibitat-harness-go" .
    ;;
  image)
    docker build -t "$IMAGE" -f "$root/Dockerfile" "$root"
    ;;
  run)
    if [ ! -d "$root/vendor" ]; then
      go mod tidy
      go mod vendor
    fi
    if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
      echo "Building runtime image $IMAGE from $root/Dockerfile..."
      docker build -t "$IMAGE" -f "$root/Dockerfile" "$root"
    fi
    resolve_docker_host
    if [ -n "${DOCKER_HOST:-}" ]; then
      echo "Using Docker host: $DOCKER_HOST"
    fi
    go build -mod=vendor -o "$root/aibitat-harness-go" .
    "$root/aibitat-harness-go" --agent "$root/agents/chatter"
    ;;
  clean)
    rm -rf "$root/vendor" "$root/.go" "$root/aibitat-harness-go"
    ;;
  *)
    usage
    exit 1
    ;;
esac
