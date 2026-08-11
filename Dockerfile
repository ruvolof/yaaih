# aibitat-harness-go runtime image.
#
# This image is the sandbox where agent tools execute:
#   - bash session (stateful, attached via `docker exec`)
#   - Chromium with remote debugging (Playwright connects over CDP from host)
#   - common command-line tooling the agent may invoke
#
# Built locally by `make image` or auto-built by the harness on first run.

# This tag must match the Playwright release that the vendored playwright-go
# targets: the harness drives this container's Chromium from the host, and the
# two ends speak a versioned protocol. `./build.sh update-playwright` bumps
# playwright-go and reports the tag to use here.
FROM mcr.microsoft.com/playwright:v1.61.1-noble

# Tooling commonly invoked by agents, plus socat which the harness uses to
# expose Chromium's CDP port: starting with Chrome 132, the browser's
# --remote-debugging-address=0.0.0.0 flag is silently coerced to loopback,
# so the harness runs Chromium on 127.0.0.1 inside the container and uses
# socat to forward 0.0.0.0:9222 → 127.0.0.1:9223 so the host's port mapping
# can actually reach it.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
        bash \
        ca-certificates \
        curl \
        git \
        iproute2 \
        jq \
        ripgrep \
        less \
        nano \
        procps \
        socat \
        unzip \
 && rm -rf /var/lib/apt/lists/*

# Workspace is bind-mounted at runtime.
WORKDIR /workspace

# Default to an interactive bash so `docker exec` can attach cleanly.
# The harness drives this via the Docker exec API, not via CMD.
CMD ["sleep", "infinity"]
