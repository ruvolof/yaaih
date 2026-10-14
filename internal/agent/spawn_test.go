package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ruvolof/yaaih/internal/llm"
	"github.com/ruvolof/yaaih/internal/session"
)

func sessionFiles(t *testing.T, sessionsDir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(sessionsDir, "*.jsonl"))
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	return matches
}

func sessionsSpawned(events []Event) []SessionSpawnedMsg {
	var out []SessionSpawnedMsg
	for _, ev := range events {
		if msg, ok := ev.(SessionSpawnedMsg); ok {
			out = append(out, msg)
		}
	}
	return out
}

// cancellingProvider stops the turn on its callIndex-th request, the way an
// operator pressing esc mid-stream does.
type cancellingProvider struct {
	*stubProvider
	callIndex int
	cancel    context.CancelFunc
}

func (p *cancellingProvider) StreamChat(ctx context.Context, req *llm.ChatRequest, cb llm.StreamCallback) (*llm.ChatResponse, error) {
	if p.requestCount() == p.callIndex {
		p.stubProvider.mu.Lock()
		p.stubProvider.requests = append(p.stubProvider.requests, req)
		p.stubProvider.mu.Unlock()
		p.cancel()
		return nil, ctx.Err()
	}
	return p.stubProvider.StreamChat(ctx, req, cb)
}

func TestSpawnHandsABriefingAndTheTaskToANewSession(t *testing.T) {
	provider := twoTurnsThenSummary("Staging is deployed; metrics were checked and look fine.")
	provider.responses = append(provider.responses, textResponse("rollback plan written"))
	agent, sessionsDir, parentID := newPersistedTestAgent(t, provider)

	var parentHistory []llm.Message
	var childID session.ID
	events := collectEvents(t, agent, func() {
		agent.Send(context.Background(), "deploy staging")
		agent.Send(context.Background(), "now the metrics")
		parentHistory = agent.History()
		agent.Spawn(context.Background(), "write a rollback plan")
		childID = agent.CurrentSession()
	})

	if childID == parentID || childID == "" {
		t.Fatalf("current session after spawning = %q, want a new one (parent %q)", childID, parentID)
	}
	if agent.SessionParent() != parentID {
		t.Errorf("SessionParent = %q, want %q", agent.SessionParent(), parentID)
	}

	child := agent.History()
	wantRoles := []string{"system", "system", "user", "assistant"}
	if got := roles(child); strings.Join(got, ",") != strings.Join(wantRoles, ",") {
		t.Fatalf("child history = %v, want the prompt, the briefing, the task and the reply", got)
	}
	if !strings.Contains(child[1].Content, spawnBriefingHeading(parentID)) ||
		!strings.Contains(child[1].Content, "metrics were checked") {
		t.Errorf("briefing message = %q, want the marked briefing", child[1].Content)
	}
	if child[2].Content != "write a rollback plan" {
		t.Errorf("first user message = %q, want the task", child[2].Content)
	}

	briefingRequest := provider.request(4)
	if briefingRequest == nil || len(briefingRequest.Tools) != 0 {
		t.Fatal("the briefing was not written by a tool-less call")
	}
	if !strings.Contains(briefingRequest.Messages[1].Content, "deploy staging") ||
		!strings.Contains(briefingRequest.Messages[1].Content, "Task for the new session: write a rollback plan") {
		t.Errorf("briefing request = %q, want the conversation and the task", briefingRequest.Messages[1].Content)
	}

	spawned := sessionsSpawned(events)
	if len(spawned) != 1 || spawned[0].Parent != parentID || spawned[0].Child != childID || spawned[0].Task != "write a rollback plan" {
		t.Errorf("spawn events = %+v, want one naming both sessions and the task", spawned)
	}
	if done := assistantDone(events); len(done) != 3 {
		t.Errorf("%d turn ends, want one per Send and one for the spawn", len(done))
	}
	if errs := errorMessages(events); len(errs) != 0 {
		t.Fatalf("spawn reported errors %v", errs)
	}

	parent, err := session.LoadTranscript(sessionsDir, parentID)
	if err != nil {
		t.Fatalf("LoadTranscript(parent): %v", err)
	}
	if len(parent.Messages) != len(parentHistory) {
		t.Errorf("parent transcript holds %d messages, want the %d it had before spawning", len(parent.Messages), len(parentHistory))
	}
	parentFile, err := os.ReadFile(filepath.Join(sessionsDir, string(parentID)+".jsonl"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(parentFile), `"reason":"spawned"`) {
		t.Error("the parent session was not closed as spawned")
	}

	resumed, err := session.LoadTranscript(sessionsDir, childID)
	if err != nil {
		t.Fatalf("LoadTranscript(child): %v", err)
	}
	if resumed.Header.Parent != parentID || resumed.Header.Task != "write a rollback plan" {
		t.Errorf("child header = %+v, want its parent and task", resumed.Header)
	}
	if strings.Join(roles(resumed.Messages), ",") != strings.Join(wantRoles, ",") {
		t.Errorf("resumed child = %v, want %v", roles(resumed.Messages), wantRoles)
	}
	if briefing, ok := SpawnBriefing(resumed.Messages[1]); !ok || briefing != "Staging is deployed; metrics were checked and look fine." {
		t.Errorf("SpawnBriefing(resumed briefing) = %q, %v; want the briefing without its heading", briefing, ok)
	}
	if resumed.Usage.InputTokens != 20 {
		t.Errorf("child usage = %d input tokens, want the briefing's and the reply's", resumed.Usage.InputTokens)
	}
}

func TestFailedSpawnLeavesTheSessionAlone(t *testing.T) {
	for _, tc := range []struct {
		name     string
		briefing []*llm.ChatResponse
		wantErr  string
	}{
		{name: "provider error", wantErr: "session unchanged"},
		{name: "empty briefing", briefing: []*llm.ChatResponse{textResponse("  ")}, wantErr: "empty briefing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := &stubProvider{responses: append([]*llm.ChatResponse{textResponse("staging is deployed")}, tc.briefing...)}
			agent, sessionsDir, parentID := newPersistedTestAgent(t, provider)

			var before []llm.Message
			events := collectEvents(t, agent, func() {
				agent.Send(context.Background(), "deploy staging")
				before = agent.History()
				agent.Spawn(context.Background(), "write a rollback plan")
			})

			if agent.CurrentSession() != parentID {
				t.Errorf("current session = %q, want the parent %q kept", agent.CurrentSession(), parentID)
			}
			if after := agent.History(); len(after) != len(before) {
				t.Errorf("a failed spawn left %d messages against %d before it", len(after), len(before))
			}
			if files := sessionFiles(t, sessionsDir); len(files) != 1 {
				t.Errorf("session files = %v, want only the parent's", files)
			}
			errs := errorMessages(events)
			if len(errs) != 1 || !strings.Contains(errs[0], tc.wantErr) {
				t.Errorf("errors = %v, want one containing %q", errs, tc.wantErr)
			}
			if len(sessionsSpawned(events)) != 0 {
				t.Error("a failed spawn announced a new session")
			}
		})
	}
}

