package history

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ruvolof/yaaih/internal/llm"
)

func systemMessage(content string) llm.Message {
	return llm.Message{Role: llm.RoleSystem, Content: content}
}

func userMessage(content string) llm.Message {
	return llm.Message{Role: llm.RoleUser, Content: content}
}

// exchange is one assistant message calling a tool plus the reply answering it,
// the pairing every provider rejects a request for breaking.
func exchange(id string, resultBytes int) []llm.Message {
	return []llm.Message{
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: id, Name: "shell_exec", Arguments: `{"command":"ls"}`}}},
		{Role: llm.RoleTool, ToolCallID: id, Name: "shell_exec", Content: strings.Repeat("x", resultBytes)},
	}
}

// batchedExchange is one assistant message calling several tools at once —
// which models do routinely — and the replies answering all of them. Dropping
// it means dropping every one of those messages together.
func batchedExchange(turn, calls, resultBytes int) []llm.Message {
	assistant := llm.Message{Role: llm.RoleAssistant, Content: strings.Repeat("thinking ", turn%5)}
	var replies []llm.Message
	for call := 0; call < calls; call++ {
		id := fmt.Sprintf("call-%d-%d", turn, call)
		assistant.ToolCalls = append(assistant.ToolCalls, llm.ToolCall{
			ID: id, Name: "shell_exec", Arguments: `{"command":"ls"}`,
		})
		replies = append(replies, llm.Message{
			Role: llm.RoleTool, ToolCallID: id, Name: "shell_exec",
			Content: strings.Repeat("x", resultBytes),
		})
	}
	return append([]llm.Message{assistant}, replies...)
}

// conversation builds a system prompt followed by turns of one user message and
// one tool exchange each.
func conversation(turns, resultBytes int) []llm.Message {
	msgs := []llm.Message{systemMessage("you are a test agent")}
	for turn := 0; turn < turns; turn++ {
		msgs = append(msgs, userMessage(fmt.Sprintf("turn %d", turn)))
		msgs = append(msgs, exchange(fmt.Sprintf("call-%d", turn), resultBytes)...)
	}
	return msgs
}

// danglingToolCalls returns the ids of tool calls with no matching reply, and
// the ids of replies with no matching call. Both are fatal to a request.
func danglingToolCalls(msgs []llm.Message) []string {
	answered := make(map[string]bool)
	called := make(map[string]bool)
	for _, msg := range msgs {
		if msg.Role == llm.RoleTool {
			answered[msg.ToolCallID] = true
		}
		for _, call := range msg.ToolCalls {
			called[call.ID] = true
		}
	}
	var dangling []string
	for id := range called {
		if !answered[id] {
			dangling = append(dangling, "unanswered "+id)
		}
	}
	for id := range answered {
		if !called[id] {
			dangling = append(dangling, "orphan reply "+id)
		}
	}
	return dangling
}

func TestWindowLeavesAConversationUnderBudgetAlone(t *testing.T) {
	msgs := conversation(3, 200)
	budget := BudgetFor(100_000)

	windowed, stats := Window(msgs, nil, budget, 0)

	if len(windowed) != len(msgs) {
		t.Fatalf("windowed %d messages, want all %d", len(windowed), len(msgs))
	}
	if stats.DroppedMessages != 0 || stats.ShrunkResults != 0 {
		t.Errorf("stats = %+v, want nothing removed", stats)
	}
	if stats.OverBudget {
		t.Error("a conversation well under budget was reported over it")
	}
}

func TestWindowDisabledSendsEverything(t *testing.T) {
	msgs := conversation(50, 4096)

	windowed, stats := Window(msgs, nil, BudgetFor(0), 0)

	if len(windowed) != len(msgs) {
		t.Fatalf("windowed %d messages, want all %d", len(windowed), len(msgs))
	}
	if stats.DroppedMessages != 0 {
		t.Errorf("dropped %d messages with windowing disabled", stats.DroppedMessages)
	}
}

