package docker

import (
	"os"
	"os/exec"
	"slices"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/go-connections/nat"
)

func TestContainerIsCappedAndCannotGainPrivileges(t *testing.T) {
	sandbox := &Runtime{
		image:     "runtime:test",
		workspace: "/host/workspace",
		toolsDir:  "/host/tools",
		limits:    Limits{MemoryBytes: 2 << 30, CPUs: 1.5, PIDs: 1024},
	}
	cdpPort, _ := nat.NewPort("tcp", CDPPort)
	cfg, hostCfg := sandbox.containerSpec(cdpPort)

	resources := hostCfg.Resources
	if resources.Memory != 2<<30 || resources.MemorySwap != 2<<30 {
		t.Errorf("memory = %d, swap = %d, want both %d", resources.Memory, resources.MemorySwap, 2<<30)
	}
	if resources.NanoCPUs != 1_500_000_000 {
		t.Errorf("NanoCPUs = %d, want 1.5 CPUs", resources.NanoCPUs)
	}
	if resources.PidsLimit == nil || *resources.PidsLimit != 1024 {
		t.Errorf("PidsLimit = %v, want 1024", resources.PidsLimit)
	}
	if !slices.Contains(hostCfg.SecurityOpt, "no-new-privileges") {
		t.Errorf("SecurityOpt = %v, want no-new-privileges", hostCfg.SecurityOpt)
	}
	if hostCfg.Init == nil || !*hostCfg.Init {
		t.Errorf("Init = %v, want an init as PID 1 to reap the processes the shell leaves behind", hostCfg.Init)
	}
	if cfg.User != "" || len(cfg.Env) != 0 {
		t.Errorf("User = %q, Env = %v, want the image's own user and environment", cfg.User, cfg.Env)
	}

	if len(hostCfg.Mounts) != 2 {
		t.Fatalf("mounts = %+v, want workspace and tools", hostCfg.Mounts)
	}
	if workspace := hostCfg.Mounts[0]; workspace.Target != "/workspace" || workspace.ReadOnly {
		t.Errorf("workspace mount = %+v, want read-write at /workspace", workspace)
	}
	if tools := hostCfg.Mounts[1]; tools.Target != "/tools" || !tools.ReadOnly {
		t.Errorf("tools mount = %+v, want read-only at /tools", tools)
	}
}

func TestZeroLimitsLeaveTheContainerUnlimited(t *testing.T) {
	resources := Limits{}.resources()
	if resources.Memory != 0 || resources.MemorySwap != 0 || resources.NanoCPUs != 0 || resources.PidsLimit != nil {
		t.Errorf("resources = %+v, want nothing set", resources)
	}
}

// A uid with no passwd entry in the image gets HOME=/ from Docker, which it
// cannot write; the browser profile and every tool's dotfiles live under HOME.
func TestHostUserGetsAWritableHome(t *testing.T) {
	sandbox := &Runtime{image: "runtime:test", workspace: "/host/workspace", user: "1000:1000"}
	cdpPort, _ := nat.NewPort("tcp", CDPPort)
	cfg, _ := sandbox.containerSpec(cdpPort)

	if cfg.User != "1000:1000" {
		t.Errorf("User = %q, want 1000:1000", cfg.User)
	}
	if !slices.Contains(cfg.Env, "HOME="+sandboxHome) {
		t.Errorf("Env = %v, want HOME=%s", cfg.Env, sandboxHome)
	}
}

func TestCPULimitIsLoweredToWhatTheHostHas(t *testing.T) {
	fitted, clampedFrom := fitLimitsToHost(Limits{CPUs: 4, PIDs: 1024}, 2)
	if fitted.CPUs != 2 || clampedFrom != 4 {
		t.Errorf("fitted = %+v, clampedFrom = %v, want 2 CPUs lowered from 4", fitted, clampedFrom)
	}
	if fitted.PIDs != 1024 {
		t.Errorf("PIDs = %d, want the other limits untouched", fitted.PIDs)
	}

	for _, hostCPUs := range []int{0, 2, 8} {
		fitted, clampedFrom := fitLimitsToHost(Limits{CPUs: 2}, hostCPUs)
		if fitted.CPUs != 2 || clampedFrom != 0 {
			t.Errorf("host with %d CPUs: fitted = %+v, clampedFrom = %v, want the limit as given", hostCPUs, fitted, clampedFrom)
		}
	}
}

