// Skills loaded on demand: the model asks for one through skill_load, the
// operator through LoadSkill, and from then on its instructions are part of
// the system prompt for the rest of the session.
//
// The body goes into the system prompt rather than into the conversation
// because the system prompt is the one message the context window and
// compaction never take away. A skill loaded early in a long session would
// otherwise be silently forgotten half way through the work it was loaded for.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/ruvolof/yaaih/internal/llm"
	"github.com/ruvolof/yaaih/internal/skills"
)

// SkillState is where a skill stands in the current session.
type SkillState string

const (
	SkillAlways    SkillState = "always"    // part of the system prompt from the start
	SkillLoaded    SkillState = "loaded"    // loaded on demand in this session
	SkillAvailable SkillState = "available" // on demand, not loaded yet
)

// SkillStatus describes one of the agent's skills for the operator.
type SkillStatus struct {
	Name        string
	Description string
	State       SkillState
}

// SkillStates lists the agent's skills in the order they appear in the prompt.
func (a *Agent) SkillStates() []SkillStatus {
	loaded := a.currentLoadedSkills()
	statuses := make([]SkillStatus, 0, len(a.cfg.Skills))
	for _, skill := range a.cfg.Skills {
		state := SkillAlways
		if skill.OnDemand() {
			state = SkillAvailable
			if slices.Contains(loaded, skill.Name) {
				state = SkillLoaded
			}
		}
		statuses = append(statuses, SkillStatus{Name: skill.Name, Description: skill.Description, State: state})
	}
	return statuses
}

// LoadSkill loads an on-demand skill at the operator's request. Refused while a
// turn is in flight, since the turn is reading the system prompt it changes.
func (a *Agent) LoadSkill(name string) (string, error) {
	if !a.turnMu.TryLock() {
		return "", errTurnInFlight
	}
	defer a.turnMu.Unlock()
	return a.loadSkill(name, "operator")
}

// loadSkill adds the named on-demand skill to the system prompt and records the
// new prompt in the session file. Loading a skill that is already loaded
// changes nothing. by names who asked, for the log.
func (a *Agent) loadSkill(name, by string) (string, error) {
	skill, found := skills.Find(a.cfg.Skills, name)
	if !found || !skill.OnDemand() {
		return "", fmt.Errorf("no skill %q to load — on-demand skills: %s", name, strings.Join(a.onDemandSkillNames(), ", "))
	}

	a.mu.Lock()
	if slices.Contains(a.loadedSkills, name) {
		a.mu.Unlock()
		return fmt.Sprintf("Skill %q is already loaded; its instructions are in your system prompt.", name), nil
	}
	a.loadedSkills = append(a.loadedSkills, name)
	a.messages[0] = llm.Message{Role: llm.RoleSystem, Content: a.composeSystemPrompt(a.loadedSkills)}
	a.mu.Unlock()

	a.persistSystemPrompt()
	log.Info("skill loaded", "name", name, "by", by)
	return fmt.Sprintf("Loaded skill %q. Its instructions are now part of your system prompt; follow them from here on.", name), nil
}

// systemPrompt is the prompt the conversation opens with right now: the
// agent's own, composed with its skills.
func (a *Agent) systemPrompt() string {
	return a.composeSystemPrompt(a.currentLoadedSkills())
}

func (a *Agent) composeSystemPrompt(loaded []string) string {
	return skills.Compose(a.cfg.SystemPrompt, a.cfg.Skills, loaded)
}

func (a *Agent) currentLoadedSkills() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.loadedSkills)
}

// adoptLoadedSkills keeps the names in recorded that are still on-demand
// skills of this agent. A transcript remembers the skills it had loaded, but a
// skill can be deleted, renamed or made always between runs.
func (a *Agent) adoptLoadedSkills(recorded []string) []string {
	var adopted []string
	for _, name := range recorded {
		skill, found := skills.Find(a.cfg.Skills, name)
		if !found || !skill.OnDemand() {
			log.Warn("skill loaded in the session is no longer an on-demand skill, dropping it", "name", name)
			continue
		}
		if !slices.Contains(adopted, name) {
			adopted = append(adopted, name)
		}
	}
	return adopted
}

func (a *Agent) onDemandSkillNames() []string {
	var names []string
	for _, skill := range a.cfg.Skills {
		if skill.OnDemand() {
			names = append(names, skill.Name)
		}
	}
	return names
}

// skillLoadTool is the model's way of loading a skill. The enum is fixed for
// the life of the agent, so the tool definitions sent with every request do
// not change as skills are loaded.
func (a *Agent) skillLoadTool() *Tool {
	return &Tool{
		Def: llm.ToolDefinition{
			Name: "skill_load",
			Description: "Load one of the skills listed under <available_skills> in your system prompt. " +
				"Its instructions join the system prompt for the rest of the session. " +
				"Load a skill before starting a task that matches its description.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{
						"type": "string",
						"enum": a.onDemandSkillNames(),
					},
				},
				"required": []string{"name"},
			},
		},
		Fn: func(ctx context.Context, args string) (string, error) {
			var params struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal([]byte(args), &params); err != nil {
				return "", fmt.Errorf("invalid args: %w", err)
			}
			return a.loadSkill(params.Name, "model")
		},
	}
}
