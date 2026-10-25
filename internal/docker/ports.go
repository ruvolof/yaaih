package docker

import (
	"errors"
	"fmt"
	"strings"

	"github.com/docker/go-connections/nat"
)

// PublishedPortsEnv names the variable set inside the session container to the
// container ports published to the host, comma-separated as they were declared
// ("8040-8050,9000/udp"), so the agent can tell which ports the operator can
// reach.
const PublishedPortsEnv = "YAAIH_PUBLISHED_PORTS"

// maxPublishedPorts caps how many ports one agent may publish. With Docker's
// userland proxy every published port can be a docker-proxy process of its
// own, so a careless "1-65535" would stall the container's start.
const maxPublishedPorts = 100

// PublishedPorts is the parsed form of the ports an agent publishes from its
// session container to the host.
type PublishedPorts struct {
	specs          []string // normalized, with the host IP made explicit
	bindings       nat.PortMap
	containerPorts []string // the container side of each spec, as written
}

// ParsePorts parses port specs in the `docker run -p` syntax, ranges included.
// Two defaults differ from docker's: a spec without a host IP is published on
// 127.0.0.1 rather than on every interface, and a bare container port or range
// ("8000", "8040-8050") is published on the same port numbers of the host
// rather than on random ones, so the operator knows where to connect.
//
// Every container port must map to exactly one fixed host port: no random host
// ports, and a host range must be as long as its container range. The CDP port
// is reserved for the browser.
func ParsePorts(specs []string) (PublishedPorts, error) {
	ports := PublishedPorts{bindings: nat.PortMap{}}
	hostPortsSeen := map[string]bool{}
	published := 0
	for _, spec := range specs {
		normalized := loopbackByDefault(strings.TrimSpace(spec))
		mappings, err := nat.ParsePortSpec(normalized)
		if err != nil {
			return PublishedPorts{}, fmt.Errorf("%q: %w", spec, err)
		}
		for _, mapping := range mappings {
			hostPort := mapping.Binding.HostPort
			switch {
			case hostPort == "":
				return PublishedPorts{}, fmt.Errorf("%q: a host port is required, the operator could not find a random one", spec)
			case strings.Contains(hostPort, "-"):
				return PublishedPorts{}, fmt.Errorf("%q: the host range must be as long as the container range", spec)
			case mapping.Port.Proto() == "tcp" && mapping.Port.Port() == CDPPort:
				return PublishedPorts{}, fmt.Errorf("%q: container port %s is reserved for the browser", spec, CDPPort)
			}
			hostKey := hostPort + "/" + mapping.Port.Proto()
			if hostPortsSeen[hostKey] {
				return PublishedPorts{}, fmt.Errorf("%q: host port %s is published twice", spec, hostKey)
			}
			hostPortsSeen[hostKey] = true
			ports.bindings[mapping.Port] = append(ports.bindings[mapping.Port], mapping.Binding)
			published++
		}
		ports.specs = append(ports.specs, normalized)
		ports.containerPorts = append(ports.containerPorts, normalized[strings.LastIndex(normalized, ":")+1:])
	}
	if published > maxPublishedPorts {
		return PublishedPorts{}, fmt.Errorf("%d ports published, at most %d are allowed", published, maxPublishedPorts)
	}
	return ports, nil
}

// Endpoints says where the operator reaches each spec: the host address and
// ports, followed by the container ports only when they differ
// ("127.0.0.1:8040-8050", "0.0.0.0:9000 → 80", "127.0.0.1:5353/udp").
func (ports PublishedPorts) Endpoints() []string {
	endpoints := make([]string, 0, len(ports.specs))
	for index, spec := range ports.specs {
		hostSide := spec[:strings.LastIndex(spec, ":")]
		containerPorts, protocol, hasProtocol := strings.Cut(ports.containerPorts[index], "/")
		hostPorts := hostSide[strings.LastIndex(hostSide, ":")+1:]
		endpoint := hostSide
		if hasProtocol {
			endpoint += "/" + protocol
		}
		if hostPorts != containerPorts {
			endpoint += " → " + containerPorts
		}
		endpoints = append(endpoints, endpoint)
	}
	return endpoints
}

// loopbackByDefault spells out the host IP and host port that a spec leaves to
// ParsePorts' defaults.
func loopbackByDefault(spec string) string {
	switch strings.Count(spec, ":") {
	case 0:
		containerPorts, _, _ := strings.Cut(spec, "/")
		return "127.0.0.1:" + containerPorts + ":" + spec
	case 1:
		return "127.0.0.1:" + spec
	}
	return spec
}

// isPortConflict reports whether the daemon failed to start a container because
// a host port it was asked to publish is already taken.
func isPortConflict(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "port is already allocated") ||
		strings.Contains(message, "address already in use")
}

var errPortTaken = errors.New("a port in runtime.ports is already in use on the host, by another harness running this agent or by another program")
