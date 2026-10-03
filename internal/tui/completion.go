package tui

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/ruvolof/aibitat-harness-go/internal/agent"
	"github.com/ruvolof/aibitat-harness-go/internal/config"
	"github.com/ruvolof/aibitat-harness-go/internal/session"
)

// maxListedCompletions caps how many candidates one Tab prints, so a prefix
// that matches every saved session does not bury the conversation.
const maxListedCompletions = 40

// completeInput extends a slash command being typed: the command name, or the
// argument of a command that knows its values. It goes as far as every match
// agrees; when that leaves the text as it was, candidates are the matches, for
// the operator to see what it could become.
func (m *Model) completeInput(text string) (completed string, candidates []string) {
	body, ok := strings.CutPrefix(text, "/")
	if !ok || strings.Contains(body, "\n") {
		return text, nil
	}

	name, argument, hasArgument := strings.Cut(body, " ")
	if !hasArgument {
		extended, matches := extendWord(name, commandNames())
		if len(matches) == 1 {
			return "/" + extended + " ", nil
		}
		if extended != name {
			return "/" + extended, nil
		}
		return text, matches
	}

	argument = strings.TrimLeft(argument, " ")
	command, ok := lookupCommand(name)
	if !ok || command.Complete == nil || strings.Contains(argument, " ") {
		return text, nil
	}
	extended, matches := extendWord(argument, command.Complete(m))
	if extended != argument || len(matches) == 1 {
		return "/" + name + " " + extended, nil
	}
	return text, matches
}

// extendWord returns prefix extended as far as every option starting with it
// agrees, along with those options.
func extendWord(prefix string, options []string) (extended string, matches []string) {
	for _, option := range options {
		if strings.HasPrefix(option, prefix) {
			matches = append(matches, option)
		}
	}
	if len(matches) == 0 {
		return prefix, nil
	}
	extended = matches[0]
	for _, match := range matches[1:] {
		for !strings.HasPrefix(match, extended) {
			_, lastRuneSize := utf8.DecodeLastRuneInString(extended)
			extended = extended[:len(extended)-lastRuneSize]
		}
	}
	return extended, matches
}

// renderCompletions lists the candidates one Tab could not choose between.
func renderCompletions(candidates []string) string {
	listed := candidates
	if len(listed) > maxListedCompletions {
		listed = listed[:maxListedCompletions]
	}
	line := strings.Join(listed, "  ")
	if hidden := len(candidates) - len(listed); hidden > 0 {
		line += fmt.Sprintf("  … and %d more", hidden)
	}
	return styleMuted.Render(line)
}

// commandNames lists every name a command answers to, aliases included.
func commandNames() []string {
	names := make([]string, 0, len(commandTable))
	for name := range commandTable {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func completeAgentNames(m *Model) []string {
	names, err := config.DiscoverAgents(m.agentsDir)
	if err != nil {
		return nil
	}
	return names
}

// completeSessionIDs lists the sessions /resume can reach — every agent's,
// since it switches agent to resume another's — newest first, leaving out the
// one already open.
func completeSessionIDs(m *Model) []string {
	names, err := config.DiscoverAgents(m.agentsDir)
	if err != nil {
		return nil
	}
	if m.ag != nil && !slices.Contains(names, m.currentAgent) {
		names = append(names, m.currentAgent)
	}
	var current session.ID
	if m.ag != nil {
		current = m.ag.CurrentSession()
	}
	var ids []string
	for _, name := range names {
		dir, ok := m.sessionsDirFor(name)
		if !ok {
			continue
		}
		listed, err := session.List(dir)
		if err != nil {
			continue
		}
		for _, id := range listed {
			if id != current {
				ids = append(ids, string(id))
			}
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	return slices.Compact(ids)
}

func completeAvailableSkills(m *Model) []string {
	if m.ag == nil {
		return nil
	}
	var names []string
	for _, status := range m.ag.SkillStates() {
		if status.State == agent.SkillAvailable {
			names = append(names, status.Name)
		}
	}
	return names
}
