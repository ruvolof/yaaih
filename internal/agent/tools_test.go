package agent

import (
	"slices"
	"sort"
	"testing"

	"github.com/ruvolof/aibitat-harness-go/internal/browser"
	"github.com/ruvolof/aibitat-harness-go/internal/config"
	"github.com/ruvolof/aibitat-harness-go/internal/llm"
)

// builtinCapabilities lists every built-in and the capability that unlocks it.
// A tool added without a capability tag is reachable by every agent, so a new
// entry here is a deliberate decision rather than an oversight.
var builtinCapabilities = map[string]string{
	"shell_exec":         "shell",
	"browser_navigate":   "browser",
	"browser_back":       "browser",
	"browser_wait_for":   "browser",
	"browser_content":    "browser",
	"browser_screenshot": "browser",
	"browser_click":      "browser",
	"browser_type":       "browser",
	"browser_tabs":       "browser",
	"browser_select_tab": "browser",
	"browser_close_tab":  "browser",
	"browser_eval":       "browser",
	"file_read":          "filesystem",
	"file_write":         "filesystem",
	"file_list":          "filesystem",
}

// allCapabilities is every capability a built-in can be tagged with.
var allCapabilities = []string{"shell", "browser", "filesystem"}

// fullRegistry builds a registry with every dependency present, so capability
// filtering is the only thing that can withhold a tool. The browser is a bare
// value standing in for "a browser exists" — these tests assert on which tools
// are reachable, never invoke one.
func fullRegistry(t *testing.T) *Registry {
	t.Helper()
	return NewRegistry(&config.Config{}, &stubRunner{}, &browser.Browser{})
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
// sandbox there is no runner or browser to close over, so no built-in — the
// filesystem tools execute through the runner too — is ever registered, and
// none can be reached even by an agent that declares the capability.
func TestRegistrySkipsToolsWithoutDependencies(t *testing.T) {
	registry := NewRegistry(&config.Config{}, nil, nil)
	allCaps := []string{"shell", "browser", "filesystem"}

	for name := range builtinCapabilities {
		if _, registered := registry.Lookup(name); registered {
			t.Errorf("%s: registered without its dependency", name)
		}
		if _, ok := registry.Enabled(name, allCaps); ok {
			t.Errorf("%s: dispatchable without its dependency", name)
		}
	}

	if got := definitionNames(registry, allCaps); len(got) != 0 {
		t.Errorf("definitions without a sandbox = %v, want none", got)
	}
}

// The table above is what every capability assertion in this file iterates
// over, so a built-in registered without an entry would be exempt from all of
// them rather than failing anything.
func TestBuiltinCapabilitiesListsEveryRegisteredTool(t *testing.T) {
	for _, name := range definitionNames(fullRegistry(t), allCapabilities) {
		if _, listed := builtinCapabilities[name]; !listed {
			t.Errorf("built-in %q is registered but not listed in builtinCapabilities", name)
		}
	}
}

// /reload rebuilds the script tools from disk. Clearing them must take exactly
// the previous generation — a built-in swept up with them would leave the agent
// short a capability it declared, with nothing in the UI to say so.
func TestRemoveByCapabilityClearsOnlyThatGeneration(t *testing.T) {
	registry := fullRegistry(t)
	builtinsBefore := definitionNames(registry, allCapabilities)

	registry.Add(&Tool{Capability: "scripts", Def: toolDef("deploy_staging")})
	registry.Add(&Tool{Capability: "scripts", Def: toolDef("tail_logs")})

	if _, ok := registry.Enabled("deploy_staging", []string{"scripts"}); !ok {
		t.Fatal("a freshly added script tool is not dispatchable")
	}

	if removed := registry.RemoveByCapability("scripts"); removed != 2 {
		t.Errorf("removed %d tools, want the 2 script tools", removed)
	}
	if _, ok := registry.Lookup("tail_logs"); ok {
		t.Error("a script tool deleted from disk is still registered after a reload")
	}
	if got := definitionNames(registry, allCapabilities); !slices.Equal(got, builtinsBefore) {
		t.Errorf("built-ins after the sweep = %v, want %v", got, builtinsBefore)
	}
	if removed := registry.RemoveByCapability("scripts"); removed != 0 {
		t.Errorf("a second sweep removed %d tools, want 0", removed)
	}
}

// A script tool that takes a built-in's name replaces it, which is how the
// registry is documented to behave; the sweep must then not leave a hole where
// the built-in used to be.
func TestAddReplacesAToolOfTheSameName(t *testing.T) {
	registry := fullRegistry(t)

	registry.Add(&Tool{Capability: "scripts", Def: toolDef("shell_exec")})

	tool, ok := registry.Enabled("shell_exec", []string{"scripts"})
	if !ok {
		t.Fatal("the replacement is not dispatchable under its own capability")
	}
	if tool.Capability != "scripts" {
		t.Errorf("capability = %q, want the replacement's own", tool.Capability)
	}
	if _, ok := registry.Enabled("shell_exec", []string{"shell"}); ok {
		t.Error("the replaced built-in is still reachable under the shell capability")
	}
}

// The tool list is part of every request, and a provider that caches the
// tokenised prefix of a prompt charges full price for one that arrives in a
// different order. Ranging over the registry's map would reorder it on every
// iteration of every turn.
func TestDefinitionsComeBackInTheSameOrderEveryTime(t *testing.T) {
	registry := fullRegistry(t)
	registry.Add(&Tool{Def: llm.ToolDefinition{Name: "aaa_script_tool"}, Capability: "script"})
	registry.Add(&Tool{Def: llm.ToolDefinition{Name: "zzz_script_tool"}, Capability: "script"})

	caps := append(slices.Clone(allCapabilities), "script")
	first := names(registry.Definitions(caps))
	if !slices.IsSorted(first) {
		t.Errorf("definitions = %v, want them by name", first)
	}
	for call := 0; call < 20; call++ {
		if got := names(registry.Definitions(caps)); !slices.Equal(got, first) {
			t.Fatalf("call %d returned %v, want the same order as %v", call, got, first)
		}
	}
}

// names is definitionNames without the sort, for a test about ordering.
func names(defs []llm.ToolDefinition) []string {
	out := make([]string, 0, len(defs))
	for _, def := range defs {
		out = append(out, def.Name)
	}
	return out
}