func TestCancelledSpawnLeavesTheSessionAlone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	provider := &cancellingProvider{
		stubProvider: &stubProvider{responses: []*llm.ChatResponse{textResponse("staging is deployed")}},
		callIndex:    1,
		cancel:       cancel,
	}
	agent, sessionsDir, parentID := newPersistedTestAgent(t, provider)

	events := collectEvents(t, agent, func() {
		agent.Send(context.Background(), "deploy staging")
		agent.Spawn(ctx, "write a rollback plan")
	})

	if agent.CurrentSession() != parentID {
		t.Errorf("current session = %q, want the parent %q kept", agent.CurrentSession(), parentID)
	}
	if files := sessionFiles(t, sessionsDir); len(files) != 1 {
		t.Errorf("session files = %v, want only the parent's", files)
	}
	done := assistantDone(events)
	if len(done) != 2 || !done[1].Cancelled {
		t.Errorf("turn ends = %+v, want the spawn's marked cancelled", done)
	}
	if errs := errorMessages(events); len(errs) != 0 {
		t.Errorf("a cancelled spawn reported errors %v", errs)
	}
}

func TestSpawnFromAFreshSessionSkipsTheBriefing(t *testing.T) {
	provider := &stubProvider{responses: []*llm.ChatResponse{textResponse("rollback plan written")}}
	agent, _, parentID := newPersistedTestAgent(t, provider)

	events := collectEvents(t, agent, func() {
		agent.Spawn(context.Background(), "write a rollback plan")
	})

	if provider.requestCount() != 1 {
		t.Errorf("provider saw %d requests, want only the task's turn", provider.requestCount())
	}
	if got := roles(agent.History()); strings.Join(got, ",") != "system,user,assistant" {
		t.Errorf("child history = %v, want the prompt, the task and the reply", got)
	}
	spawned := sessionsSpawned(events)
	if len(spawned) != 1 || spawned[0].Briefing != "" || spawned[0].Parent != parentID {
		t.Errorf("spawn events = %+v, want one with no briefing", spawned)
	}
}

func TestSpawnCarriesTheLoadedSkills(t *testing.T) {
	provider := &stubProvider{responses: []*llm.ChatResponse{
		textResponse("network looks fine"),
		textResponse("The user is debugging the network."),
		textResponse("dns fixed"),
	}}
	agent, sessionsDir, _ := newSkilledTestAgent(t, provider, testSkills(), session.Transcript{})

	var childID session.ID
	collectEvents(t, agent, func() {
		if _, err := agent.LoadSkill("network"); err != nil {
			t.Fatalf("LoadSkill: %v", err)
		}
		agent.Send(context.Background(), "check the network")
		agent.Spawn(context.Background(), "fix dns")
		childID = agent.CurrentSession()
	})

	if skillState(agent, "network") != SkillLoaded {
		t.Error("the spawned session lost the parent's skill")
	}
	if !strings.Contains(agent.History()[0].Content, networkSkillBody) {
		t.Error("the spawned session opens without the skill in its prompt")
	}
	resumed, err := session.LoadTranscript(sessionsDir, childID)
	if err != nil {
		t.Fatalf("LoadTranscript: %v", err)
	}
	if len(resumed.Skills) != 1 || resumed.Skills[0] != "network" {
		t.Errorf("resumed child skills = %v, want the inherited one", resumed.Skills)
	}
}

func TestSpawnBriefingRecognisesOnlyTheBriefing(t *testing.T) {
	for _, parent := range []session.ID{"20261015-001541-r1oW", ""} {
		briefing, ok := SpawnBriefing(SpawnBriefingMessage(parent, "notes\n\nmore notes"))
		if !ok || briefing != "notes\n\nmore notes" {
			t.Errorf("parent %q: SpawnBriefing = %q, %v; want the briefing back", parent, briefing, ok)
		}
	}

	notBriefings := []llm.Message{
		{Role: llm.RoleSystem, Content: "you are a test agent"},
		{Role: llm.RoleSystem, Content: compactSummaryPrefix + "\n\nwhat happened"},
		{Role: llm.RoleUser, Content: SpawnBriefingMessage("parent", "notes").Content},
	}
	for _, message := range notBriefings {
		if briefing, ok := SpawnBriefing(message); ok {
			t.Errorf("SpawnBriefing(%s %q) = %q, want it not taken for a briefing", message.Role, message.Content, briefing)
		}
	}
}
