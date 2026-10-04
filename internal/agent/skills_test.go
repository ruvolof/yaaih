package agent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ruvolof/yaaih/internal/config"
	"github.com/ruvolof/yaaih/internal/governor"
	"github.com/ruvolof/yaaih/internal/llm"
	"github.com/ruvolof/yaaih/internal/session"
	"github.com/ruvolof/yaaih/internal/skills"
)

const networkSkillBody = "NETWORK SKILL: always pass -m 5 to curl."

func testSkills() []skills.Skill {
	return []skills.Skill{
		{Name: "network", Description: "debugging network problems", Body: networkSkillBody},
		{Name: "style", Description: "writing code in the house style", Body: "STYLE SKILL: tabs."},
		{Name: "safety", Body: "SAFETY SKILL: never delete backups.", Always: true},
	}
}

// newSkilledTestAgent is a persisted test agent with the given skills, resuming
// seed when it has messages.
func newSkilledTestAgent(t *testing.T, provider llm.Provider, agentSkills []skills.Skill, seed session.Transcript) (*Agent, string, session.ID) {
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
		Skills:       agentSkills,
		Runtime:      config.RuntimeCfg{Workspace: t.TempDir()},
		Capabilities: []string{"shell"},
		Options:      windowedOptions(64_000),
	}
	agent := New(cfg, provider, governor.New(cfg.Model), governor.New(cfg.Model),
		&stubRunner{}, nil, NewGatekeeper(false), writer, seed, sessionsDir)
	return agent, sessionsDir, id
}

func skillState(agent *Agent, name string) SkillState {
	for _, status := range agent.SkillStates() {
		if status.Name == name {
			return status.State
		}
	}
	return ""
}

func TestSkillLoadIsOfferedOnlyWhenThereIsASkillToLoad(t *testing.T) {
	onlyAlways := []skills.Skill{{Name: "safety", Body: "SAFETY", Always: true}}
	withoutCatalog, _, _ := newSkilledTestAgent(t, &stubProvider{}, onlyAlways, session.Transcript{})
	t.Cleanup(withoutCatalog.Close)
	if _, registered := withoutCatalog.Registry().Lookup("skill_load"); registered {
		t.Error("skill_load is offered to an agent with no on-demand skills")
	}

	withCatalog, _, _ := newSkilledTestAgent(t, &stubProvider{}, testSkills(), session.Transcript{})
	t.Cleanup(withCatalog.Close)
	tool, registered := withCatalog.Registry().Lookup("skill_load")
	if !registered {
		t.Fatal("skill_load is not offered to an agent with on-demand skills")
	}
	nameSchema := tool.Def.Parameters["properties"].(map[string]any)["name"].(map[string]any)
	if got := nameSchema["enum"].([]string); !slices.Equal(got, []string{"network", "style"}) {
		t.Errorf("skill_load enum = %v, want the on-demand skills only", got)
	}
}

func TestSystemPromptStartsWithTheCatalogAndEagerSkills(t *testing.T) {
	agent, _, _ := newSkilledTestAgent(t, &stubProvider{}, testSkills(), session.Transcript{})
	t.Cleanup(agent.Close)

	prompt := agent.History()[0].Content
	if !strings.Contains(prompt, `<skill name="network">debugging network problems</skill>`) {
		t.Errorf("the catalog does not list the network skill:\n%s", prompt)
	}
	if strings.Contains(prompt, networkSkillBody) {
		t.Errorf("an on-demand skill's body is in the prompt before it was loaded:\n%s", prompt)
	}
	if !strings.Contains(prompt, "SAFETY SKILL") {
		t.Errorf("an always skill's body is missing from the prompt:\n%s", prompt)
	}
}

// The point of loading from the model's side: the skill it asked for is in the
// very next request of the same turn, and the transcript remembers it.
func TestModelLoadsASkillMidTurn(t *testing.T) {
	provider := &stubProvider{responses: []*llm.ChatResponse{
		toolCallResponse("call-1", "skill_load", `{"name":"network"}`),
		textResponse("curl -m 5 it is"),
	}}
	agent, sessionsDir, id := newSkilledTestAgent(t, provider, testSkills(), session.Transcript{})

	events := collectEvents(t, agent, func() {
		agent.Send(context.Background(), "why is the API timing out?")
	})

	if errs := errorMessages(events); len(errs) != 0 {
		t.Fatalf("turn reported errors %v", errs)
	}
	if strings.Contains(provider.request(0).Messages[0].Content, networkSkillBody) {
		t.Error("the first request already carried the skill")
	}
	secondPrompt := provider.request(1).Messages[0].Content
	if !strings.Contains(secondPrompt, networkSkillBody) {
		t.Errorf("the request after skill_load was sent without the skill:\n%s", secondPrompt)
	}
	if strings.Contains(secondPrompt, `<skill name="network">debugging`) {
		t.Errorf("the loaded skill is still listed as available:\n%s", secondPrompt)
	}
	if skillState(agent, "network") != SkillLoaded {
		t.Errorf("network state = %q, want loaded", skillState(agent, "network"))
	}

	transcript, err := session.LoadTranscript(sessionsDir, id)
	if err != nil {
		t.Fatalf("LoadTranscript: %v", err)
	}
	if !slices.Equal(transcript.Skills, []string{"network"}) {
		t.Errorf("transcript skills = %v, want [network]", transcript.Skills)
	}
	if transcript.Messages[0].Content != secondPrompt {
		t.Error("the transcript does not replay the prompt the model was sent")
	}
}

