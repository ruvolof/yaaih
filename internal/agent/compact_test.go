package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/ruvolof/yaaih/internal/config"
	"github.com/ruvolof/yaaih/internal/executor"
	"github.com/ruvolof/yaaih/internal/governor"
	"github.com/ruvolof/yaaih/internal/llm"
	"github.com/ruvolof/yaaih/internal/session"
)

// newPersistedTestAgent writes a real transcript, so a test can reopen what a
// compaction left behind the way a resumed session does.
func newPersistedTestAgent(t *testing.T, provider llm.Provider) (*Agent, string, session.ID) {
	t.Helper()
	return newPersistedTestAgentWithRunner(t, provider, &stubRunner{})
}

// newPersistedTestAgentWithRunner is newPersistedTestAgent for a test that has
// to hold the turn open inside a tool call.
func newPersistedTestAgentWithRunner(t *testing.T, provider llm.Provider, runner executor.Executor) (*Agent, string, session.ID) {
	t.Helper()
	sessionsDir := t.TempDir()
	id := session.NewID()
	writer, err := session.NewWriter(sessionsDir, session.Header{ID: id, Model: "stub-model"})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	cfg := &config.Config{
		Model:        "stub-model",
		SystemPrompt: "you are a test agent",
		Runtime:      config.RuntimeCfg{Workspace: t.TempDir()},
		Capabilities: []string{"shell"},
		Options:      windowedOptions(64_000),
	}
	agent := New(cfg, provider, governor.New(cfg.Model), governor.New(cfg.Model),
		runner, nil, NewGatekeeper(false), writer, session.Transcript{}, sessionsDir)
	return agent, sessionsDir, id
}

func twoTurnsThenSummary(summary string) *stubProvider {
	return &stubProvider{responses: []*llm.ChatResponse{
		toolCallResponse("call-1", "shell_exec", `{"command":"deploy"}`),
		textResponse("staging is deployed"),
		toolCallResponse("call-2", "shell_exec", `{"command":"metrics"}`),
		textResponse("metrics look fine"),
		textResponse(summary),
	}}
}

func roles(msgs []llm.Message) []string {
	out := make([]string, len(msgs))
	for i, msg := range msgs {
		out[i] = string(msg.Role)
	}
	return out
}

