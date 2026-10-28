package session

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ruvolof/yaaih/internal/llm"
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

// A tool result far past any line buffer has to come back whole: skipping it
// would leave the call before it unanswered in the middle of the conversation.
func TestLoadKeepsAnOversizedLine(t *testing.T) {
	base := t.TempDir()
	id := ID("20260605-101122-HUGE")
	hugeOutput := strings.Repeat("x", 20*1024*1024)
	w, err := NewWriter(base, testHeader(id))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	appendMessages(t, w, []llm.Message{
		{Role: llm.RoleUser, Content: "cat the log"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "call_big", Name: "shell_exec"}}},
		{Role: llm.RoleTool, ToolCallID: "call_big", Name: "shell_exec", Content: hugeOutput},
		{Role: llm.RoleAssistant, Content: "That is a big log."},
	})
	_ = w.Close("clean")

	_, got, _, err := Load(base, id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d messages, want 4", len(got))
	}
	if got[2].ToolCallID != "call_big" || got[2].Content != hugeOutput {
		t.Errorf("oversized tool result not restored: call %q, %d bytes", got[2].ToolCallID, len(got[2].Content))
	}
	if got[3].Content != "That is a big log." {
		t.Errorf("message after the oversized line = %+v", got[3])
	}
}

func TestLoadReadsAFinalLineWithoutNewline(t *testing.T) {
	base := t.TempDir()
	id := ID("20260605-101122-TAIL")
	transcript := `{"type":"header","version":1,"id":"20260605-101122-TAIL"}` + "\r\n" +
		`{"type":"message","seq":1,"role":"user","content":"hi"}` + "\r\n" +
		`{"type":"message","seq":2,"role":"assistant","content":"hello"}`
	if err := os.WriteFile(filepath.Join(base, string(id)+".jsonl"), []byte(transcript), 0o644); err != nil {
		t.Fatal(err)
	}

	_, got, _, err := Load(base, id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 2 || got[1].Content != "hello" {
		t.Fatalf("got %+v, want the unterminated last message kept", got)
	}
}

func TestLoadSkipsATruncatedFinalLine(t *testing.T) {
	base := t.TempDir()
	id := ID("20260605-101122-CUT")
	transcript := `{"type":"message","seq":1,"role":"user","content":"hi"}` + "\n" +
		`{"type":"message","seq":2,"role":"assistant","content":"hel`
	if err := os.WriteFile(filepath.Join(base, string(id)+".jsonl"), []byte(transcript), 0o644); err != nil {
		t.Fatal(err)
	}

	_, got, _, err := Load(base, id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 || got[0].Content != "hi" {
		t.Fatalf("got %+v, want only the complete line", got)
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

func TestResolveRefLatestIsTheMostRecentlyWritten(t *testing.T) {
	sessionsDir := t.TempDir()
	for _, id := range []ID{"20260101-000000-aaaa", "20260301-000000-bbbb", "renamed"} {
		w, err := NewWriter(sessionsDir, testHeader(id))
		if err != nil {
			t.Fatalf("NewWriter: %v", err)
		}
		_ = w.Close("clean")
	}
	start := time.Now()
	for offset, id := range []ID{"20260301-000000-bbbb", "renamed", "20260101-000000-aaaa"} {
		modified := start.Add(time.Duration(offset) * time.Hour)
		if err := os.Chtimes(filepath.Join(sessionsDir, string(id)+".jsonl"), modified, modified); err != nil {
			t.Fatalf("Chtimes: %v", err)
		}
	}
	id, err := ResolveRef(sessionsDir, LatestRef)
	if err != nil {
		t.Fatalf("ResolveRef(latest): %v", err)
	}
	if id != "20260101-000000-aaaa" {
		t.Errorf("ResolveRef(latest) = %q, want the last written 20260101-000000-aaaa", id)
	}
}

func TestResolveRefLatestWithoutSessions(t *testing.T) {
	if _, err := ResolveRef(filepath.Join(t.TempDir(), "missing"), LatestRef); err == nil {
		t.Error("ResolveRef(latest) on a missing dir = nil error, want error")
	}
}

func TestResolveRefAcceptsTheSessionFile(t *testing.T) {
	sessionsDir := t.TempDir()
	w, err := NewWriter(sessionsDir, testHeader("20260605-101122-aGzX"))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	_ = w.Close("clean")

	t.Chdir(filepath.Dir(sessionsDir))
	relative := filepath.Join(filepath.Base(sessionsDir), "20260605-101122-aGzX.jsonl")
	for _, ref := range []string{
		"20260605-101122-aGzX",
		"20260605-101122-aGzX.jsonl",
		filepath.Join(sessionsDir, "20260605-101122-aGzX.jsonl"),
		relative,
	} {
		id, err := ResolveRef(sessionsDir, ref)
		if err != nil {
			t.Errorf("ResolveRef(%q): %v", ref, err)
			continue
		}
		if id != "20260605-101122-aGzX" {
			t.Errorf("ResolveRef(%q) = %q, want 20260605-101122-aGzX", ref, id)
		}
	}
}

func TestResolveRefRejectsAFileOutsideTheSessionsDir(t *testing.T) {
	sessionsDir, otherDir := t.TempDir(), t.TempDir()
	w, err := NewWriter(otherDir, testHeader("elsewhere"))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	_ = w.Close("clean")
	if _, err := ResolveRef(sessionsDir, filepath.Join(otherDir, "elsewhere.jsonl")); err == nil {
		t.Error("ResolveRef(file in another dir) = nil error, want error")
	}
}

func TestResolveRefRejectsAnInvalidID(t *testing.T) {
	if _, err := ResolveRef(t.TempDir(), "a/b"); err == nil {
		t.Error("ResolveRef(a/b) = nil error, want error")
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

// appendMessages is the transcript a compaction is written on top of.
func appendMessages(t *testing.T, w *Writer, msgs []llm.Message) {
	t.Helper()
	for i, m := range msgs {
		entry := Entry{
			Seq: i + 1, Role: string(m.Role), Content: m.Content,
			ToolCalls: m.ToolCalls, ToolCallID: m.ToolCallID, Name: m.Name,
		}
		if err := w.Append(entry); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
}

// A compacted session has to reopen compacted. Anything else would restore the
// conversation the operator paid a model call to be rid of.
func TestLoadAppliesCompaction(t *testing.T) {
	base := t.TempDir()
	id := ID("20260605-101122-cmpA")
	w, err := NewWriter(base, testHeader(id))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	appendMessages(t, w, []llm.Message{
		{Role: llm.RoleSystem, Content: "You are a DevOps Agent."},
		{Role: llm.RoleUser, Content: "deploy staging"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "call_abc", Name: "shell_exec", Arguments: `{"command":"ls"}`}}},
		{Role: llm.RoleTool, ToolCallID: "call_abc", Name: "shell_exec", Content: "README.md\n"},
		{Role: llm.RoleAssistant, Content: "Deployed."},
		{Role: llm.RoleUser, Content: "now the metrics"},
	})
	if err := w.AppendCompaction(Compaction{
		Seq: 7, From: 1, Through: 5, Role: string(llm.RoleSystem), Content: "Staging was deployed from README.md.",
	}); err != nil {
		t.Fatalf("AppendCompaction: %v", err)
	}
	if err := w.Close("clean"); err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, msgs, _, err := Load(base, id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []llm.Message{
		{Role: llm.RoleSystem, Content: "You are a DevOps Agent."},
		{Role: llm.RoleSystem, Content: "Staging was deployed from README.md."},
		{Role: llm.RoleUser, Content: "now the metrics"},
	}
	if len(msgs) != len(want) {
		t.Fatalf("loaded %d messages, want %d: %+v", len(msgs), len(want), msgs)
	}
	for i := range want {
		if msgs[i].Role != want[i].Role || msgs[i].Content != want[i].Content {
			t.Errorf("message %d = %+v, want %+v", i, msgs[i], want[i])
		}
	}
}

// Messages recorded after a compaction belong to the compacted conversation,
// not to the one that was replaced.
func TestLoadKeepsWritingAfterACompaction(t *testing.T) {
	base := t.TempDir()
	id := ID("20260605-101122-cmpB")
	w, err := NewWriter(base, testHeader(id))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	appendMessages(t, w, []llm.Message{
		{Role: llm.RoleSystem, Content: "system"},
		{Role: llm.RoleUser, Content: "first"},
		{Role: llm.RoleAssistant, Content: "first reply"},
		{Role: llm.RoleUser, Content: "second"},
	})
	if err := w.AppendCompaction(Compaction{
		Seq: 5, From: 1, Through: 3, Role: string(llm.RoleSystem), Content: "summary",
	}); err != nil {
		t.Fatalf("AppendCompaction: %v", err)
	}
	if err := w.Append(Entry{Seq: 6, Role: string(llm.RoleAssistant), Content: "second reply"}); err != nil {
		t.Fatalf("Append after compaction: %v", err)
	}
	if err := w.Close("clean"); err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, msgs, _, err := Load(base, id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := make([]string, len(msgs))
	for i, m := range msgs {
		got[i] = string(m.Role) + ":" + m.Content
	}
	want := []string{"system:system", "system:summary", "user:second", "assistant:second reply"}
	if len(got) != len(want) {
		t.Fatalf("loaded %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("loaded %v, want %v", got, want)
		}
	}
}

// A range that does not fit the conversation means the file and the reader
// disagree. Losing the compaction beats losing the session.
func TestLoadSurvivesAnImpossibleCompactionRange(t *testing.T) {
	base := t.TempDir()
	id := ID("20260605-101122-cmpC")
	w, err := NewWriter(base, testHeader(id))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	appendMessages(t, w, []llm.Message{
		{Role: llm.RoleSystem, Content: "system"},
		{Role: llm.RoleUser, Content: "first"},
	})
	if err := w.AppendCompaction(Compaction{
		Seq: 3, From: 1, Through: 99, Role: string(llm.RoleSystem), Content: "summary",
	}); err != nil {
		t.Fatalf("AppendCompaction: %v", err)
	}
	if err := w.Close("clean"); err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, msgs, _, err := Load(base, id)
	if err != nil {
		t.Fatalf("Load returned an error rather than a usable session: %v", err)
	}
	if len(msgs) != 2 || msgs[1].Content != "summary" {
		t.Errorf("loaded %+v, want the system prompt and the clamped summary", msgs)
	}
}

// A session resumed under an edited agent.md carried on with the new prompt, so
// that is the prompt it has to reopen with — and a compaction written after the
// change still indexes the conversation the agent was holding.
func TestLoadReplaysASystemPromptChange(t *testing.T) {
	base := t.TempDir()
	id := ID("20260605-101122-prmA")
	w, err := NewWriter(base, testHeader(id))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	appendMessages(t, w, []llm.Message{
		{Role: llm.RoleSystem, Content: "You are a DevOps Agent."},
		{Role: llm.RoleUser, Content: "deploy staging"},
		{Role: llm.RoleAssistant, Content: "Deployed."},
	})
	if err := w.AppendSystemPrompt(SystemPrompt{Seq: 4, Content: "You are a careful DevOps Agent."}); err != nil {
		t.Fatalf("AppendSystemPrompt: %v", err)
	}
	if err := w.Append(Entry{Seq: 5, Role: string(llm.RoleUser), Content: "now production"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := w.AppendCompaction(Compaction{
		Seq: 6, From: 1, Through: 3, Role: string(llm.RoleSystem), Content: "Staging was deployed.",
	}); err != nil {
		t.Fatalf("AppendCompaction: %v", err)
	}
	if err := w.Close("clean"); err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, msgs, _, err := Load(base, id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []llm.Message{
		{Role: llm.RoleSystem, Content: "You are a careful DevOps Agent."},
		{Role: llm.RoleSystem, Content: "Staging was deployed."},
		{Role: llm.RoleUser, Content: "now production"},
	}
	if len(msgs) != len(want) {
		t.Fatalf("loaded %d messages, want %d: %+v", len(msgs), len(want), msgs)
	}
	for i := range want {
		if msgs[i].Role != want[i].Role || msgs[i].Content != want[i].Content {
			t.Errorf("message %d = %+v, want %+v", i, msgs[i], want[i])
		}
	}
}

func TestLoadTranscriptKeepsTheLastRecordedSkills(t *testing.T) {
	base := t.TempDir()
	id := ID("20260605-101122-sklA")
	w, err := NewWriter(base, testHeader(id))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	appendMessages(t, w, []llm.Message{
		{Role: llm.RoleSystem, Content: "prompt with no skills"},
		{Role: llm.RoleUser, Content: "check the network"},
	})
	for seq, record := range []SystemPrompt{
		{Content: "prompt with network-debug", Skills: []string{"network-debug"}},
		{Content: "prompt with both", Skills: []string{"network-debug", "style"}},
	} {
		record.Seq = 3 + seq
		if err := w.AppendSystemPrompt(record); err != nil {
			t.Fatalf("AppendSystemPrompt: %v", err)
		}
	}
	if err := w.Close("clean"); err != nil {
		t.Fatalf("Close: %v", err)
	}

	transcript, err := LoadTranscript(base, id)
	if err != nil {
		t.Fatalf("LoadTranscript: %v", err)
	}
	if got := strings.Join(transcript.Skills, ","); got != "network-debug,style" {
		t.Errorf("Skills = %v, want the last record's", transcript.Skills)
	}
	if transcript.Messages[0].Content != "prompt with both" {
		t.Errorf("system prompt = %q, want the last record's", transcript.Messages[0].Content)
	}
	if transcript.Header.ID != id {
		t.Errorf("Header.ID = %q, want %q", transcript.Header.ID, id)
	}
}

func TestLoadTranscriptWithoutSkillRecords(t *testing.T) {
	base := t.TempDir()
	id := ID("20260605-101122-sklB")
	w, err := NewWriter(base, testHeader(id))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	appendMessages(t, w, []llm.Message{{Role: llm.RoleSystem, Content: "prompt"}})
	if err := w.AppendSystemPrompt(SystemPrompt{Seq: 2, Content: "edited prompt"}); err != nil {
		t.Fatalf("AppendSystemPrompt: %v", err)
	}
	if err := w.Close("clean"); err != nil {
		t.Fatalf("Close: %v", err)
	}

	transcript, err := LoadTranscript(base, id)
	if err != nil {
		t.Fatalf("LoadTranscript: %v", err)
	}
	if transcript.Skills != nil {
		t.Errorf("Skills = %v, want none", transcript.Skills)
	}
}

func TestWithSystemPrompt(t *testing.T) {
	user := llm.Message{Role: llm.RoleUser, Content: "hello"}
	system := func(prompt string) llm.Message { return llm.Message{Role: llm.RoleSystem, Content: prompt} }

	cases := []struct {
		name        string
		msgs        []llm.Message
		want        []llm.Message
		wantChanged bool
	}{
		{"same prompt", []llm.Message{system("current"), user}, []llm.Message{system("current"), user}, false},
		{"edited prompt", []llm.Message{system("old"), user}, []llm.Message{system("current"), user}, true},
		{"no system message", []llm.Message{user}, []llm.Message{system("current"), user}, true},
		{"empty conversation", nil, []llm.Message{system("current")}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			original := append([]llm.Message(nil), tc.msgs...)
			got, changed := WithSystemPrompt(tc.msgs, "current")
			if changed != tc.wantChanged {
				t.Errorf("changed = %v, want %v", changed, tc.wantChanged)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			for i := range tc.want {
				if got[i].Role != tc.want[i].Role || got[i].Content != tc.want[i].Content {
					t.Errorf("message %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
			for i := range original {
				if tc.msgs[i].Content != original[i].Content {
					t.Errorf("the input was modified at %d: %+v", i, tc.msgs[i])
				}
			}
		})
	}
}

func TestHeaderRecordsLineageOnlyForASpawnedSession(t *testing.T) {
	base := t.TempDir()
	plainID, spawnedID := ID("20260605-101122-pln1"), ID("20260605-101123-spn1")

	plain, err := NewWriter(base, testHeader(plainID))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	_ = plain.Close("clean")
	spawnedHeader := testHeader(spawnedID)
	spawnedHeader.Parent = plainID
	spawnedHeader.Task = "write a rollback plan"
	spawned, err := NewWriter(base, spawnedHeader)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	_ = spawned.Close("clean")

	raw, err := os.ReadFile(filepath.Join(base, string(plainID)+".jsonl"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if first := splitLines(string(raw))[0]; strings.Contains(first, "parent") || strings.Contains(first, "task") {
		t.Errorf("plain header = %s, want no lineage fields", first)
	}
	hdr, _, _, err := Load(base, spawnedID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if hdr.Parent != plainID || hdr.Task != "write a rollback plan" {
		t.Errorf("spawned header = %+v, want its parent and task", hdr)
	}
}
