package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ruvolof/yaaih/internal/config"
	"github.com/ruvolof/yaaih/internal/llm"
)

// distinctLines builds output the noise collapsers cannot reduce, so a test
// exercises the byte budget rather than the deduplicator.
func distinctLines(count int) string {
	var out strings.Builder
	for i := range count {
		fmt.Fprintf(&out, "line %06d: content that differs on every single line\n", i)
	}
	return out.String()
}

// toolReply returns the tool message the agent recorded for toolCallID.
func toolReply(t *testing.T, history []llm.Message, toolCallID string) llm.Message {
	t.Helper()
	for _, msg := range history {
		if msg.Role == llm.RoleTool && msg.ToolCallID == toolCallID {
			return msg
		}
	}
	t.Fatalf("no tool reply for %s in history", toolCallID)
	return llm.Message{}
}

// runOneToolCall drives a turn whose scripted response is a single tool call
// and returns the resulting history.
func runOneToolCall(t *testing.T, agent *Agent) []llm.Message {
	t.Helper()
	runTurn(t, agent, "do the thing")
	return agent.History()
}

func agentForToolOutput(t *testing.T, caps []string, output string) (*Agent, *stubRunner) {
	t.Helper()
	runner := &stubRunner{output: output}
	provider := &stubProvider{responses: []*llm.ChatResponse{
		toolCallResponse("call-1", "shell_exec", `{"command":"produce output"}`),
		textResponse("done"),
	}}
	return newTestAgent(t, caps, provider, runner), runner
}

// A result that fits stays byte-for-byte identical: reduction must not be
// something the model has to reason about on ordinary output.
func TestSmallToolResultEntersHistoryUnchanged(t *testing.T) {
	agent, _ := agentForToolOutput(t, []string{"shell"}, "total 3\nfile-a\nfile-b\n")

	reply := toolReply(t, runOneToolCall(t, agent), "call-1")

	if reply.Content != "total 3\nfile-a\nfile-b\n" {
		t.Errorf("content = %q, want it untouched", reply.Content)
	}
}

func TestOversizedToolResultIsBoundedInHistory(t *testing.T) {
	huge := "FIRST LINE\n" + distinctLines(20000) + "LAST LINE\n"
	agent, _ := agentForToolOutput(t, []string{"shell"}, huge)

	reply := toolReply(t, runOneToolCall(t, agent), "call-1")

	if len(reply.Content) > testToolResultBudget*2 {
		t.Errorf("history kept %d bytes against a %d budget", len(reply.Content), testToolResultBudget)
	}
	if !strings.Contains(reply.Content, "FIRST LINE") {
		t.Error("head of the output was lost")
	}
	if !strings.Contains(reply.Content, "LAST LINE") {
		t.Error("tail of the output was lost — that is where failures report")
	}
	if !strings.Contains(reply.Content, "output truncated") {
		t.Error("output was cut without telling the model")
	}
}

// The whole point of the bound: the cost is paid once, not on every later
// request. This asserts the reduced form is what actually goes on the wire.
func TestOversizedToolResultIsBoundedOnTheWire(t *testing.T) {
	huge := strings.Repeat("x", 500_000)
	runner := &stubRunner{output: huge}
	provider := &stubProvider{responses: []*llm.ChatResponse{
		toolCallResponse("call-1", "shell_exec", `{"command":"cat big"}`),
		textResponse("done"),
	}}
	agent := newTestAgent(t, []string{"shell"}, provider, runner)

	runTurn(t, agent, "read the file")

	second := provider.request(1)
	if second == nil {
		t.Fatal("the model never got a second request")
	}
	var wire int
	for _, msg := range second.Messages {
		wire += len(msg.Content)
	}
	if wire > 10_000 {
		t.Errorf("second request carried %d bytes of content, want the tool result bounded", wire)
	}
}

func TestToolResultNoiseIsCollapsedBeforeBudgeting(t *testing.T) {
	var progress strings.Builder
	for percent := 0; percent <= 100; percent++ {
		progress.WriteString("\rProgress: ")
		progress.WriteString(strings.Repeat("=", percent/2))
	}
	output := "\x1b[32mStarting\x1b[0m\n" + progress.String() + "\nFinished\n"
	agent, _ := agentForToolOutput(t, []string{"shell"}, output)

	reply := toolReply(t, runOneToolCall(t, agent), "call-1")

	if strings.Contains(reply.Content, "\x1b[") {
		t.Errorf("escape sequences reached the history: %q", reply.Content)
	}
	if strings.Count(reply.Content, "Progress:") != 1 {
		t.Errorf("kept %d repaints, want 1", strings.Count(reply.Content, "Progress:"))
	}
	// Collapsing alone brings this under budget, so nothing should be dropped.
	if strings.Contains(reply.Content, "output truncated") {
		t.Errorf("truncated output that noise removal had already made fit: %q", reply.Content)
	}
	if !strings.Contains(reply.Content, "Starting") || !strings.Contains(reply.Content, "Finished") {
		t.Error("lost real content while removing noise")
	}
}

