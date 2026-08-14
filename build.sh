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
Usage: $0 {tidy|vendor|build|image|test|cover|run|update-playwright|clean}

Targets:
  tidy              - download dependencies into ./.go/pkg/mod
  vendor            - tidy + go mod vendor
  build             - build binary using vendored deps
  image             - build the runtime Docker image from the repo root Dockerfile
  test              - vet + run the test suite under the race detector
  cover             - test with coverage; prints a per-package table and writes an HTML report
  run               - build binary, build runtime image if missing, and run the harness with ./agents/chatter
  update-playwright - upgrade playwright-go to latest stable, then report the matching Dockerfile base image
  clean             - remove vendor/, .go/, and the binary

Environment:
  PKG               - package pattern for test/cover (default ./...), e.g. PKG=./internal/executor/
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
  test)
    go vet -mod=vendor "${PKG:-./...}"
    go test -mod=vendor -race "${PKG:-./...}"
    ;;
  cover)
    coverdir="$root/.go/cover"
    profile="$coverdir/coverage.out"
    mkdir -p "$coverdir"
    go test -mod=vendor -race -covermode=atomic -coverprofile="$profile" -cover "${PKG:-./...}"
    go tool cover -html="$profile" -o "$coverdir/coverage.html"
    echo
    echo "total: $(go tool cover -func="$profile" | awk 'END {print $NF}')"
    echo
    below=$(go tool cover -func="$profile" \
      | awk '$1 != "total:" { pct = $3; sub(/%$/, "", pct); if (pct + 0 < 100) printf "%7.1f%%  %-28s %s\n", pct + 0, $2, $1 }' \
      | sort -n)
    echo "least-covered functions ($(printf '%s\n' "$below" | wc -l) below 100%, showing 40):"
    printf '%s\n' "$below" | head -n 40
    echo
    echo "full per-function report: go tool cover -func=$profile"
    echo "browsable line-by-line:   $coverdir/coverage.html"
    ;;
  update-playwright)
    go get -u github.com/mxschmitt/playwright-go
    go mod tidy
    go mod vendor
    driver_version=$(sed -n 's/.*playwrightCliVersion *= *"\([^"]*\)".*/\1/p' \
      "$root/vendor/github.com/mxschmitt/playwright-go/run.go" | head -n1)
    echo
    if [ -n "$driver_version" ]; then
      echo "playwright-go now targets Playwright v$driver_version."
      echo "Set the Dockerfile base image to mcr.microsoft.com/playwright:v$driver_version-noble, then run: $0 image"
    else
      echo "playwright-go updated, but its target Playwright version could not be read."
      echo "Check playwrightCliVersion in vendor/github.com/mxschmitt/playwright-go/run.go, match the Dockerfile base image, then run: $0 image"
    fi
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
