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
//   - Capping the container's memory, CPU, and process count so a runaway
//     command is stopped inside the sandbox instead of starving the host.
//   - Stopping and removing the container on shutdown.
//
// All other packages depend on this one for a *Runtime handle and use the
// Docker SDK indirectly through it.
package docker

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
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

// Labels stamped on every session container. The owner labels name the
// harness process that started it, so the orphan sweep can tell a crashed
// harness's leftover from a sandbox another harness is still using.
const (
	sessionLabel   = "aibitat-harness-go.session"
	ownerHostLabel = "aibitat-harness-go.owner-host"
	ownerPIDLabel  = "aibitat-harness-go.owner-pid"
)

// owner identifies a harness process: a pid is only meaningful on the host
// it was issued on.
type owner struct {
	host string
	pid  int
}

func currentOwner() owner {
	host, err := os.Hostname()
	if err != nil {
		log.Warn("hostname unavailable, containers labelled without one", "err", err)
	}
	return owner{host: host, pid: os.Getpid()}
}

func (o owner) labels() map[string]string {
	return map[string]string{
		ownerHostLabel: o.host,
		ownerPIDLabel:  strconv.Itoa(o.pid),
	}
}

// ownerOf reads the owner labels back. ok is false when either label is
// missing or the pid is not a number.
func ownerOf(labels map[string]string) (o owner, ok bool) {
	host, hasHost := labels[ownerHostLabel]
	pid, err := strconv.Atoi(labels[ownerPIDLabel])
	if !hasHost || err != nil {
		return owner{}, false
	}
	return owner{host: host, pid: pid}, true
}

// Runtime represents a running session container plus the Docker client used
// to manage it.
type Runtime struct {
	cli         *client.Client
	containerID string
	image       string
	workspace   string // absolute host path bind-mounted at /workspace
	toolsDir    string // absolute host path bind-mounted read-only at /tools ("" disables)
	limits      Limits
	user        string // uid:gid the container runs as; "" leaves the image's user (root)
	cdpHostPort string // host port the container's CDPPort is published to
	owner       owner
}

// Limits caps what the session container may take from the host. A zero field
// leaves that resource unlimited.
type Limits struct {
	MemoryBytes int64
	CPUs        float64
	// PIDs counts threads as well as processes.
	PIDs int64
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
	// Limits caps the container's memory, CPU, and process count.
	Limits Limits
	// RunAsHostUser runs the container as the invoking user's uid:gid, so files
	// written into the workspace are owned by that user on the host. Ignored
	// where the daemon already maps container root to the invoking user.
	RunAsHostUser bool
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

	// The daemon's view of the host decides two things the options cannot on
	// their own: how many CPUs there are to limit to, and whether a rootless
	// daemon already maps container root to the invoking user. Without it the
	// limits go through unchecked and the user is taken as requested.
	info, err := cli.Info(ctx)
	if err != nil {
		log.Warn("daemon info unavailable", "err", err)
	}

	limits, clampedFrom := fitLimitsToHost(opts.Limits, info.NCPU)
	if clampedFrom > 0 {
		log.Warn("cpu limit above what the daemon has, clamped",
			"requested", clampedFrom, "limit", limits.CPUs)
		fmt.Fprintf(opts.ProgressWriter,
			"runtime.limits.cpus is %g but the Docker host has %d CPUs; limiting to %d\n",
			clampedFrom, info.NCPU, info.NCPU)
	}

	user, notApplied := sandboxUser(opts.RunAsHostUser, os.Getuid(), os.Getgid(), isRootless(info.SecurityOptions))
	if notApplied != "" {
		log.Info("run_as_host_user not applied", "reason", notApplied)
		fmt.Fprintf(opts.ProgressWriter, "runtime.run_as_host_user ignored: %s\n", notApplied)
	}

	rt := &Runtime{
		cli:       cli,
		image:     opts.Image,
		workspace: abs,
		toolsDir:  toolsAbs,
		limits:    limits,
		user:      user,
		owner:     currentOwner(),
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

	cfg, hostCfg := r.containerSpec(cdpPort)
	created, err := r.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, "")
	if err != nil {
		return fmt.Errorf("docker: create container: %w", err)
	}
	r.containerID = created.ID
	for _, warning := range created.Warnings {
		log.Warn("container create warning", "warning", warning)
	}

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
		"memory_bytes", r.limits.MemoryBytes,
		"cpus", r.limits.CPUs,
		"pids", r.limits.PIDs,
		"user", cmp.Or(r.user, "image default"),
	)
	return nil
}

// sandboxHome is HOME inside a container running as the host user. That uid
// usually has no entry in the image's /etc/passwd, and Docker then sets HOME
// to "/", which only root can write: Chromium keeps its profile under HOME, and
// git, pip, and npm write their configuration and caches there.
const sandboxHome = "/tmp"

