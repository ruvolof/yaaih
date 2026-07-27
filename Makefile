# aibitat-harness-go build targets.
# Runtime sandbox is a Docker container; the host machine is only used to
# build the harness binary.
#
# All Go state (module cache, build cache, GOPATH bin) is kept under .go/
# in this directory so nothing leaks into the developer's global Go install.

BINARY        := aibitat-harness-go
IMAGE         := aibitat-harness-runtime:latest
DOCKERFILE    := Dockerfile

# Project-local Go caches.
export GOPATH      := $(CURDIR)/.go
export GOMODCACHE  := $(CURDIR)/.go/pkg/mod
export GOCACHE     := $(CURDIR)/.go/cache
export GOBIN       := $(CURDIR)/.go/bin

.PHONY: all tidy vendor build image test clean run

all: vendor build image

# Resolve and download dependencies into the project-local module cache.
tidy:
	go mod tidy

# Vendor Go dependencies into ./vendor/ for hermetic builds.
vendor: tidy
	go mod vendor

# Build the harness binary (vendored).
build:
	go build -mod=vendor -o $(BINARY) .

# Build the Docker runtime image (sandbox for the agent).
image:
	docker build -t $(IMAGE) -f $(DOCKERFILE) .

# Run the test suite.
test:
	go test -mod=vendor ./...

# Remove build artifacts, vendored deps, and the project-local Go caches.
clean:
	rm -rf vendor/ .go/ $(BINARY) $(BINARY).exe

# Convenience: run with the example agent in this directory.
run: build
	./$(BINARY) --agent ./agent.md
