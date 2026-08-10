// Package docker manages the lifecycle of the agent's runtime container.
//
// The harness runs on the host; tool execution happens inside a single
// long-lived container per session. This package handles:
//
//   - Verifying the Docker daemon is reachable.
//   - Verifying the runtime image is present locally (build it ahead of time
//     with `./build.sh image` or `.\build.ps1 image`).
//   - Starting the session container with the workspace bind-mounted, with
//     Chromium's remote-debugging port published to the host so the browser
//     tool (running in-process on the host via playwright-go) can connect via CDP.
//   - Stopping and removing the container on shutdown.
//
// All other packages depend on this one for a *Runtime handle and use the
// Docker SDK indirectly through it.
package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"

	"github.com/ruvolof/aibitat-harness-go/internal/logging"
)

var log = logging.For("docker")

// CDPPort is the in-container port the harness exposes for Chromium
// remote-debugging. The browser package launches Chromium on 127.0.0.1 and
// uses socat to forward 0.0.0.0:CDPPort → loopback; this port is what's
// published to the host via the container's PortBindings.
const CDPPort = "9222"

// Runtime represents a running session container plus the Docker client used
// to manage it.
type Runtime struct {
	cli         *client.Client
	containerID string
	image       string
	workspace   string // absolute host path bind-mounted at /workspace
	toolsDir    string // absolute host path bind-mounted read-only at /tools ("" disables)
	cdpHostPort string // host port the container's CDPPort is published to
}

// Options configures a session container.
type Options struct {
	// Image is the Docker image tag to use, e.g. "harness-go-runtime:latest".
	// The image must already exist locally; build it with the project's build
	// script (`./build.sh image` or `.\build.ps1 image`).
	Image string
	// Workspace is the host directory bind-mounted read-write at /workspace.
	// Created if missing.
	Workspace string
	// ToolsDir is the host directory bind-mounted read-only at /tools, holding
	// the agent's persistent (promoted) script tools. Empty disables the mount.
	// Created if missing.
	ToolsDir string
	// ProgressWriter receives human-readable progress lines. nil discards them.
	ProgressWriter io.Writer
}

// New verifies the Docker daemon is reachable and the runtime image is
// present locally, then returns an idle Runtime handle. Call Start to actually
// launch the session container.
func New(ctx context.Context, opts Options) (*Runtime, error) {
	if opts.Image == "" {
		return nil, errors.New("docker: image is required")
	}
	if opts.Workspace == "" {
		return nil, errors.New("docker: workspace is required")
	}
	if opts.ProgressWriter == nil {
		opts.ProgressWriter = io.Discard
	}

	resolveDockerHost(ctx)

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker: new client: %w", err)
	}
	ping, err := cli.Ping(ctx)
	if err != nil {
		log.Error("daemon unreachable", "err", err)
		return nil, fmt.Errorf("docker: daemon unreachable: %w", err)
	}
	log.Info("daemon reachable",
		"api_version", ping.APIVersion,
		"os_type", ping.OSType,
	)

	abs, err := filepath.Abs(opts.Workspace)
	if err != nil {
		return nil, fmt.Errorf("docker: resolve workspace: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("docker: create workspace: %w", err)
	}

	var toolsAbs string
	if opts.ToolsDir != "" {
		toolsAbs, err = filepath.Abs(opts.ToolsDir)
		if err != nil {
			return nil, fmt.Errorf("docker: resolve tools dir: %w", err)
		}
		// Created if missing so the read-only bind mount always succeeds, even
		// before the operator has promoted any tools.
		if err := os.MkdirAll(toolsAbs, 0o755); err != nil {
			return nil, fmt.Errorf("docker: create tools dir: %w", err)
		}
	}

	rt := &Runtime{
		cli:       cli,
		image:     opts.Image,
		workspace: abs,
		toolsDir:  toolsAbs,
	}

	if err := rt.verifyImage(ctx); err != nil {
		return nil, err
	}
	return rt, nil
}

