package tui

import (
	"strings"
	"testing"
)

// The gauge is the operator's warning that a compaction is coming. A stats
// refresh that forgets the context figures would blank it silently.
func TestContextFillIsKeptForTheSidebar(t *testing.T) {
	m := &Model{}
	m.noteContextFill(4000, 64_000)

	if m.contextTokens != 4000 || m.contextBudget != 64_000 {
		t.Errorf("context = %d/%d, want 4000/64000", m.contextTokens, m.contextBudget)
	}
	if !strings.Contains(m.renderSidebar(), "Context") {
		t.Error("the sidebar has no Context section after a stats update")
	}
}

// Meeting the ceiling as a failed turn is a bad way to learn the context was
// filling up.
func TestContextWarningAppearsOnceThenRearms(t *testing.T) {
	m := &Model{}

	m.noteContextFill(4000, 64_000) // 6%
	if body := m.chatBuf.String(); body != "" {
		t.Fatalf("warned at 6%% full: %q", stripANSI(body))
	}

	m.noteContextFill(58_000, 64_000) // 90%
	warning := stripANSI(m.chatBuf.String())
	if !strings.Contains(warning, "90% full") || !strings.Contains(warning, "/compact") {
		t.Fatalf("warning = %q, want the fill level and what to do about it", warning)
	}

	m.noteContextFill(60_000, 64_000) // 93%, still the same filling
	if got := strings.Count(stripANSI(m.chatBuf.String()), "/compact"); got != 1 {
		t.Errorf("warned %d times during one filling, want 1", got)
	}

	// A /compact drops the fill; the next filling deserves its own warning.
	m.noteContextFill(9_000, 64_000)
	m.noteContextFill(59_000, 64_000)
	if got := strings.Count(stripANSI(m.chatBuf.String()), "/compact"); got != 2 {
		t.Errorf("warned %d times across two fillings, want 2", got)
	}
}

func TestContextWarningStaysQuietWithoutABudget(t *testing.T) {
	m := &Model{}
	m.noteContextFill(0, 0)
	m.noteContextFill(5_000, 0)

	if body := m.chatBuf.String(); body != "" {
		t.Errorf("warned with windowing disabled: %q", stripANSI(body))
	}
	if strings.Contains(m.renderSidebar(), "Context") {
		t.Error("the sidebar shows a context gauge with no budget to measure against")
	}
}