// The invariant that matters most: a windowed request the provider will accept.
//
// Swept across closely-spaced ceilings and uneven message sizes on purpose. A
// window that drops messages one at a time rather than an exchange at a time
// still lands on an exchange boundary for most ceilings — it only strands a
// reply when the ceiling falls between an assistant message and the result
// answering it, so a handful of round numbers is not enough to catch it.
func TestWindowNeverSeparatesAToolCallFromItsReply(t *testing.T) {
	msgs := []llm.Message{systemMessage("you are a test agent")}
	for turn := 0; turn < 30; turn++ {
		msgs = append(msgs, userMessage(strings.Repeat("question ", 1+turn%7)))
		msgs = append(msgs, batchedExchange(turn, 1+turn%3, 128*(1+turn%9))...)
	}

	trimmedAtSomeCeiling := false
	for ceiling := 200; ceiling <= 8000; ceiling += 37 {
		windowed, stats := Window(msgs, nil, BudgetFor(ceiling), 0)
		if dangling := danglingToolCalls(windowed); len(dangling) != 0 {
			t.Fatalf("ceiling %d: windowed request has %v", ceiling, dangling)
		}
		if windowed[0].Role != llm.RoleSystem {
			t.Fatalf("ceiling %d: request opens with %s, want the system prompt", ceiling, windowed[0].Role)
		}
		trimmedAtSomeCeiling = trimmedAtSomeCeiling || stats.DroppedMessages > 0
	}
	if !trimmedAtSomeCeiling {
		t.Error("no ceiling in the sweep dropped anything, so the sweep proved nothing")
	}
}

func TestWindowKeepsTheSystemPromptAndTheTurnInFlight(t *testing.T) {
	msgs := conversation(30, 2048)
	msgs = append(msgs, userMessage("the question being answered right now"))
	msgs = append(msgs, exchange("call-live", 2048)...)

	windowed, stats := Window(msgs, nil, BudgetFor(1500), 0)

	if windowed[0].Content != "you are a test agent" {
		t.Fatalf("first message = %q, want the system prompt", windowed[0].Content)
	}
	tail := windowed[len(windowed)-3:]
	if tail[0].Content != "the question being answered right now" {
		t.Errorf("the turn in flight lost its user message: %+v", tail[0])
	}
	if tail[2].ToolCallID != "call-live" {
		t.Errorf("the turn in flight lost its tool result: %+v", tail[2])
	}
	if stats.DroppedMessages == 0 {
		t.Error("nothing was dropped from a conversation far over budget")
	}
}

// The model has to be told its memory has a hole, or it answers from a
// conversation it believes it still has.
func TestWindowTellsTheModelWhatItLost(t *testing.T) {
	msgs := conversation(30, 2048)

	windowed, stats := Window(msgs, nil, BudgetFor(2000), 0)

	marker := windowed[1]
	if marker.Role != llm.RoleSystem {
		t.Fatalf("marker role = %s, want system so it is not mistaken for a turn", marker.Role)
	}
	if !strings.Contains(marker.Content, fmt.Sprintf("%d earlier messages", stats.DroppedMessages)) {
		t.Errorf("marker = %q, want it to name the %d dropped messages", marker.Content, stats.DroppedMessages)
	}
}

// The reason Budget has two water marks: between advances the provider must see
// a byte-identical prefix, or prompt caching buys nothing.
func TestWindowHoldsTheRetainedPrefixStillBetweenAdvances(t *testing.T) {
	msgs := conversation(30, 2048)
	budget := BudgetFor(4000)

	first, stats := Window(msgs, nil, budget, 0)
	if stats.DroppedMessages == 0 {
		t.Fatal("the setup never crossed the ceiling")
	}
	prefix := first[:len(first)-1]

	// A further turn arrives. It is far larger than the slack a trim-to-the-
	// ceiling window would have left — that window would have to advance again
	// here, moving the prefix — and still smaller than the headroom the two
	// water marks buy.
	grown := append(append([]llm.Message(nil), msgs...), userMessage(strings.Repeat("more ", 640)))
	second, next := Window(grown, nil, budget, stats.DropThrough)

	if next.DropThrough != stats.DropThrough {
		t.Fatalf("window advanced to %d on a request that still fit (was %d)", next.DropThrough, stats.DropThrough)
	}
	for i, msg := range prefix {
		if second[i].Content != msg.Content || second[i].Role != msg.Role {
			t.Fatalf("prefix message %d changed between requests: %+v vs %+v", i, second[i], msg)
		}
	}
	if next.Advanced(stats.DropThrough) {
		t.Error("Advanced reported a move that did not happen")
	}
}