func TestCompactReplacesTheEarlierConversationAndSurvivesResume(t *testing.T) {
	provider := twoTurnsThenSummary("The user deployed staging and checked metrics; both are fine.")
	agent, sessionsDir, id := newPersistedTestAgent(t, provider)

	events := collectEvents(t, agent, func() {
		agent.Send(context.Background(), "deploy staging")
		agent.Send(context.Background(), "now the metrics")
		agent.Compact(context.Background(), "")
	})

	compacted := agent.History()
	wantRoles := []string{"system", "system", "user", "assistant", "tool", "assistant"}
	if got := roles(compacted); strings.Join(got, ",") != strings.Join(wantRoles, ",") {
		t.Fatalf("compacted history = %v, want the prompt, a summary, and the last turn kept whole", got)
	}
	if !strings.Contains(compacted[1].Content, "deployed staging and checked metrics") {
		t.Errorf("summary message = %q, want the model's summary", compacted[1].Content)
	}
	if !strings.Contains(compacted[1].Content, compactSummaryPrefix) {
		t.Error("the summary is not marked as one, so the model reads it as an instruction")
	}
	if compacted[2].Content != "now the metrics" {
		t.Errorf("message after the summary = %q, want the last turn's question", compacted[2].Content)
	}

	trims := historyTrimmed(events)
	if len(trims) != 1 || !trims[0].Summarised || trims[0].DroppedMessages != 4 {
		t.Errorf("history trimmed events = %+v, want one summarised trim of four messages", trims)
	}
	if errs := errorMessages(events); len(errs) != 0 {
		t.Fatalf("compaction reported errors %v", errs)
	}

	// The agent is closed by collectEvents, so the transcript is complete.
	_, resumed, _, err := session.Load(sessionsDir, id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(resumed) != len(compacted) {
		t.Fatalf("resumed %d messages against %d live: %v", len(resumed), len(compacted), roles(resumed))
	}
	for i := range compacted {
		if resumed[i].Role != compacted[i].Role || resumed[i].Content != compacted[i].Content {
			t.Errorf("resumed message %d = %+v, want %+v", i, resumed[i], compacted[i])
		}
	}
}

// What the model is sent after a compaction is the compacted conversation —
// otherwise the summary would cost a call and save nothing.
func TestTurnAfterCompactionSendsTheSummary(t *testing.T) {
	provider := twoTurnsThenSummary("Staging is deployed and its metrics are fine.")
	provider.responses = append(provider.responses, textResponse("understood"))
	agent, _, _ := newPersistedTestAgent(t, provider)

	collectEvents(t, agent, func() {
		agent.Send(context.Background(), "deploy staging")
		agent.Send(context.Background(), "now the metrics")
		agent.Compact(context.Background(), "")
		agent.Send(context.Background(), "what is left to do?")
	})

	sent := provider.request(5)
	if sent == nil {
		t.Fatal("the turn after the compaction never reached the provider")
	}
	if len(sent.Messages) != 7 {
		t.Fatalf("request carried %d messages, want the compacted conversation: %v", len(sent.Messages), roles(sent.Messages))
	}
	if !strings.Contains(sent.Messages[1].Content, "Staging is deployed") {
		t.Errorf("second message = %q, want the summary", sent.Messages[1].Content)
	}
	for _, msg := range sent.Messages {
		if msg.Content == "deploy staging" {
			t.Error("the compacted-away turn was sent anyway")
		}
	}
	if dangling := danglingToolCalls(sent.Messages); len(dangling) != 0 {
		t.Errorf("the compacted conversation carries %v", dangling)
	}
}

// The operator asked for a summary; a failed call has to leave them with the
// conversation they had, not half of one.
func TestFailedCompactionLeavesTheConversationAlone(t *testing.T) {
	provider := &stubProvider{responses: []*llm.ChatResponse{
		toolCallResponse("call-1", "shell_exec", `{"command":"deploy"}`),
		textResponse("staging is deployed"),
		textResponse("second turn"),
		// No response left for the summarising call.
	}}
	agent, _, _ := newPersistedTestAgent(t, provider)

	var before []llm.Message
	events := collectEvents(t, agent, func() {
		agent.Send(context.Background(), "deploy staging")
		agent.Send(context.Background(), "now the metrics")
		before = agent.History()
		agent.Compact(context.Background(), "")
	})

	after := agent.History()
	if len(after) != len(before) {
		t.Fatalf("a failed compaction left %d messages against %d before it", len(after), len(before))
	}
	errs := errorMessages(events)
	if len(errs) != 1 || !strings.Contains(errs[0], "conversation unchanged") {
		t.Errorf("errors = %v, want one saying the conversation was left alone", errs)
	}
}

func TestCompactRefusesWhenThereIsNothingToSummarise(t *testing.T) {
	provider := &stubProvider{}
	agent, _, _ := newPersistedTestAgent(t, provider)

	events := collectEvents(t, agent, func() {
		agent.Compact(context.Background(), "")
	})

	if provider.request(0) != nil {
		t.Error("a fresh session was sent to the model to be summarised")
	}
	errs := errorMessages(events)
	if len(errs) != 1 || !strings.Contains(errs[0], "nothing to compact") {
		t.Errorf("errors = %v, want one explaining there is nothing to compact", errs)
	}
}

func TestCompactPassesTheOperatorsFocusToTheModel(t *testing.T) {
	provider := twoTurnsThenSummary("summary")
	agent, _, _ := newPersistedTestAgent(t, provider)

	collectEvents(t, agent, func() {
		agent.Send(context.Background(), "deploy staging")
		agent.Send(context.Background(), "now the metrics")
		agent.Compact(context.Background(), "keep every hostname")
	})

	summarising := provider.request(4)
	if summarising == nil {
		t.Fatal("the summarising call never reached the provider")
	}
	if len(summarising.Tools) != 0 {
		t.Errorf("the summarising call offered %d tools; it has nothing to call them for", len(summarising.Tools))
	}
	request := summarising.Messages[len(summarising.Messages)-1].Content
	if !strings.Contains(request, "keep every hostname") {
		t.Error("the operator's focus never reached the model")
	}
	if !strings.Contains(request, "deploy staging") {
		t.Error("the conversation being summarised was not in the request")
	}
}

// The scenario a tight budget produces: one turn browses several pages, fills
// the context on its own, and the operator reaches for /compact. The last user
// message is the second message in the session, so a span that stops there
// would leave nothing to summarise — refusing at the one moment compaction is
// the way out.
func TestCompactAfterASingleLongTurn(t *testing.T) {
	provider := &stubProvider{responses: []*llm.ChatResponse{
		toolCallResponse("call-1", "shell_exec", `{"command":"curl /"}`),
		toolCallResponse("call-2", "shell_exec", `{"command":"curl /about"}`),
		toolCallResponse("call-3", "shell_exec", `{"command":"curl /cs"}`),
		textResponse("the site has three sections"),
		textResponse("The agent read /, /about and /cs; the site has three sections."),
	}}
	agent, sessionsDir, id := newPersistedTestAgent(t, provider)

	events := collectEvents(t, agent, func() {
		agent.Send(context.Background(), "explore the website")
		agent.Compact(context.Background(), "")
	})

	if errs := errorMessages(events); len(errs) != 0 {
		t.Fatalf("compaction after a single long turn reported %v", errs)
	}
	compacted := agent.History()
	wantRoles := []string{"system", "system", "assistant"}
	if got := roles(compacted); strings.Join(got, ",") != strings.Join(wantRoles, ",") {
		t.Fatalf("compacted history = %v, want the prompt, a summary, and the final reply", got)
	}
	if !strings.Contains(compacted[1].Content, "three sections") {
		t.Errorf("summary = %q, want the model's summary of the turn", compacted[1].Content)
	}
	if compacted[2].Content != "the site has three sections" {
		t.Errorf("last message = %q, want the reply the operator just read", compacted[2].Content)
	}
	if dangling := danglingToolCalls(compacted); len(dangling) != 0 {
		t.Errorf("compacting a turn mid-flight left %v", dangling)
	}

	_, resumed, _, err := session.Load(sessionsDir, id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(resumed) != len(compacted) {
		t.Errorf("resumed %d messages against %d live: %v", len(resumed), len(compacted), roles(resumed))
	}
}

// Every iteration of a turn re-reduces the same conversation, so the same
// shortening is arrived at again and again. Announcing each one buries the
// reply under notices about work the operator has already been told about.
func TestUnchangedReductionIsReportedOnce(t *testing.T) {
	agent := newTestAgentWithOptions(t, []string{"shell"}, &stubProvider{}, &stubRunner{}, windowedOptions(600))
	agent.record(llm.Message{Role: llm.RoleUser, Content: "gather the logs"})
	agent.record(llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
		{ID: "call-1", Name: "shell_exec", Arguments: `{"command":"one"}`},
	}})
	agent.record(llm.Message{Role: llm.RoleTool, ToolCallID: "call-1", Name: "shell_exec", Content: bulkOutput(200)})

	var reductions []int
	events := collectEvents(t, agent, func() {
		for iteration := 0; iteration < 3; iteration++ {
			_, stats := agent.requestMessages(nil)
			reductions = append(reductions, stats.ShrunkResults)
		}
	})

	if reductions[0] == 0 {
		t.Fatalf("nothing was shortened for a conversation over its budget: %v", reductions)
	}
	for i, shrunk := range reductions {
		if shrunk != reductions[0] {
			t.Fatalf("iteration %d shortened %d results against %d on the first", i, shrunk, reductions[0])
		}
	}
	if trims := historyTrimmed(events); len(trims) != 1 {
		t.Errorf("three identical requests reported %d trims, want 1: %+v", len(trims), trims)
	}
}

