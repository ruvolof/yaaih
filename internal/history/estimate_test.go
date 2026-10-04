package history

import (
	"strings"
	"testing"

	"github.com/ruvolof/yaaih/internal/llm"
)

func TestEstimateGrowsWithTheConversation(t *testing.T) {
	short := []llm.Message{userMessage("hello")}
	long := []llm.Message{userMessage(strings.Repeat("hello ", 1000))}

	if Estimate(short, nil) >= Estimate(long, nil) {
		t.Errorf("a long conversation estimated at %d against a short one at %d",
			Estimate(long, nil), Estimate(short, nil))
	}
	if Estimate(nil, nil) != 0 {
		t.Errorf("an empty request estimated at %d tokens", Estimate(nil, nil))
	}
}

// A tool call costs what its arguments cost. Models write large ones — a whole
// file passed to file_write — and counting only Content would miss all of it.
func TestEstimateCountsToolCallArguments(t *testing.T) {
	bare := llm.Message{Role: llm.RoleAssistant}
	calling := llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
		{ID: "call-1", Name: "file_write", Arguments: strings.Repeat(`{"content":"x"}`, 500)},
	}}

	if EstimateMessage(calling) <= EstimateMessage(bare)+100 {
		t.Errorf("a message carrying a large tool call estimated at %d against %d for an empty one",
			EstimateMessage(calling), EstimateMessage(bare))
	}
}

func TestCalibrationCorrectsTowardsWhatTheProviderCharged(t *testing.T) {
	var calibration Calibration
	if got := calibration.Apply(1000); got != 1000 {
		t.Errorf("an uncalibrated estimate of 1000 became %d, want it left alone", got)
	}

	// The estimator undershot by a quarter.
	calibration = calibration.Observe(1000, 1250)
	if got := calibration.Apply(1000); got != 1250 {
		t.Errorf("calibrated estimate = %d, want 1250", got)
	}

	// A budget expressed in provider tokens has to shrink by the same factor to
	// mean the same thing in estimated ones.
	bounded := calibration.Bound(Budget{High: 1250, Low: 875})
	if bounded.High != 1000 {
		t.Errorf("bounded ceiling = %d, want 1000 estimated tokens", bounded.High)
	}
}

func TestCalibrationIgnoresResponsesItCannotLearnFrom(t *testing.T) {
	calibrated := Calibration{}.Observe(1000, 1250)

	if got := calibrated.Observe(1000, 0); got.Scale() != calibrated.Scale() {
		t.Errorf("a response with no usage moved the scale to %v", got.Scale())
	}
	if got := calibrated.Observe(0, 5000); got.Scale() != calibrated.Scale() {
		t.Errorf("an empty request moved the scale to %v", got.Scale())
	}
}

// One odd response — a prefix billed from cache, a provider counting an image —
// must not be able to move the budget far.
func TestCalibrationIsClamped(t *testing.T) {
	if scale := (Calibration{}).Observe(1000, 40).Scale(); scale < 0.7 {
		t.Errorf("a report far below the estimate moved the scale to %v", scale)
	}
	if scale := (Calibration{}).Observe(1000, 900_000).Scale(); scale > 2.1 {
		t.Errorf("a report far above the estimate moved the scale to %v", scale)
	}
}