func TestLoadSkillRejectsWhatCannotBeLoaded(t *testing.T) {
	agent, _, _ := newSkilledTestAgent(t, &stubProvider{}, testSkills(), session.Transcript{})
	t.Cleanup(agent.Close)

	for _, name := range []string{"nonexistent", "safety"} {
		if _, err := agent.LoadSkill(name); err == nil {
			t.Errorf("LoadSkill(%q) succeeded, want an error naming the on-demand skills", name)
		} else if !strings.Contains(err.Error(), "network, style") {
			t.Errorf("LoadSkill(%q) error = %q, want it to list the on-demand skills", name, err)
		}
	}
}

func TestLoadingASkillTwiceRecordsItOnce(t *testing.T) {
	agent, sessionsDir, id := newSkilledTestAgent(t, &stubProvider{}, testSkills(), session.Transcript{})
	collectEvents(t, agent, func() {
		if _, err := agent.LoadSkill("network"); err != nil {
			t.Fatalf("LoadSkill: %v", err)
		}
		result, err := agent.LoadSkill("network")
		if err != nil {
			t.Fatalf("second LoadSkill: %v", err)
		}
		if !strings.Contains(result, "already loaded") {
			t.Errorf("second LoadSkill = %q, want it to say the skill is already loaded", result)
		}
	})
	if got := promptChangesOnDisk(t, sessionsDir, id); got != 1 {
		t.Errorf("transcript records %d prompt changes, want 1", got)
	}
}

func TestLoadSkillIsRefusedDuringATurn(t *testing.T) {
	agent, _, _ := newSkilledTestAgent(t, &stubProvider{}, testSkills(), session.Transcript{})
	t.Cleanup(agent.Close)

	agent.turnMu.Lock()
	_, err := agent.LoadSkill("network")
	agent.turnMu.Unlock()
	if !errors.Is(err, errTurnInFlight) {
		t.Errorf("LoadSkill during a turn = %v, want errTurnInFlight", err)
	}
	if skillState(agent, "network") != SkillAvailable {
		t.Error("a refused load still loaded the skill")
	}
}

func TestNewSessionUnloadsAndResumeRestoresSkills(t *testing.T) {
	agent, sessionsDir, firstID := newSkilledTestAgent(t, &stubProvider{}, testSkills(), session.Transcript{})
	collectEvents(t, agent, func() {
		if _, err := agent.LoadSkill("network"); err != nil {
			t.Fatalf("LoadSkill: %v", err)
		}
		if _, err := agent.NewSession(); err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		if skillState(agent, "network") != SkillAvailable {
			t.Error("a new session kept the previous session's skill")
		}
		if strings.Contains(agent.History()[0].Content, networkSkillBody) {
			t.Error("a new session opens with the previous session's skill in its prompt")
		}

		if _, err := agent.ResumeSession(firstID); err != nil {
			t.Fatalf("ResumeSession: %v", err)
		}
		if skillState(agent, "network") != SkillLoaded {
			t.Error("resuming did not restore the session's skill")
		}
		if !strings.Contains(agent.History()[0].Content, networkSkillBody) {
			t.Error("the resumed session opens without its skill in the prompt")
		}
	})
	if got := promptChangesOnDisk(t, sessionsDir, firstID); got != 1 {
		t.Errorf("transcript records %d prompt changes, want only the load — resuming changed nothing", got)
	}
}

func TestResumeDropsASkillThatIsNoLongerOnDemand(t *testing.T) {
	seed := session.Transcript{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: "an older prompt"},
			{Role: llm.RoleUser, Content: "hello"},
			{Role: llm.RoleAssistant, Content: "hi"},
		},
		Skills: []string{"deleted-since", "safety", "network"},
	}
	agent, _, _ := newSkilledTestAgent(t, &stubProvider{}, testSkills(), seed)
	t.Cleanup(agent.Close)

	if loaded := agent.currentLoadedSkills(); !slices.Equal(loaded, []string{"network"}) {
		t.Errorf("loaded skills = %v, want only the one that is still on demand", loaded)
	}
	if !strings.Contains(agent.History()[0].Content, networkSkillBody) {
		t.Error("the resumed prompt is missing the skill that is still on demand")
	}
}

func TestCompactKeepsTheLoadedSkill(t *testing.T) {
	provider := twoTurnsThenSummary("Deployed staging, metrics fine.")
	agent, _, _ := newSkilledTestAgent(t, provider, testSkills(), session.Transcript{})

	events := collectEvents(t, agent, func() {
		if _, err := agent.LoadSkill("network"); err != nil {
			t.Fatalf("LoadSkill: %v", err)
		}
		agent.Send(context.Background(), "deploy staging")
		agent.Send(context.Background(), "now the metrics")
		agent.Compact(context.Background(), "")
	})

	if errs := errorMessages(events); len(errs) != 0 {
		t.Fatalf("reported errors %v", errs)
	}
	history := agent.History()
	if !strings.Contains(history[1].Content, compactSummaryPrefix) {
		t.Fatalf("the conversation was not compacted: %v", roles(history))
	}
	if !strings.Contains(history[0].Content, networkSkillBody) {
		t.Error("compaction took the loaded skill out of the system prompt")
	}
}
