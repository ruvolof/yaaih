package governor

import (
	"sync"
	"testing"

	"github.com/ruvolof/aibitat-harness-go/internal/llm"
)

func TestRecordAccumulatesAcrossTurns(t *testing.T) {
	gov := New("stub-model")

	gov.Record(llm.TokenUsage{InputTokens: 100, OutputTokens: 20})
	gov.Record(llm.TokenUsage{InputTokens: 250, OutputTokens: 35})

	stats := gov.Snapshot()
	if stats.Model != "stub-model" {
		t.Errorf("Model = %q, want stub-model", stats.Model)
	}
	if stats.TotalInputTokens != 350 || stats.TotalOutputTokens != 55 {
		t.Errorf("tokens = %d in / %d out, want 350 / 55", stats.TotalInputTokens, stats.TotalOutputTokens)
	}
	if stats.TotalTokens != 405 {
		t.Errorf("TotalTokens = %d, want 405", stats.TotalTokens)
	}
}

// Cost only exists when the provider reports it. A provider that does not must
// leave the flag clear, so the sidebar shows "N/A" instead of a confident $0.00.
func TestCostIsUnavailableUntilOneIsRecorded(t *testing.T) {
	gov := New("stub-model")

	gov.Record(llm.TokenUsage{InputTokens: 100, OutputTokens: 20})
	if stats := gov.Snapshot(); stats.CostAvailable {
		t.Errorf("CostAvailable = true with cost %v, want it unavailable", stats.EstimatedCostUSD)
	}

	gov.AddCost(0.0004)
	gov.AddCost(0.0006)

	stats := gov.Snapshot()
	if !stats.CostAvailable {
		t.Error("CostAvailable = false after two costs were recorded")
	}
	if stats.EstimatedCostUSD != 0.001 {
		t.Errorf("EstimatedCostUSD = %v, want 0.001", stats.EstimatedCostUSD)
	}
}

// /new restarts the session-scoped governor while the run total carries on, so
// Reset has to clear the cost flag along with the counters — otherwise a fresh
// session opens claiming a cost of zero rather than an unknown one.
func TestResetClearsEverythingButTheModel(t *testing.T) {
	gov := New("stub-model")
	gov.Record(llm.TokenUsage{InputTokens: 100, OutputTokens: 20})
	gov.AddCost(0.5)

	gov.Reset()

	stats := gov.Snapshot()
	if stats.TotalTokens != 0 || stats.EstimatedCostUSD != 0 {
		t.Errorf("after Reset: %+v, want zeroed totals", stats)
	}
	if stats.CostAvailable {
		t.Error("CostAvailable survived a Reset")
	}
	if stats.Model != "stub-model" {
		t.Errorf("Model = %q, want it kept across a Reset", stats.Model)
	}
}

// Resuming restores what a session spent in earlier runs, so the sidebar picks
// up where it left off rather than restarting the count.
func TestSeedRestoresAResumedSession(t *testing.T) {
	gov := New("stub-model")
	gov.Record(llm.TokenUsage{InputTokens: 999, OutputTokens: 999})

	gov.Seed(1200, 340, 0.021, true)

	stats := gov.Snapshot()
	if stats.TotalInputTokens != 1200 || stats.TotalOutputTokens != 340 {
		t.Errorf("tokens = %d / %d, want the seeded 1200 / 340", stats.TotalInputTokens, stats.TotalOutputTokens)
	}
	if !stats.CostAvailable || stats.EstimatedCostUSD != 0.021 {
		t.Errorf("cost = %v (available %v), want 0.021", stats.EstimatedCostUSD, stats.CostAvailable)
	}

	gov.Record(llm.TokenUsage{InputTokens: 100, OutputTokens: 10})
	if stats := gov.Snapshot(); stats.TotalInputTokens != 1300 {
		t.Errorf("TotalInputTokens = %d, want the next turn added to the seed", stats.TotalInputTokens)
	}
}

// A resumed session seeded with no cost must not claim one.
func TestSeedWithoutCostLeavesItUnavailable(t *testing.T) {
	gov := New("stub-model")
	gov.Seed(10, 5, 0, false)

	if stats := gov.Snapshot(); stats.CostAvailable {
		t.Error("CostAvailable = true after seeding a session that never reported a cost")
	}
}

// The turn loop records usage while the TUI renders snapshots, on different
// goroutines. Meaningful under -race.
func TestConcurrentRecordAndSnapshot(t *testing.T) {
	gov := New("stub-model")
	const turns = 200

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < turns; i++ {
			gov.Record(llm.TokenUsage{InputTokens: 1, OutputTokens: 1})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < turns; i++ {
			gov.AddCost(0.001)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < turns; i++ {
			gov.Snapshot()
		}
	}()
	wg.Wait()

	if stats := gov.Snapshot(); stats.TotalTokens != 2*turns {
		t.Errorf("TotalTokens = %d, want %d — a lost update means the mutex is not covering it", stats.TotalTokens, 2*turns)
	}
}
