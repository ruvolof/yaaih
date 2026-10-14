// Spawning: handing the work over to a fresh session focused on one task.
//
// A long session carries everything it has been through, and most of that is
// noise for the next piece of work. Spawning starts that work in a new session
// of the same agent — same tools, workspace and shell — opened with a briefing
// the model writes from the old one, keeping only what the task needs. The old
// session is closed, not discarded: it stays resumable, and the new one records
// which session it came from.
package agent

import (
	"context"
	"fmt"
	"runtime/debug"
	"strings"

	"github.com/ruvolof/yaaih/internal/llm"
	"github.com/ruvolof/yaaih/internal/session"
)

// spawnBriefingPrompt instructs the briefing call. The reader is the agent in
// the new session, which starts with none of this conversation: what the
// briefing leaves out, it will not know.
const spawnBriefingPrompt = `You are preparing a handoff briefing. The conversation below, between a user and an AI agent, is about to be closed, and a fresh session of the same agent will start on the task given at the end. The new session has the same tools, the same workspace, and the same shell (working directory, variables, background jobs), but none of this conversation: the briefing is all it will know about it.

Include only what the task needs:
- what the user is after and the constraints they set, in their own words where the wording matters
- facts established: paths, names, versions, identifiers, commands, error messages, results worth keeping
- decisions taken, and why
- what was tried and did not work, so it is not tried again
- where the work stands now

Leave out everything unrelated to the task. Do not restate the task and do not start on it. Write it as notes addressed to the agent that will pick the work up.`

// spawnBriefingPrefix marks the briefing in the new conversation, so the model
// reads it as a record of where the work comes from rather than as an
// instruction. %s names the session it was written from.
const spawnBriefingPrefix = "[Briefing handed over from %s, which this session was spawned from to work on the task that follows]"

// Spawn closes the current session and starts a new one on task, opened with a
// briefing of the current conversation written for that task, then runs the
// first turn on it. The loaded skills carry over; standing approvals and the
// session's usage do not.
//
// Serialised against turns like Compact. Nothing changes unless the briefing
// arrives intact and the new session file can be created, so a failed or
// stopped spawn leaves the current session exactly as it was.
func (a *Agent) Spawn(ctx context.Context, task string) {
	if !a.turnMu.TryLock() {
		log.Warn("Spawn called while a turn is in progress")
		a.publish(ErrorMsg{Err: "agent is still working on the previous turn — please wait for it to finish"})
		return
	}
	defer a.turnMu.Unlock()

	// The briefing and the new session's first turn occupy the UI as one turn,
	// so they end with a single AssistantDoneMsg on every path out.
	turnEnd := AssistantDoneMsg{}
	defer func() { a.publish(turnEnd) }()

	defer func() {
		if r := recover(); r != nil {
			log.Error("Spawn panic", "value", fmt.Sprintf("%v", r), "stack", string(debug.Stack()))
			a.publish(ErrorMsg{Err: fmt.Sprintf("panic while spawning: %v (see logs/)", r)})
		}
	}()

	if a.refuseOverSpendLimit() {
		return
	}

	parent := a.CurrentSession()
	messages := a.snapshotMessages()
	briefing := ""
	var usage llm.TokenUsage
	if len(messages) > 1 {
		log.Info("writing spawn briefing", "history_msgs", len(messages), "task_len", len(task))
		request := "Conversation to brief from:\n\n" + a.renderSpan(messages[1:]) +
			"\n\nTask for the new session: " + task
		var err error
		briefing, usage, err = a.streamOneOff(ctx, spawnBriefingPrompt, request)
		if err != nil {
			if ctx.Err() != nil {
				log.Info("spawn cancelled by operator")
				turnEnd.Cancelled = true
				return
			}
			log.Error("spawn briefing failed", "err", err)
			a.publish(ErrorMsg{Err: fmt.Sprintf("spawn failed, session unchanged: %v", err)})
			return
		}
		if strings.TrimSpace(briefing) == "" {
			a.publish(ErrorMsg{Err: "the model returned an empty briefing — session unchanged"})
			return
		}
	}

	child, err := a.startSession(parent, task, a.currentLoadedSkills(), "spawned")
	if err != nil {
		log.Error("spawn could not start the new session", "err", err)
		a.publish(ErrorMsg{Err: fmt.Sprintf("spawn failed, session unchanged: %v", err)})
		return
	}
	log.Info("spawned session", "parent", parent, "child", child, "briefing_len", len(briefing))

	// Charged after the switch: the briefing is what the new session was
	// opened with, and its file is what restores the usage on resume.
	if briefing != "" {
		a.chargeUsage(usage)
		a.recordUsage(SpawnBriefingMessage(parent, briefing), sessionUsage(usage))
	}

	a.publish(SessionSpawnedMsg{Parent: parent, Child: child, Briefing: briefing, Task: task})
	a.runTurn(ctx, task, &turnEnd)
}

// SpawnBriefingMessage is the message a spawned session opens with: the
// briefing written for it, under a heading naming parent, the session it was
// spawned from ("" when that session was not persisted).
func SpawnBriefingMessage(parent session.ID, briefing string) llm.Message {
	return llm.Message{Role: llm.RoleSystem, Content: spawnBriefingHeading(parent) + "\n\n" + briefing}
}

// SpawnBriefing returns the briefing message carries when it is the one a
// spawned session opened with, as SpawnBriefingMessage builds it.
func SpawnBriefing(message llm.Message) (string, bool) {
	if message.Role != llm.RoleSystem {
		return "", false
	}
	headingStart, headingEnd, _ := strings.Cut(spawnBriefingPrefix, "%s")
	heading, briefing, found := strings.Cut(message.Content, "\n\n")
	if !found || !strings.HasPrefix(heading, headingStart) || !strings.HasSuffix(heading, headingEnd) {
		return "", false
	}
	return briefing, true
}

func spawnBriefingHeading(parent session.ID) string {
	source := "an earlier session"
	if parent != "" {
		source = "session " + string(parent)
	}
	return fmt.Sprintf(spawnBriefingPrefix, source)
}