// One turn can outgrow the budget on its own — twenty-five iterations of tool
// output will do it — and none of it may be dropped. What gives is the detail.
func TestWindowShrinksResultsWhenTheTurnInFlightIsTooLarge(t *testing.T) {
	msgs := []llm.Message{systemMessage("you are a test agent"), userMessage("do the big thing")}
	for i := 0; i < 8; i++ {
		msgs = append(msgs, exchange(fmt.Sprintf("call-%d", i), 8192)...)
	}

	windowed, stats := Window(msgs, nil, BudgetFor(2000), 0)

	if stats.ShrunkResults == 0 {
		t.Fatal("no tool results were shortened for a turn that cannot be windowed")
	}
	if dangling := danglingToolCalls(windowed); len(dangling) != 0 {
		t.Errorf("shortening broke the exchange: %v", dangling)
	}
	if len(windowed) != len(msgs) {
		t.Errorf("windowed %d messages, want all %d — none of this turn may be dropped", len(windowed), len(msgs))
	}
	if stats.AfterTokens >= stats.BeforeTokens {
		t.Errorf("shortening freed nothing: %d → %d tokens", stats.BeforeTokens, stats.AfterTokens)
	}
}

func TestWindowDoesNotModifyTheConversationItIsGiven(t *testing.T) {
	msgs := []llm.Message{systemMessage("you are a test agent"), userMessage("do the big thing")}
	msgs = append(msgs, exchange("call-0", 16384)...)
	original := msgs[3].Content

	if _, stats := Window(msgs, nil, BudgetFor(500), 0); stats.ShrunkResults == 0 {
		t.Fatal("the setup never reached the shortening stage")
	}
	if msgs[3].Content != original {
		t.Errorf("the stored tool result was rewritten: %d bytes left of %d", len(msgs[3].Content), len(original))
	}
}

func TestWindowReportsOverBudgetOnlyWhenNothingIsLeftToRemove(t *testing.T) {
	msgs := conversation(20, 2048)
	if _, stats := Window(msgs, nil, BudgetFor(3000), 0); stats.OverBudget {
		t.Errorf("reported over budget with exchanges still droppable: %+v", stats)
	}

	// A system prompt alone above the ceiling: there is nothing the window is
	// allowed to touch.
	huge := []llm.Message{systemMessage(strings.Repeat("prompt ", 5000))}
	if _, stats := Window(huge, nil, BudgetFor(100), 0); !stats.OverBudget {
		t.Errorf("an unfixable request was not reported over budget: %+v", stats)
	}
}

func TestWindowCountsToolDefinitions(t *testing.T) {
	msgs := conversation(2, 100)
	tools := []llm.ToolDefinition{{
		Name:        "shell_exec",
		Description: strings.Repeat("run a command in the sandbox. ", 40),
		Parameters:  map[string]interface{}{"type": "object"},
	}}

	_, without := Window(msgs, nil, BudgetFor(100_000), 0)
	_, with := Window(msgs, tools, BudgetFor(100_000), 0)

	if with.BeforeTokens <= without.BeforeTokens {
		t.Errorf("tool definitions counted as free: %d vs %d tokens", with.BeforeTokens, without.BeforeTokens)
	}
}

