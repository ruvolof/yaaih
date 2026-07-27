package tui

import "testing"

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