// containerSpec is the container the session runs in: the workspace and tools
// mounts, the CDP port published on loopback, the resource limits, and the
// user. No-new-privileges stops setuid binaries in the image from handing
// root back to a container that runs as the host user, and keeps root from
// gaining capabilities it was not started with.
func (r *Runtime) containerSpec(cdpPort nat.Port) (*container.Config, *container.HostConfig) {
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
		AutoRemove:  false,
		Resources:   r.limits.resources(),
		SecurityOpt: []string{"no-new-privileges"},
	}
	cfg := &container.Config{
		Image:        r.image,
		User:         r.user,
		WorkingDir:   "/workspace",
		Tty:          false,
		AttachStdin:  false,
		AttachStdout: false,
		AttachStderr: false,
		Cmd:          []string{"sleep", "infinity"},
		ExposedPorts: nat.PortSet{cdpPort: struct{}{}},
		Labels:       r.owner.labels(),
	}
	cfg.Labels[sessionLabel] = "true"
	if r.user != "" {
		cfg.Env = []string{"HOME=" + sandboxHome}
	}
	return cfg, hostCfg
}

// resources converts the limits into Docker's terms. Swap is capped at the
// memory limit, which leaves the container none: a runaway process that swaps
// still thrashes the host's disk, and it is the host the limit protects.
func (limits Limits) resources() container.Resources {
	var resources container.Resources
	if limits.MemoryBytes > 0 {
		resources.Memory = limits.MemoryBytes
		resources.MemorySwap = limits.MemoryBytes
	}
	if limits.CPUs > 0 {
		resources.NanoCPUs = int64(limits.CPUs * 1e9)
	}
	if limits.PIDs > 0 {
		pids := limits.PIDs
		resources.PidsLimit = &pids
	}
	return resources
}

// fitLimitsToHost lowers a CPU limit the Docker host cannot meet, which the
// daemon would otherwise refuse outright. Docker Desktop's VM commonly has
// fewer CPUs than the machine it runs on. clampedFrom is the requested value
// when it was lowered, zero otherwise; hostCPUs of zero means unknown.
func fitLimitsToHost(limits Limits, hostCPUs int) (fitted Limits, clampedFrom float64) {
	if hostCPUs > 0 && limits.CPUs > float64(hostCPUs) {
		clampedFrom = limits.CPUs
		limits.CPUs = float64(hostCPUs)
	}
	return limits, clampedFrom
}

// sandboxUser returns the uid:gid to run the container as, or "" to keep the
// image's own user. notApplied explains a request that was not honoured.
//
// A rootless daemon already runs container root as the invoking user, so files
// in the workspace are already theirs; any other uid maps to one of the
// user's subordinate ids, which the operator cannot write as on the host. On
// Windows there is no numeric uid to pass (os.Getuid returns -1), and Docker
// Desktop's file sharing already gives the operator ownership.
func sandboxUser(requested bool, uid, gid int, rootlessDaemon bool) (user, notApplied string) {
	switch {
	case !requested:
		return "", ""
	case uid < 0:
		return "", "this host has no numeric user id; Docker Desktop already gives workspace files to the invoking user"
	case rootlessDaemon:
		return "", "the Docker daemon is rootless, so container root is already the invoking user on the host"
	}
	return fmt.Sprintf("%d:%d", uid, gid), ""
}

// isRootless reports whether the daemon's security options mark it rootless.
func isRootless(securityOptions []string) bool {
	return slices.ContainsFunc(securityOptions, func(option string) bool {
		return strings.Contains(option, "name=rootless")
	})
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

// CleanupOrphans removes session containers left behind by a harness that is
// no longer running, typically after a crash. Containers whose owning harness
// is still alive are left alone, as are those started from another host, whose
// owner cannot be checked from here. Best-effort; logs only.
func CleanupOrphans(ctx context.Context, progress io.Writer) {
	resolveDockerHost(ctx)
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return
	}
	defer cli.Close()

	args := filters.NewArgs(filters.Arg("label", sessionLabel+"=true"))
	containers, err := cli.ContainerList(ctx, container.ListOptions{All: true, Filters: args})
	if err != nil {
		log.Debug("orphan sweep skipped, cannot list containers", "err", err)
		return
	}
	for _, orphan := range orphanedContainers(containers, currentOwner().host, processAlive) {
		if progress != nil {
			fmt.Fprintf(progress, "removing orphan container %s\n", orphan.ID[:12])
		}
		log.Info("removing orphan container",
			"container_id", orphan.ID[:12],
			"owner_host", orphan.Labels[ownerHostLabel],
			"owner_pid", orphan.Labels[ownerPIDLabel],
		)
		_ = cli.ContainerRemove(ctx, orphan.ID, container.RemoveOptions{Force: true})
	}
}

// orphanedContainers picks the session containers whose owning harness is gone:
// those owned from this host by a pid that is no longer alive, and those with
// no owner labels at all, since nothing marks them as in use.
//
// A pid reused by an unrelated process keeps its old container alive. That
// errs toward leaking a container rather than destroying one in use.
func orphanedContainers(containers []types.Container, host string, alive func(pid int) bool) []types.Container {
	var orphans []types.Container
	for _, candidate := range containers {
		containerOwner, known := ownerOf(candidate.Labels)
		switch {
		case !known:
			orphans = append(orphans, candidate)
		case containerOwner.host != host:
			log.Debug("leaving container owned from another host",
				"container_id", candidate.ID[:12], "owner_host", containerOwner.host)
		case !alive(containerOwner.pid):
			orphans = append(orphans, candidate)
		}
	}
	return orphans
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
