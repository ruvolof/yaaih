package agent

import (
	"sort"
	"testing"

	"github.com/ruvolof/aibitat-harness-go/internal/browser"
	"github.com/ruvolof/aibitat-harness-go/internal/config"
)

// builtinCapabilities lists every built-in and the capability that unlocks it.
// A tool added without a capability tag is reachable by every agent, so a new
// entry here is a deliberate decision rather than an oversight.
var builtinCapabilities = map[string]string{
	"shell_exec":         "shell",
	"browser_navigate":   "browser",
	"browser_content":    "browser",
	"browser_screenshot": "browser",
	"browser_click":      "browser",
	"browser_type":       "browser",
	"browser_eval":       "browser",
	"file_read":          "filesystem",
	"file_write":         "filesystem",
	"file_list":          "filesystem",
}

// fullRegistry builds a registry with every dependency present, so capability
// filtering is the only thing that can withhold a tool. The browser is a bare
// value standing in for "a browser exists" — these tests assert on which tools
// are reachable, never invoke one.
func fullRegistry(t *testing.T) *Registry {
	t.Helper()
	return NewRegistry(
		&config.Config{Runtime: config.RuntimeCfg{Workspace: t.TempDir()}},
		&stubRunner{},
		&browser.Browser{},
	)
}

func definitionNames(r *Registry, caps []string) []string {
	var names []string
	for _, def := range r.Definitions(caps) {
		names = append(names, def.Name)
	}
	sort.Strings(names)
	return names
}

// TestEnabledRequiresCapability is the core of A1: a tool the agent was not
// granted must not be dispatchable, however the model names it.
func TestEnabledRequiresCapability(t *testing.T) {
	registry := fullRegistry(t)

	for name, capability := range builtinCapabilities {
		if _, ok := registry.Lookup(name); !ok {
			t.Fatalf("%s: not registered, test needs updating", name)
		}
		if _, ok := registry.Enabled(name, nil); ok {
			t.Errorf("%s: enabled for an agent with no capabilities", name)
		}
		if _, ok := registry.Enabled(name, []string{"something-else"}); ok {
			t.Errorf("%s: enabled by an unrelated capability", name)
		}
		if _, ok := registry.Enabled(name, []string{capability}); !ok {
			t.Errorf("%s: not enabled by its own capability %q", name, capability)
		}
	}

	if _, ok := registry.Enabled("no_such_tool", []string{"shell", "browser", "filesystem"}); ok {
		t.Error("a name that was never registered resolved to a tool")
	}
}

// TestEnabledMatchesDefinitions pins the invariant behind the bug: the set of
// tools the model is offered and the set it may call are the same set. A tool
// missing from Definitions but reachable through dispatch is exactly the gap
// a hallucinated tool name walks through.
func TestEnabledMatchesDefinitions(t *testing.T) {
	registry := fullRegistry(t)

	for _, caps := range [][]string{
		nil,
		{"shell"},
		{"browser"},
		{"filesystem"},
		{"shell", "filesystem"},
		{"shell", "browser", "filesystem"},
	} {
		offered := map[string]bool{}
		for _, name := range definitionNames(registry, caps) {
			offered[name] = true
			if _, ok := registry.Enabled(name, caps); !ok {
				t.Errorf("caps=%v: %s is offered to the model but not callable", caps, name)
			}
		}
		for name := range builtinCapabilities {
			if _, ok := registry.Enabled(name, caps); ok && !offered[name] {
				t.Errorf("caps=%v: %s is callable but was never offered", caps, name)
			}
		}
	}
}

// TestUntaggedToolIsAlwaysAvailable covers the other half of the filter: script
// tools and any future untagged built-in stay reachable with no capabilities.
func TestUntaggedToolIsAlwaysAvailable(t *testing.T) {
	registry := fullRegistry(t)
	registry.Add(&Tool{Def: toolDef("always_on")})
	registry.Add(&Tool{Def: toolDef("script_tool"), Capability: "scripts"})

	if _, ok := registry.Enabled("always_on", nil); !ok {
		t.Error("untagged tool withheld from an agent with no capabilities")
	}
	if _, ok := registry.Enabled("script_tool", nil); ok {
		t.Error("script tool enabled without the scripts capability")
	}
	if _, ok := registry.Enabled("script_tool", []string{"scripts"}); !ok {
		t.Error("script tool withheld from an agent granted scripts")
	}
}

// TestRegistrySkipsToolsWithoutDependencies is the defence in depth: with no
// sandbox there is no runner or browser to close over, so those tools are never
// registered and cannot be reached even by an agent that declares the
// capability.
func TestRegistrySkipsToolsWithoutDependencies(t *testing.T) {
	registry := NewRegistry(&config.Config{Runtime: config.RuntimeCfg{Workspace: t.TempDir()}}, nil, nil)
	allCaps := []string{"shell", "browser", "filesystem"}

	for name, capability := range builtinCapabilities {
		_, registered := registry.Lookup(name)
		switch capability {
		case "filesystem":
			if !registered {
				t.Errorf("%s: filesystem tools need no sandbox and should still be registered", name)
			}
		default:
			if registered {
				t.Errorf("%s: registered without its dependency", name)
			}
			if _, ok := registry.Enabled(name, allCaps); ok {
				t.Errorf("%s: dispatchable without its dependency", name)
			}
		}
	}

	if got := definitionNames(registry, allCaps); len(got) != 3 {
		t.Errorf("definitions without a sandbox = %v, want the three file_* tools", got)
	}
}
