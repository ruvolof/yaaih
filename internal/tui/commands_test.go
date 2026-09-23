package tui

import (
	"strings"
	"testing"

	"github.com/ruvolof/aibitat-harness-go/internal/agent"
	"github.com/ruvolof/aibitat-harness-go/internal/scripttools"
)

func TestLookupCommand(t *testing.T) {
	tests := []struct {
		name      string
		want      string // resolved Command.Name, or "" if not found
		wantFound bool
	}{
		{"exit", "exit", true},
		{"quit", "exit", true}, // alias resolves to the exit command
		{"help", "help", true},
		{"xyz", "", false},
		{"", "", false},
	}
	for _, tt := range tests {
		c, ok := lookupCommand(tt.name)
		if ok != tt.wantFound {
			t.Errorf("lookupCommand(%q) found=%v, want %v", tt.name, ok, tt.wantFound)
			continue
		}
		if ok && c.Name != tt.want {
			t.Errorf("lookupCommand(%q) resolved to %q, want %q", tt.name, c.Name, tt.want)
		}
	}
}

func TestSplitCommand(t *testing.T) {
	tests := []struct {
		in       string
		wantName string
		wantArgs string
	}{
		{"/exit", "exit", ""},
		{"/save my-name", "save", "my-name"},
		{"/model gpt-4   ", "model", "gpt-4"},
		{"/load   id with spaces", "load", "id with spaces"},
		{"/", "", ""},
		{"/  ", "", ""},
		{"  /help  ", "help", ""},
	}
	for _, tt := range tests {
		name, args := splitCommand(tt.in)
		if name != tt.wantName || args != tt.wantArgs {
			t.Errorf("splitCommand(%q) = (%q, %q), want (%q, %q)",
				tt.in, name, args, tt.wantName, tt.wantArgs)
		}
	}
}

func TestSortedCommandsDedupAliases(t *testing.T) {
	cmds := sortedCommands()
	seen := map[string]int{}
	for _, c := range cmds {
		seen[c.Name]++
	}
	if seen["exit"] != 1 {
		t.Errorf("exit appears %d times in sortedCommands, want 1 (aliases must dedup)", seen["exit"])
	}
	// Sorted by name: help should precede a hypothetical later command and
	// exit should come before help.
	for i := 1; i < len(cmds); i++ {
		if cmds[i-1].Name > cmds[i].Name {
			t.Errorf("sortedCommands not sorted: %q before %q", cmds[i-1].Name, cmds[i].Name)
		}
	}
}

// Compaction rewrites the conversation a running turn is reading, so it has to
// wait for one — and say how to stop the turn if the operator does not want to.
func TestCompactRefusesWhileATurnIsRunning(t *testing.T) {
	command, ok := lookupCommand("compact")
	if !ok {
		t.Fatal("/compact is not registered")
	}
	model := &Model{busy: true, ag: &agent.Agent{}}

	if cmd := command.Handler(model, ""); cmd != nil {
		t.Error("/compact started a summarising call on top of a running turn")
	}
	notice := model.chatBuf.String()
	if !strings.Contains(notice, "busy") || !strings.Contains(notice, "esc") {
		t.Errorf("notice = %q, want it to say the agent is busy and how to stop it", stripANSI(notice))
	}
}

func TestCompactRefusesWithoutAnAgent(t *testing.T) {
	command, _ := lookupCommand("compact")
	model := &Model{}

	if cmd := command.Handler(model, ""); cmd != nil {
		t.Error("/compact ran with no agent selected")
	}
	if !strings.Contains(stripANSI(model.chatBuf.String()), "no agent selected") {
		t.Errorf("notice = %q, want it to point at the agent picker", stripANSI(model.chatBuf.String()))
	}
}

// /reload is where the operator sees what they just armed: which tools the
// agent wrote itself, and which will run without asking on an agent that asks.
func TestReloadNamesDraftsAndToolsThatSkipApproval(t *testing.T) {
	registered := []scripttools.Registered{
		{Name: "check_url", Restricted: true},
		{Name: "fetch", Restricted: false},
		{Name: "ping", Draft: true, Restricted: true},
	}

	if got, want := describeScriptTools(registered, true),
		"check_url, fetch (no approval), ping (workspace draft)"; got != want {
		t.Errorf("with approval required: %q, want %q", got, want)
	}
	if got, want := describeScriptTools(registered, false),
		"check_url, fetch, ping (workspace draft)"; got != want {
		t.Errorf("without approval required: %q, want %q", got, want)
	}
}