// An agentic turn is one user message followed by many tool calls, so the last
// user message is the second message in the session. A span that stops there
// leaves nothing to summarise at exactly the point where summarising is the
// only way out of a full context.
func TestCompactableSpanCoversASingleLongTurn(t *testing.T) {
	msgs := []llm.Message{systemMessage("you are a test agent"), userMessage("explore the site")}
	for call := 0; call < 6; call++ {
		msgs = append(msgs, batchedExchange(call, 1, 4096)...)
	}
	msgs = append(msgs, llm.Message{Role: llm.RoleAssistant, Content: "here is what I found"})

	from, through := CompactableSpan(msgs)

	if through-from < MinCompactableMessages {
		t.Fatalf("span = [%d,%d) over %d messages, want the turn to be compactable", from, through, len(msgs))
	}
	if from != 1 {
		t.Errorf("span starts at %d, want it to keep the system prompt", from)
	}
	if through != len(msgs)-1 {
		t.Errorf("span ends at %d, want it to keep the final reply (index %d)", through, len(msgs)-1)
	}

	// What is left has to be a conversation a provider will accept.
	kept := append(append([]llm.Message{}, msgs[:from]...), msgs[through:]...)
	if dangling := danglingToolCalls(kept); len(dangling) != 0 {
		t.Errorf("compacting would leave %v", dangling)
	}
}

// Where a whole turn can be kept, it is: the operator reads the last exchange,
// and a summary of it would be worse than the thing itself.
func TestCompactableSpanKeepsTheLastTurnWhenItCan(t *testing.T) {
	msgs := conversation(4, 512)

	from, through := CompactableSpan(msgs)

	if msgs[through].Role != llm.RoleUser {
		t.Errorf("span ends at a %s, want the last user message", msgs[through].Role)
	}
	if from != 1 {
		t.Errorf("span starts at %d, want 1", from)
	}
}

func TestCompactableSpanRefusesATrivialSession(t *testing.T) {
	for _, msgs := range [][]llm.Message{
		{systemMessage("prompt")},
		{systemMessage("prompt"), userMessage("hello")},
	} {
		from, through := CompactableSpan(msgs)
		if through-from >= MinCompactableMessages {
			t.Errorf("span = [%d,%d) over %d messages, want nothing to compact", from, through, len(msgs))
		}
	}
}

// One pass of shortening is not always enough. Abandoning a turn that is
// otherwise going fine is worse than losing the text of an old result.
func TestShrinkingGivesUpResultsEntirelyBeforeGivingUpTheTurn(t *testing.T) {
	msgs := []llm.Message{systemMessage("you are a test agent"), userMessage("do the big thing")}
	for i := 0; i < 12; i++ {
		msgs = append(msgs, exchange(fmt.Sprintf("call-%d", i), 8192)...)
	}

	windowed, stats := Window(msgs, nil, BudgetFor(700), 0)

	if stats.OverBudget {
		t.Errorf("turn abandoned at %d tokens against a 700 ceiling, with results still holding text", stats.AfterTokens)
	}
	if dangling := danglingToolCalls(windowed); len(dangling) != 0 {
		t.Errorf("shortening broke the exchange: %v", dangling)
	}
	kept := 0
	for _, msg := range windowed {
		if msg.Role == llm.RoleTool {
			kept++
		}
	}
	if kept != 12 {
		t.Errorf("%d tool results survived, want all 12 — dropping one strands its call", kept)
	}
}

// The one thing a budget can never buy its way out of.
func TestFixedCostIsReportedSoAnImpossibleBudgetExplainsItself(t *testing.T) {
	msgs := conversation(3, 512)
	tools := []llm.ToolDefinition{{Name: "shell_exec", Description: strings.Repeat("run a command. ", 50)}}

	_, stats := Window(msgs, tools, BudgetFor(50), 0)

	if !stats.OverBudget {
		t.Fatal("a budget below the system prompt and tool definitions was not reported over")
	}
	if stats.FixedTokens < estimateTools(tools) {
		t.Errorf("FixedTokens = %d, want at least the %d the tool definitions cost",
			stats.FixedTokens, estimateTools(tools))
	}
	if stats.FixedTokens >= stats.BeforeTokens {
		t.Errorf("FixedTokens = %d against %d for the whole request", stats.FixedTokens, stats.BeforeTokens)
	}
}
