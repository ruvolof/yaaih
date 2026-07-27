// Package governor tracks cumulative token usage and (when available) cost
// across an agent session. It does not do local tokenisation — usage comes
// straight from the LLM API response, and cost is only reported when the
// provider returns it (e.g. OpenRouter). Otherwise the sidebar shows "N/A".
package governor

import (
	"sync"

	"github.com/ruvolof/aibitat-harness-go/internal/llm"
	"github.com/ruvolof/aibitat-harness-go/internal/logging"
)

var log = logging.For("governor")

// Stats is a snapshot suitable for rendering in the TUI sidebar.
type Stats struct {
	Model             string
	TotalInputTokens  int
	TotalOutputTokens int
	TotalTokens       int
	EstimatedCostUSD  float64
	CostAvailable     bool
}

// Governor accumulates per-turn usage.
type Governor struct {
	mu sync.RWMutex

	model         string
	totalInput    int
	totalOutput   int
	totalCostUSD  float64
	costAvailable bool
}

// New returns an empty Governor for the given model identifier.
func New(model string) *Governor { return &Governor{model: model} }

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
	}
}
