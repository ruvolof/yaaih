package session

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/ruvolof/aibitat-harness-go/internal/llm"
)

func testHeader(id ID) Header {
	return Header{
		ID:        id,
		Started:   "2026-06-05T10:11:22Z",
		AgentPath: "/agents/devops.md",
		Model:     "deepseek/deepseek-v4-flash",
		Provider:  "https://openrouter.ai/api/v1",
	}
}

func TestWriteCloseRoundTrip(t *testing.T) {
	base := t.TempDir()
	id := ID("20260605-101122-aGzX")

	w, err := NewWriter(base, testHeader(id))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: "You are a DevOps Agent."},
		{Role: llm.RoleUser, Content: "deploy staging"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "call_abc", Name: "shell_exec", Arguments: `{"command":"ls"}`}}},
		{Role: llm.RoleTool, ToolCallID: "call_abc", Name: "shell_exec", Content: "README.md\nsrc/\n"},
		{Role: llm.RoleAssistant, Content: "Done."},
	}
	for i, m := range msgs {
		if err := w.Append(Entry{Seq: i + 1, Role: string(m.Role), Content: m.Content, ToolCalls: m.ToolCalls, ToolCallID: m.ToolCallID, Name: m.Name}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	if err := w.Close("clean"); err != nil {
		t.Fatalf("Close: %v", err)
	}

	hdr, got, _, err := Load(base, id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if hdr.ID != id || hdr.Model != "deepseek/deepseek-v4-flash" || hdr.Version != formatVersion {
		t.Errorf("header mismatch: %+v", hdr)
	}
	if len(got) != len(msgs) {
		t.Fatalf("got %d messages, want %d", len(got), len(msgs))
	}
	if got[2].ToolCalls[0].ID != "call_abc" || got[3].ToolCallID != "call_abc" {
		t.Errorf("tool call round-trip broken: %+v / %+v", got[2], got[3])
	}
	if got[4].Content != "Done." {
		t.Errorf("last message = %+v", got[4])
	}
}

func TestLoadPartialElidesDangling(t *testing.T) {
	base := t.TempDir()
	id := ID("20260605-101122-PART")

	w, err := NewWriter(base, testHeader(id))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	// System, user, then an assistant tool_call with NO following tool result,
	// then simulate a crash by never writing a footer.
	_ = w.Append(Entry{Seq: 1, Role: "system", Content: "sys"})
	_ = w.Append(Entry{Seq: 2, Role: "user", Content: "go"})
	_ = w.Append(Entry{Seq: 3, Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_x", Name: "shell_exec", Arguments: "{}"}}})
	// Simulate a crash: release the OS handle without writing a footer. (A real
	// crash leaves the file open until the process dies; closing the fd here
	// just lets the test's TempDir cleanup remove it on Windows.)
	_ = w.f.Close()

	_, got, _, err := Load(base, id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d messages, want 2 (dangling assistant elided): %+v", len(got), got)
	}
	if got[len(got)-1].Role != llm.RoleUser {
		t.Errorf("last message after elision = %q, want user", got[len(got)-1].Role)
	}
}

func TestLoadKeepsAnsweredToolCalls(t *testing.T) {
	base := t.TempDir()
	id := ID("20260605-101122-KEEP")
	w, _ := NewWriter(base, testHeader(id))
	_ = w.Append(Entry{Seq: 1, Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_y", Name: "shell_exec"}}})
	_ = w.Append(Entry{Seq: 2, Role: "tool", ToolCallID: "call_y", Name: "shell_exec", Content: "ok"})
	_ = w.Close("clean")

	_, got, _, err := Load(base, id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("answered tool_call should be kept: got %d", len(got))
	}
}

func TestOpenForAppendNoDuplicateHeader(t *testing.T) {
	base := t.TempDir()
	id := ID("20260605-101122-RESM")
	w, _ := NewWriter(base, testHeader(id))
	_ = w.Append(Entry{Seq: 1, Role: "system", Content: "sys"})
	_ = w.Close("clean")

	// Reopen and append more, as the --resume path does.
	w2, err := OpenForAppend(base, id)
	if err != nil {
		t.Fatalf("OpenForAppend: %v", err)
	}
	_ = w2.Append(Entry{Seq: 2, Role: "user", Content: "more"})
	_ = w2.Close("clean")

	// Exactly one header line should be present.
	data, _ := os.ReadFile(filepath.Join(base, string(id)+".jsonl"))
	headers := 0
	for _, line := range splitLines(string(data)) {
		if line != "" && containsType(line, "header") {
			headers++
		}
	}
	if headers != 1 {
		t.Errorf("found %d header lines, want exactly 1", headers)
	}
}

func TestConcurrentAppends(t *testing.T) {
	base := t.TempDir()
	id := ID("20260605-101122-CONC")
	w, _ := NewWriter(base, testHeader(id))

	var wg sync.WaitGroup
	const n = 50
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = w.Append(Entry{Seq: i + 1, Role: "user", Content: "msg"})
		}(i)
	}
	wg.Wait()
	_ = w.Close("clean")

	_, got, _, err := Load(base, id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != n {
		t.Errorf("got %d messages, want %d (no interleaving / lost writes)", len(got), n)
	}
}

func TestLoadSumsUsage(t *testing.T) {
	base := t.TempDir()
	id := ID("20260605-101122-USAG")
	w, _ := NewWriter(base, testHeader(id))
	_ = w.Append(Entry{Seq: 1, Role: "user", Content: "hi"})
	_ = w.Append(Entry{Seq: 2, Role: "assistant", Content: "a", Usage: &Usage{InputTokens: 100, OutputTokens: 20, Cost: 0.01, CostAvailable: true}})
	_ = w.Append(Entry{Seq: 3, Role: "user", Content: "more"})
	_ = w.Append(Entry{Seq: 4, Role: "assistant", Content: "b", Usage: &Usage{InputTokens: 150, OutputTokens: 30, Cost: 0.02, CostAvailable: true}})
	_ = w.Close("clean")

	_, _, usage, err := Load(base, id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if usage.InputTokens != 250 || usage.OutputTokens != 50 {
		t.Errorf("tokens = %d/%d, want 250/50", usage.InputTokens, usage.OutputTokens)
	}
	if !usage.CostAvailable || usage.Cost < 0.029 || usage.Cost > 0.031 {
		t.Errorf("cost = %v (available=%v), want ~0.03", usage.Cost, usage.CostAvailable)
	}
}

func TestRenameKeepsWritingAndMovesFile(t *testing.T) {
	base := t.TempDir()
	w, err := NewWriter(base, testHeader("sess-old"))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	_ = w.Append(Entry{Seq: 1, Role: "system", Content: "sys"})

	if err := w.Rename(base, "sess-new"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if w.ID() != "sess-new" {
		t.Errorf("ID() = %q after rename, want sess-new", w.ID())
	}
	// Writing still works and lands in the renamed file.
	_ = w.Append(Entry{Seq: 2, Role: "user", Content: "hi"})
	_ = w.Close("clean")

	if _, err := os.Stat(filepath.Join(base, "sess-old.jsonl")); !os.IsNotExist(err) {
		t.Errorf("old file should be gone, stat err = %v", err)
	}
	_, msgs, _, err := Load(base, "sess-new")
	if err != nil {
		t.Fatalf("Load(sess-new): %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages after rename, want 2", len(msgs))
	}
}

func TestRenameRejectsExistingAndInvalid(t *testing.T) {
	base := t.TempDir()
	w, _ := NewWriter(base, testHeader("a"))
	defer w.Close("clean")
	other, _ := NewWriter(base, testHeader("b"))
	_ = other.Close("clean")

	if err := w.Rename(base, "b"); err == nil {
		t.Error("Rename to existing id should fail")
	}
	if err := w.Rename(base, "../escape"); err == nil {
		t.Error("Rename to a path-traversal id should fail")
	}
	// w must still be usable under its original name after the failed renames.
	if w.ID() != "a" {
		t.Errorf("ID() = %q, want a (unchanged after failed rename)", w.ID())
	}
	if err := w.Append(Entry{Seq: 1, Role: "user", Content: "still works"}); err != nil {
		t.Errorf("Append after failed rename: %v", err)
	}
}

func TestValidateID(t *testing.T) {
	for _, ok := range []ID{"abc", "20260605-101122-aGzX", "my.session_1-2"} {
		if err := ValidateID(ok); err != nil {
			t.Errorf("ValidateID(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []ID{"", "../x", "a/b", "a b", `a\b`} {
		if err := ValidateID(bad); err == nil {
			t.Errorf("ValidateID(%q) = nil, want error", bad)
		}
	}
}

func TestLoadUnknownID(t *testing.T) {
	if _, _, _, err := Load(t.TempDir(), ID("nope")); err == nil {
		t.Error("Load(unknown) returned nil error, want error")
	}
}

func TestListReverseChronological(t *testing.T) {
	base := t.TempDir()
	for _, id := range []ID{"20260101-000000-aaaa", "20260301-000000-bbbb", "20260201-000000-cccc"} {
		w, _ := NewWriter(base, testHeader(id))
		_ = w.Close("clean")
	}
	ids, err := List(base)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []ID{"20260301-000000-bbbb", "20260201-000000-cccc", "20260101-000000-aaaa"}
	if len(ids) != len(want) {
		t.Fatalf("got %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("order = %v, want %v", ids, want)
		}
	}
}

func TestListMissingDir(t *testing.T) {
	ids, err := List(t.TempDir())
	if err != nil {
		t.Fatalf("List missing dir: %v", err)
	}
	if ids != nil {
		t.Errorf("List missing dir = %v, want nil", ids)
	}
}

// --- small test helpers (avoid pulling in strings just for these) ---

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}

func containsType(line, typ string) bool {
	return len(line) > 0 && indexOf(line, `"type":"`+typ+`"`) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