// Client exposes the underlying Docker SDK client for callers that need to
// drive Exec, CopyTo/From, etc. directly (e.g. the executor and browser pkg).
func (r *Runtime) Client() *client.Client { return r.cli }

// ContainerID is the ID of the running session container. Empty until Start.
func (r *Runtime) ContainerID() string { return r.containerID }

// CDPEndpoint returns the URL the browser package should pass to
// playwright.Chromium.ConnectOverCDP. Empty until Start.
func (r *Runtime) CDPEndpoint() string {
	if r.cdpHostPort == "" {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%s", r.cdpHostPort)
}

// Workspace returns the absolute host path bind-mounted at /workspace.
func (r *Runtime) Workspace() string { return r.workspace }

// Start creates and starts the session container. Idempotent: a second call
// is a no-op once the container is running.
func (r *Runtime) Start(ctx context.Context) error {
	if r.containerID != "" {
		return nil
	}

	cdpPort, err := nat.NewPort("tcp", CDPPort)
	if err != nil {
		return fmt.Errorf("docker: parse cdp port: %w", err)
	}

	mounts := []mount.Mount{{
		Type:   mount.TypeBind,
		Source: r.workspace,
		Target: "/workspace",
	}}
	if r.toolsDir != "" {
		// Persistent tool folder, read-only so the agent's own shell cannot
		// rewrite promoted tools on the host (that would defeat the operator's
		// manual-promotion gate).
		mounts = append(mounts, mount.Mount{
			Type:     mount.TypeBind,
			Source:   r.toolsDir,
			Target:   "/tools",
			ReadOnly: true,
		})
	}

	hostCfg := &container.HostConfig{
		Mounts: mounts,
		PortBindings: nat.PortMap{
			cdpPort: []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: ""}}, // "" = random free port
		},
		AutoRemove: false,
	}
	cfg := &container.Config{
		Image:        r.image,
		WorkingDir:   "/workspace",
		Tty:          false,
		AttachStdin:  false,
		AttachStdout: false,
		AttachStderr: false,
		Cmd:          []string{"sleep", "infinity"},
		ExposedPorts: nat.PortSet{cdpPort: struct{}{}},
		Labels: map[string]string{
			"aibitat-harness-go.session": "true",
		},
	}

	created, err := r.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, "")
	if err != nil {
		return fmt.Errorf("docker: create container: %w", err)
	}
	r.containerID = created.ID

	if err := r.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		_ = r.cli.ContainerRemove(context.Background(), created.ID, container.RemoveOptions{Force: true})
		r.containerID = ""
		log.Error("container start failed", "err", err, "container_id", created.ID)
		return fmt.Errorf("docker: start container: %w", err)
	}

	// Inspect to resolve the host-assigned port for CDP.
	insp, err := r.cli.ContainerInspect(ctx, created.ID)
	if err != nil {
		return fmt.Errorf("docker: inspect container: %w", err)
	}
	bindings, ok := insp.NetworkSettings.Ports[cdpPort]
	if ok && len(bindings) > 0 {
		r.cdpHostPort = bindings[0].HostPort
	}
	log.Info("container started",
		"container_id", created.ID[:12],
		"image", r.image,
		"workspace", r.workspace,
		"tools_dir", r.toolsDir,
		"cdp_host_port", r.cdpHostPort,
	)
	return nil
}

// Stop stops and removes the session container. Safe to call multiple times.
func (r *Runtime) Stop(ctx context.Context) error {
	if r.containerID == "" {
		return nil
	}
	id := r.containerID
	r.containerID = ""

	timeoutSec := 5
	_ = r.cli.ContainerStop(ctx, id, container.StopOptions{Timeout: &timeoutSec})
	err := r.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true})
	if err != nil {
		log.Warn("container remove failed", "container_id", id[:12], "err", err)
	} else {
		log.Info("container stopped", "container_id", id[:12])
	}
	return err
}

// Close releases the underlying Docker SDK client. Runtime must not be used
// after Close returns. Safe to call multiple times.
func (r *Runtime) Close() error {
	if r.cli == nil {
		return nil
	}
	cli := r.cli
	r.cli = nil
	return cli.Close()
}

