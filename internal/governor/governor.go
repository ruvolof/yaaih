// Package governor tracks cumulative token usage and (when available) cost
// across an agent session, and holds the limits that session may not spend
// past. It does not do local tokenisation — usage comes straight from the LLM
// API response, and cost is only reported when the provider returns it (e.g.
// OpenRouter). Otherwise the sidebar shows "N/A".
package governor

import (
	"fmt"
	"sync"

	"github.com/ruvolof/yaaih/internal/llm"
	"github.com/ruvolof/yaaih/internal/logging"
)

var log = logging.For("governor")

// Limits caps what a session may spend. A zero field leaves that dimension
// unbounded, so the zero value is no limit at all.
type Limits struct {
	MaxCostUSD float64
	MaxTokens  int
}

// Stats is a snapshot suitable for rendering in the TUI sidebar.
type Stats struct {
	Model             string
	TotalInputTokens  int
	TotalOutputTokens int
	TotalTokens       int
	EstimatedCostUSD  float64
	CostAvailable     bool
	Limits            Limits
}

// Governor accumulates per-turn usage.
type Governor struct {
	mu sync.RWMutex

	model         string
	limits        Limits
	totalInput    int
	totalOutput   int
	totalCostUSD  float64
	costAvailable bool
}

// New returns an empty Governor for the given model identifier.
func New(model string) *Governor { return &Governor{model: model} }

// SetLimits sets the spending limits the session is held to. Reset and Seed
// leave them in place: they belong to the agent, not to one session's totals.
func (g *Governor) SetLimits(limits Limits) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.limits = limits
}

// Record adds one turn's usage to the running totals.
func (g *Governor) Record(usage llm.TokenUsage) {
	g.mu.Lock()
	g.totalInput += usage.InputTokens
	g.totalOutput += usage.OutputTokens
	totalIn := g.totalInput
	totalOut := g.totalOutput
	g.mu.Unlock()
	log.Info("turn usage",
		"tokens_in", usage.InputTokens,
		"tokens_out", usage.OutputTokens,
		"total_in", totalIn,
		"total_out", totalOut,
	)
}

// AddCost accumulates one turn's cost (in USD) into the running session
// total. Called when the provider returns explicit per-turn cost (e.g.
// OpenRouter `total_cost`). Safe to call any number of times; each call adds
// the supplied amount.
func (g *Governor) AddCost(usd float64) {
	g.mu.Lock()
	g.totalCostUSD += usd
	g.costAvailable = true
	running := g.totalCostUSD
	g.mu.Unlock()
	log.Info("turn cost",
		"turn_usd", usd,
		"total_usd", running,
	)
}

// Reset zeroes the running totals, keeping the model. Used when a new session
// starts on the same agent (the session-scoped governor restarts from zero).
func (g *Governor) Reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.totalInput = 0
	g.totalOutput = 0
	g.totalCostUSD = 0
	g.costAvailable = false
}

// Seed sets the running totals to the supplied values. Used when resuming a
// session to restore the usage accumulated by that session in prior runs.
func (g *Governor) Seed(inTokens, outTokens int, costUSD float64, costAvailable bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.totalInput = inTokens
	g.totalOutput = outTokens
	g.totalCostUSD = costUSD
	g.costAvailable = costAvailable
}

// Snapshot returns the current stats.
func (g *Governor) Snapshot() Stats {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return Stats{
		Model:             g.model,
		TotalInputTokens:  g.totalInput,
		TotalOutputTokens: g.totalOutput,
		TotalTokens:       g.totalInput + g.totalOutput,
		EstimatedCostUSD:  g.totalCostUSD,
		CostAvailable:     g.costAvailable,
		Limits:            g.limits,
	}
}

// OverLimit reports the first limit the totals have reached, as an error that
// names the figures. A cost limit is only ever reached by a reported cost: when
// the provider reports none, there is nothing to hold it against.
func (s Stats) OverLimit() error {
	if s.Limits.MaxCostUSD > 0 && s.CostAvailable && s.EstimatedCostUSD >= s.Limits.MaxCostUSD {
		return fmt.Errorf("session cost $%.4f has reached the $%.2f limit", s.EstimatedCostUSD, s.Limits.MaxCostUSD)
	}
	if s.Limits.MaxTokens > 0 && s.TotalTokens >= s.Limits.MaxTokens {
		return fmt.Errorf("session tokens %d have reached the %d limit", s.TotalTokens, s.Limits.MaxTokens)
	}
	return nil
}

// LimitUsed is how far the session has gone towards whichever limit it is
// closest to, as a fraction: 0 with no limits set, 1 or more once one is
// reached.
func (s Stats) LimitUsed() float64 {
	used := 0.0
	if s.Limits.MaxCostUSD > 0 && s.CostAvailable {
		used = max(used, s.EstimatedCostUSD/s.Limits.MaxCostUSD)
	}
	if s.Limits.MaxTokens > 0 {
		used = max(used, float64(s.TotalTokens)/float64(s.Limits.MaxTokens))
	}
	return used
}

// CostLimitUnenforced reports a cost limit that cannot be held: one is set, the
// session has made requests, and none of them came back with a cost.
func (s Stats) CostLimitUnenforced() bool {
	return s.Limits.MaxCostUSD > 0 && !s.CostAvailable && s.TotalTokens > 0
}
