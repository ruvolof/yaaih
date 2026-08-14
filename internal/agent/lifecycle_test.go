package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ruvolof/aibitat-harness-go/internal/llm"
	"github.com/ruvolof/aibitat-harness-go/internal/session"
)

func contentsOf(msgs []llm.Message) string {
	var joined strings.Builder
	for _, msg := range msgs {
		joined.WriteString(msg.Content)
		joined.WriteString("\n")
	}
	return joined.String()
}

// /new has to leave the model with nothing but its system prompt — the whole
// point of starting over — while the transcript it replaced stays complete on
// disk.
func TestNewSessionStartsCleanAndLeavesTheOldTranscriptIntact(t *testing.T) {
	provider := &stubProvider{responses: []*llm.ChatResponse{
		textResponse("first answer"),
		textResponse("second answer"),
	}}
	agent, sessionsDir, firstID := newPersistedTestAgent(t, provider)

	var secondID session.ID
	collectEvents(t, agent, func() {
		agent.Send(context.Background(), "remember the number 7")
		var err error
		secondID, err = agent.NewSession()
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		agent.Send(context.Background(), "what was the number?")
	})

	if secondID == firstID {
		t.Fatalf("NewSession reused the id %q", secondID)
	}
	if got := agent.CurrentSession(); got != secondID {
		t.Errorf("CurrentSession = %q, want the new session %q", got, secondID)
	}
	if got := agent.SessionsDir(); got != sessionsDir {
		t.Errorf("SessionsDir = %q, want %q", got, sessionsDir)
	}

	live := agent.History()
	if want := []string{"system", "user", "assistant"}; !slices.Equal(roles(live), want) {
		t.Fatalf("history roles = %v, want %v", roles(live), want)
	}
	if strings.Contains(contentsOf(live), "number 7") {
		t.Error("the replaced conversation is still in the live history")
	}

	_, replaced, _, err := session.Load(sessionsDir, firstID)
	if err != nil {
		t.Fatalf("load the replaced session: %v", err)
	}
	if !strings.Contains(contentsOf(replaced), "number 7") {
		t.Errorf("the replaced transcript lost its turn: %v", roles(replaced))
	}

	_, started, _, err := session.Load(sessionsDir, secondID)
	if err != nil {
		t.Fatalf("load the new session: %v", err)
	}
	if want := []string{"system", "user", "assistant"}; !slices.Equal(roles(started), want) {
		t.Errorf("new transcript roles = %v, want %v", roles(started), want)
	}
}

// Resuming restores both halves of a session: what was said, and what it cost.
// The run total is deliberately not rewound — it counts what this process spent,
// not what the transcript did.
func TestResumeSessionRestoresHistoryAndSessionUsage(t *testing.T) {
	provider := &stubProvider{responses: []*llm.ChatResponse{
		textResponse("first answer"),
		textResponse("second answer"),
	}}
	agent, _, firstID := newPersistedTestAgent(t, provider)

	var resumed []llm.Message
	collectEvents(t, agent, func() {
		agent.Send(context.Background(), "remember the number 7")
		if _, err := agent.NewSession(); err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		agent.Send(context.Background(), "what was the number?")
		var err error
		resumed, err = agent.ResumeSession(firstID)
		if err != nil {
			t.Fatalf("ResumeSession: %v", err)
		}
	})

	if got := agent.CurrentSession(); got != firstID {
		t.Errorf("CurrentSession = %q, want the resumed %q", got, firstID)
	}
	if !strings.Contains(contentsOf(resumed), "number 7") {
		t.Errorf("resumed history = %v, want the first session's turn", roles(resumed))
	}
	if !slices.Equal(roles(resumed), roles(agent.History())) {
		t.Errorf("the returned history %v differs from the agent's %v", roles(resumed), roles(agent.History()))
	}

	// Each scripted reply reports 10 in / 5 out.
	if stats := agent.SessionStats(); stats.TotalInputTokens != 10 || stats.TotalOutputTokens != 5 {
		t.Errorf("session usage = %d in / %d out, want the resumed session's own 10 / 5",
			stats.TotalInputTokens, stats.TotalOutputTokens)
	}
	if stats := agent.GlobalStats(); stats.TotalInputTokens != 20 {
		t.Errorf("run usage = %d in, want both turns this process ran (20)", stats.TotalInputTokens)
	}
}

// The id comes from a command line flag or a slash command, and it is joined
// onto a path, so it has to be refused before it reaches the filesystem.
func TestResumeSessionRejectsATraversingID(t *testing.T) {
	agent, _, _ := newPersistedTestAgent(t, &stubProvider{})
	defer agent.Close()

	for _, id := range []session.ID{"../../../etc/passwd", "sub/dir", ""} {
		if _, err := agent.ResumeSession(id); err == nil {
			t.Errorf("ResumeSession(%q) was accepted", id)
		}
	}
}

func TestResumeSessionReportsAMissingTranscript(t *testing.T) {
	agent, _, _ := newPersistedTestAgent(t, &stubProvider{})
	defer agent.Close()

	if _, err := agent.ResumeSession("20990101-000000-abcdef"); err == nil {
		t.Error("ResumeSession succeeded for a session that does not exist")
	}
}

// /save renames the file underneath a session that stays live, so the following
// turn has to keep landing in it under its new name.
func TestRenameSessionKeepsTheSessionLive(t *testing.T) {
	provider := &stubProvider{responses: []*llm.ChatResponse{
		textResponse("first answer"),
		textResponse("second answer"),
	}}
	agent, sessionsDir, originalID := newPersistedTestAgent(t, provider)
	const savedID session.ID = "deploy-experiment"

	collectEvents(t, agent, func() {
		agent.Send(context.Background(), "start the experiment")
		if err := agent.RenameSession(savedID); err != nil {
			t.Fatalf("RenameSession: %v", err)
		}
		agent.Send(context.Background(), "and now finish it")
	})

	if got := agent.CurrentSession(); got != savedID {
		t.Errorf("CurrentSession = %q, want %q", got, savedID)
	}
	if _, err := os.Stat(filepath.Join(sessionsDir, string(originalID)+".jsonl")); !os.IsNotExist(err) {
		t.Errorf("the original file is still there (stat err %v), want it moved", err)
	}

	_, msgs, _, err := session.Load(sessionsDir, savedID)
	if err != nil {
		t.Fatalf("load the renamed session: %v", err)
	}
	joined := contentsOf(msgs)
	if !strings.Contains(joined, "start the experiment") {
		t.Error("the renamed transcript lost the turn that preceded the rename")
	}
	if !strings.Contains(joined, "and now finish it") {
		t.Error("the turn after the rename did not land in the renamed file")
	}
}

func TestRenameSessionWithoutAnActiveSessionFails(t *testing.T) {
	agent := newTestAgent(t, nil, &stubProvider{}, nil)
	defer agent.Close()

	if err := agent.RenameSession("anything"); err == nil {
		t.Error("RenameSession succeeded with persistence disabled")
	}
	if id := agent.CurrentSession(); id != "" {
		t.Errorf("CurrentSession = %q, want empty with persistence disabled", id)
	}
}