// verifyImage fails if the runtime image isn't present locally. The image is
// expected to be built ahead of time by the project's build script.
func (r *Runtime) verifyImage(ctx context.Context) error {
	insp, _, err := r.cli.ImageInspectWithRaw(ctx, r.image)
	if err == nil {
		log.Info("image verified",
			"image", r.image,
			"id", insp.ID,
			"size_bytes", insp.Size,
		)
		return nil
	}
	if client.IsErrNotFound(err) {
		log.Error("image not found locally", "image", r.image)
		return fmt.Errorf("docker: runtime image %q not found locally; build it with `./build.sh image` or `.\\build.ps1 image`", r.image)
	}
	log.Error("image inspect failed", "image", r.image, "err", err)
	return fmt.Errorf("docker: inspect image: %w", err)
}

// resolveDockerHost mirrors build.sh's resolve_docker_host: when DOCKER_HOST
// is not already set, it asks the docker CLI for the active context's
// endpoint and exports it. This makes non-default sockets (rootless Docker,
// Colima, Docker Desktop's WSL socket, Podman, etc.) discoverable by the Go
// client, which otherwise only reads DOCKER_HOST from the environment.
//
// Best-effort: silently no-ops when `docker` is absent or no context is
// configured, leaving client.FromEnv to fall back to its built-in default.
func resolveDockerHost(ctx context.Context) {
	if os.Getenv("DOCKER_HOST") != "" {
		log.Debug("DOCKER_HOST already set, skipping context lookup",
			"docker_host", os.Getenv("DOCKER_HOST"))
		return
	}
	if _, err := osexec.LookPath("docker"); err != nil {
		log.Debug("docker CLI not on PATH, skipping context lookup")
		return
	}

	nameOut, err := osexec.CommandContext(ctx, "docker", "context", "show").Output()
	if err != nil {
		log.Debug("docker context show failed", "err", err)
		return
	}
	ctxName := strings.TrimSpace(string(nameOut))
	if ctxName == "" {
		return
	}

	hostOut, err := osexec.CommandContext(ctx, "docker", "context", "inspect",
		"--format", "{{.Endpoints.docker.Host}}", ctxName).Output()
	if err != nil {
		log.Debug("docker context inspect failed", "context", ctxName, "err", err)
		return
	}
	host := strings.TrimSpace(string(hostOut))
	if host == "" || host == "<no value>" {
		return
	}

	if err := os.Setenv("DOCKER_HOST", host); err != nil {
		log.Warn("could not set DOCKER_HOST", "err", err)
		return
	}
	log.Info("resolved DOCKER_HOST from docker context",
		"context", ctxName, "docker_host", host)
}

// CleanupOrphans removes any prior session containers labelled by this harness
// that may have been left running by a previous crash. Best-effort; logs only.
func CleanupOrphans(ctx context.Context, progress io.Writer) {
	resolveDockerHost(ctx)
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return
	}
	defer cli.Close()

	args := filters.NewArgs(filters.Arg("label", "aibitat-harness-go.session=true"))
	containers, err := cli.ContainerList(ctx, container.ListOptions{All: true, Filters: args})
	if err != nil {
		return
	}
	for _, c := range containers {
		if progress != nil {
			fmt.Fprintf(progress, "removing orphan container %s\n", c.ID[:12])
		}
		log.Info("removing orphan container", "container_id", c.ID[:12])
		_ = cli.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true})
	}
}

// WaitHealthy polls the container until it responds to a trivial exec or the
// deadline expires. Useful right after Start in slow environments.
func (r *Runtime) WaitHealthy(ctx context.Context, deadline time.Duration) error {
	if r.containerID == "" {
		return errors.New("docker: container not started")
	}
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		insp, err := r.cli.ContainerInspect(ctx, r.containerID)
		if err == nil && insp.State != nil && insp.State.Running {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("docker: container not healthy within %s", deadline)
}