// The sidebar gauge is fed by stats updates. A compaction that left the context
// figures out of its own update would blank the gauge at the one moment the
// operator is looking at it.
func TestCompactRefreshesTheContextGauge(t *testing.T) {
	provider := twoTurnsThenSummary("Staging is deployed and its metrics are fine.")
	agent, _, _ := newPersistedTestAgent(t, provider)

	events := collectEvents(t, agent, func() {
		agent.Send(context.Background(), "deploy staging")
		agent.Send(context.Background(), "now the metrics")
		agent.Compact(context.Background(), "")
	})

	var updates []StatsUpdateMsg
	for _, ev := range events {
		if msg, ok := ev.(StatsUpdateMsg); ok {
			updates = append(updates, msg)
		}
	}
	if len(updates) == 0 {
		t.Fatal("no stats updates were emitted")
	}
	last := updates[len(updates)-1]
	if last.ContextBudget != 64_000 {
		t.Errorf("ContextBudget = %d after compacting, want the configured 64000", last.ContextBudget)
	}
	if last.ContextTokens <= 0 {
		t.Errorf("ContextTokens = %d after compacting, want the compacted conversation's size", last.ContextTokens)
	}

	trims := historyTrimmed(events)
	if len(trims) != 1 {
		t.Fatalf("history trimmed events = %+v, want one for the compaction", trims)
	}
	if trims[0].AfterTokens >= trims[0].BeforeTokens {
		t.Errorf("the conversation grew over a compaction: %d → %d tokens",
			trims[0].BeforeTokens, trims[0].AfterTokens)
	}
	if last.ContextTokens != trims[0].AfterTokens {
		t.Errorf("the gauge reads %d while the compaction reported %d",
			last.ContextTokens, trims[0].AfterTokens)
	}
}