// Truncation is only acceptable because the full text stays reachable. The
// marker must name a real file with the real content in it.
func TestTruncatedToolResultSpillsFullOutput(t *testing.T) {
	huge := "HEAD MARKER\n" + distinctLines(5000) + "TAIL MARKER\n"
	agent, _ := agentForToolOutput(t, []string{"shell"}, huge)

	reply := toolReply(t, runOneToolCall(t, agent), "call-1")

	if !strings.Contains(reply.Content, "full output:") {
		t.Fatalf("no spill pointer in the marker: %q", reply.Content)
	}
	containerPath := extractSpillPath(t, reply.Content)
	if !strings.HasPrefix(containerPath, "/workspace/") {
		t.Errorf("spill path %q is not a path the sandbox can open", containerPath)
	}

	hostPath := filepath.Join(agent.cfg.Runtime.Workspace, strings.TrimPrefix(containerPath, "/workspace/"))
	spilled, err := os.ReadFile(hostPath)
	if err != nil {
		t.Fatalf("spill file named in the marker does not exist: %v", err)
	}
	if string(spilled) != huge {
		t.Errorf("spill holds %d bytes, want the full %d", len(spilled), len(huge))
	}
}

// Without a shell the sandbox path is meaningless, so the note has to describe
// the file in the terms file_read understands.
func TestSpillPointerMatchesAvailableTools(t *testing.T) {
	huge := distinctLines(50000)
	runner := &stubRunner{output: huge + "\n" + containerFSMarker + " ok 50000\n"}
	provider := &stubProvider{responses: []*llm.ChatResponse{
		toolCallResponse("call-1", "file_read", `{"path":"big.log"}`),
		textResponse("done"),
	}}
	agent := newTestAgent(t, []string{"filesystem"}, provider, runner)

	reply := toolReply(t, runOneToolCall(t, agent), "call-1")

	if !strings.Contains(reply.Content, "file_read") {
		t.Errorf("marker does not point at a tool this agent has: %q", reply.Content)
	}
	if strings.Contains(reply.Content, "/workspace/") {
		t.Errorf("marker offers a sandbox path to an agent with no sandbox: %q", reply.Content)
	}
}

// The operator reviews a call by its published result. If that were the raw
// output while the model got a truncated one, the operator would be reasoning
// about text the model never saw.
func TestOperatorIsShownTheReplyTheModelReceived(t *testing.T) {
	huge := "FIRST LINE\n" + distinctLines(20000) + "LAST LINE\n"
	agent, _ := agentForToolOutput(t, []string{"shell"}, huge)

	events := runTurn(t, agent, "do the thing")
	reply := toolReply(t, agent.History(), "call-1")

	results := toolResults(events)
	if len(results) != 1 {
		t.Fatalf("tool results = %d, want 1", len(results))
	}
	if results[0].ID != "call-1" {
		t.Errorf("result ID = %q, want call-1", results[0].ID)
	}
	if results[0].Result != reply.Content {
		t.Errorf("operator was shown %d bytes, the model was given %d", len(results[0].Result), len(reply.Content))
	}
}

// An agent with neither a shell nor filesystem access cannot open a spill file,
// so the marker must not advertise one.
func TestNoSpillPointerWithoutAToolToReadIt(t *testing.T) {
	huge := distinctLines(50000)
	runner := &stubRunner{output: huge}
	provider := &stubProvider{responses: []*llm.ChatResponse{
		toolCallResponse("call-1", "noisy_script", `{}`),
		textResponse("done"),
	}}
	agent := newTestAgentWithOptions(t, []string{"scripts"}, provider, runner, config.Options{
		MaxToolIterations:  5,
		MaxToolResultBytes: testToolResultBudget,
	})
	agent.Registry().Add(&Tool{
		Capability: "scripts",
		Def:        toolDef("noisy_script"),
		Fn:         func(ctx context.Context, args string) (string, error) { return huge, nil },
	})

	reply := toolReply(t, runOneToolCall(t, agent), "call-1")

	if !strings.Contains(reply.Content, "output truncated") {
		t.Error("output was not bounded")
	}
	if strings.Contains(reply.Content, "full output:") {
		t.Errorf("marker promises a file the agent cannot open: %q", reply.Content)
	}
}

func TestSpillDirectoryIsPrunedToBudget(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"0001-a.txt", "0002-b.txt", "0003-c.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, 400), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	pruneSpillDir(dir, 900)

	remaining, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 2 {
		t.Fatalf("kept %d files, want 2 within the 900-byte budget", len(remaining))
	}
	if remaining[0].Name() != "0002-b.txt" {
		t.Errorf("kept %q as the oldest survivor, want the newest two", remaining[0].Name())
	}
}

func extractSpillPath(t *testing.T, marker string) string {
	t.Helper()
	_, after, found := strings.Cut(marker, "full output: ")
	if !found {
		t.Fatalf("no spill pointer in %q", marker)
	}
	path, _, _ := strings.Cut(after, " ")
	return path
}