func TestSandboxUser(t *testing.T) {
	for _, tc := range []struct {
		name          string
		requested     bool
		uid, gid      int
		rootless      bool
		wantUser      string
		wantExplained bool
	}{
		{name: "not requested", requested: false, uid: 1000, gid: 1000},
		{name: "rootful daemon", requested: true, uid: 1000, gid: 100, wantUser: "1000:100"},
		{name: "rootless daemon", requested: true, uid: 1000, gid: 1000, rootless: true, wantExplained: true},
		{name: "windows host", requested: true, uid: -1, gid: -1, wantExplained: true},
		{name: "not requested on rootless", requested: false, uid: 1000, gid: 1000, rootless: true},
	} {
		user, notApplied := sandboxUser(tc.requested, tc.uid, tc.gid, tc.rootless)
		if user != tc.wantUser {
			t.Errorf("%s: user = %q, want %q", tc.name, user, tc.wantUser)
		}
		if (notApplied != "") != tc.wantExplained {
			t.Errorf("%s: notApplied = %q, want an explanation: %v", tc.name, notApplied, tc.wantExplained)
		}
	}
}

func TestIsRootless(t *testing.T) {
	rootful := []string{"name=apparmor", "name=seccomp,profile=builtin", "name=cgroupns"}
	if isRootless(rootful) {
		t.Errorf("isRootless(%v) = true", rootful)
	}
	rootless := append(rootful, "name=rootless")
	if !isRootless(rootless) {
		t.Errorf("isRootless(%v) = false", rootless)
	}
}

func TestContainerIsLabelledWithItsOwner(t *testing.T) {
	sandbox := &Runtime{image: "runtime:test", workspace: "/host/workspace", owner: owner{host: "workstation", pid: 4242}}
	cdpPort, _ := nat.NewPort("tcp", CDPPort)
	cfg, _ := sandbox.containerSpec(cdpPort)

	if cfg.Labels[sessionLabel] != "true" {
		t.Errorf("labels = %v, want %s=true so the sweep finds it", cfg.Labels, sessionLabel)
	}
	recorded, ok := ownerOf(cfg.Labels)
	if !ok || recorded != sandbox.owner {
		t.Errorf("ownerOf(%v) = %+v, %v, want %+v", cfg.Labels, recorded, ok, sandbox.owner)
	}
}

func TestOnlyContainersOfDeadHarnessesAreOrphans(t *testing.T) {
	sessionContainer := func(id string, labels map[string]string) types.Container {
		labels[sessionLabel] = "true"
		return types.Container{ID: id, Labels: labels}
	}
	ownedBy := func(host string, pid int) map[string]string {
		return owner{host: host, pid: pid}.labels()
	}
	containers := []types.Container{
		sessionContainer("alive-here-0000", ownedBy("workstation", 100)),
		sessionContainer("dead-here-00000", ownedBy("workstation", 200)),
		sessionContainer("other-host-0000", ownedBy("laptop", 200)),
		sessionContainer("no-owner-000000", map[string]string{}),
		sessionContainer("bad-pid-0000000", map[string]string{ownerHostLabel: "workstation", ownerPIDLabel: "x"}),
	}
	alive := func(pid int) bool { return pid == 100 }

	var orphanIDs []string
	for _, orphan := range orphanedContainers(containers, "workstation", alive) {
		orphanIDs = append(orphanIDs, orphan.ID)
	}
	want := []string{"dead-here-00000", "no-owner-000000", "bad-pid-0000000"}
	if !slices.Equal(orphanIDs, want) {
		t.Errorf("orphans = %v, want %v", orphanIDs, want)
	}
}

func TestProcessAlive(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Errorf("processAlive(own pid %d) = false", os.Getpid())
	}
	for _, pid := range []int{0, -1} {
		if processAlive(pid) {
			t.Errorf("processAlive(%d) = true", pid)
		}
	}

	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	exited := exec.Command(testBinary, "-test.run=^$")
	if err := exited.Run(); err != nil {
		t.Fatalf("run a process that exits at once: %v", err)
	}
	if pid := exited.Process.Pid; processAlive(pid) {
		t.Errorf("processAlive(%d) = true after the process exited and was reaped", pid)
	}
}
