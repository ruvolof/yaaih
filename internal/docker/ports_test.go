package docker

import (
	"errors"
	"slices"
	"strconv"
	"testing"

	"github.com/docker/go-connections/nat"
)

func TestPortRangeIsPublishedOnLoopbackOnTheSamePorts(t *testing.T) {
	ports, err := ParsePorts([]string{"8040-8050"})
	if err != nil {
		t.Fatalf("ParsePorts: %v", err)
	}
	if len(ports.bindings) != 11 {
		t.Fatalf("bindings = %v, want 11 ports", ports.bindings)
	}
	for number := 8040; number <= 8050; number++ {
		port, _ := nat.NewPort("tcp", strconv.Itoa(number))
		want := []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: strconv.Itoa(number)}}
		if got := ports.bindings[port]; !slices.Equal(got, want) {
			t.Errorf("bindings[%s] = %v, want %v", port, got, want)
		}
	}
	if !slices.Equal(ports.containerPorts, []string{"8040-8050"}) {
		t.Errorf("containerPorts = %v, want [8040-8050]", ports.containerPorts)
	}
}

func TestExplicitHostIPAndRemappingAreKept(t *testing.T) {
	ports, err := ParsePorts([]string{"0.0.0.0:9000-9001:8040-8041", "8443:443", "5353/udp"})
	if err != nil {
		t.Fatalf("ParsePorts: %v", err)
	}
	want := map[nat.Port]nat.PortBinding{
		"8040/tcp": {HostIP: "0.0.0.0", HostPort: "9000"},
		"8041/tcp": {HostIP: "0.0.0.0", HostPort: "9001"},
		"443/tcp":  {HostIP: "127.0.0.1", HostPort: "8443"},
		"5353/udp": {HostIP: "127.0.0.1", HostPort: "5353"},
	}
	if len(ports.bindings) != len(want) {
		t.Fatalf("bindings = %v, want %v", ports.bindings, want)
	}
	for port, binding := range want {
		if got := ports.bindings[port]; len(got) != 1 || got[0] != binding {
			t.Errorf("bindings[%s] = %v, want [%v]", port, got, binding)
		}
	}
	if want := []string{"8040-8041", "443", "5353/udp"}; !slices.Equal(ports.containerPorts, want) {
		t.Errorf("containerPorts = %v, want %v", ports.containerPorts, want)
	}
}

func TestEndpointsNameTheHostSide(t *testing.T) {
	ports, err := ParsePorts([]string{"8040-8050", "0.0.0.0:9000:80", "5353/udp", "[::1]:8443:443/tcp"})
	if err != nil {
		t.Fatalf("ParsePorts: %v", err)
	}
	want := []string{"127.0.0.1:8040-8050", "0.0.0.0:9000 → 80", "127.0.0.1:5353/udp", "[::1]:8443/tcp → 443"}
	if got := ports.Endpoints(); !slices.Equal(got, want) {
		t.Errorf("Endpoints() = %q, want %q", got, want)
	}
}

func TestInvalidPortsAreRejected(t *testing.T) {
	for name, specs := range map[string][]string{
		"not a port":            {"http"},
		"bad host IP":           {"localhost:8000:8000"},
		"random host port":      {"127.0.0.1::8000"},
		"ranges differ":         {"9000-9005:8040-8041"},
		"host range for a port": {"9000-9005:80"},
		"cdp port":              {"9220-9230"},
		"host port twice":       {"8000", "8000:80"},
		"too many":              {"10000-10100"},
	} {
		if _, err := ParsePorts(specs); err == nil {
			t.Errorf("%s: ParsePorts(%q) accepted, want an error", name, specs)
		}
	}
}

func TestSameHostPortOnDifferentProtocolsIsAllowed(t *testing.T) {
	if _, err := ParsePorts([]string{"5353", "5353/udp"}); err != nil {
		t.Errorf("ParsePorts: %v", err)
	}
}

func TestPublishedPortsJoinCDPAndAreAnnouncedInTheEnvironment(t *testing.T) {
	ports, err := ParsePorts([]string{"8040-8041", "9000"})
	if err != nil {
		t.Fatalf("ParsePorts: %v", err)
	}
	sandbox := &Runtime{image: "runtime:test", workspace: "/host/workspace", user: "1000:1000", ports: ports}
	cdpPort, _ := nat.NewPort("tcp", CDPPort)
	cfg, hostCfg := sandbox.containerSpec(cdpPort)

	for _, port := range []nat.Port{cdpPort, "8040/tcp", "8041/tcp", "9000/tcp"} {
		if _, exposed := cfg.ExposedPorts[port]; !exposed {
			t.Errorf("ExposedPorts = %v, missing %s", cfg.ExposedPorts, port)
		}
		if len(hostCfg.PortBindings[port]) != 1 {
			t.Errorf("PortBindings[%s] = %v, want one binding", port, hostCfg.PortBindings[port])
		}
	}
	if cdp := hostCfg.PortBindings[cdpPort][0]; cdp.HostIP != "127.0.0.1" || cdp.HostPort != "" {
		t.Errorf("CDP binding = %+v, want a random loopback port", cdp)
	}
	want := []string{"HOME=" + sandboxHome, PublishedPortsEnv + "=8040-8041,9000"}
	if !slices.Equal(cfg.Env, want) {
		t.Errorf("Env = %v, want %v", cfg.Env, want)
	}
}

func TestPortConflictIsRecognised(t *testing.T) {
	conflicts := []error{
		errors.New("driver failed programming external connectivity on endpoint x: Bind for 127.0.0.1:8045 failed: port is already allocated"),
		errors.New("listen tcp4 127.0.0.1:8045: bind: address already in use"),
	}
	for _, err := range conflicts {
		if !isPortConflict(err) {
			t.Errorf("isPortConflict(%q) = false", err)
		}
	}
	if isPortConflict(errors.New("no such image")) || isPortConflict(nil) {
		t.Error("isPortConflict reported a conflict for an unrelated error")
	}
}
