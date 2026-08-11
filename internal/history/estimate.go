// Prompt size estimation.
//
// The harness deliberately does not tokenise locally: a real tokeniser is
// model-specific, and getting one wrong is worse than a rough count that knows
// it is rough. What it does instead is count bytes here and correct itself
// against the token count the provider reports for the request that was
// actually sent.
package history

import (
	"encoding/json"

	"github.com/ruvolof/aibitat-harness-go/internal/llm"
)

// Byte-per-token ratios differ by an order of magnitude less than the budgets
// they feed, and the caller's calibration absorbs the rest. Prose sits near
// four bytes per token; JSON and source code are denser.
const bytesPerToken = 4

// Per-item overheads stand in for the role, delimiter, and name fields every
// message carries on the wire regardless of its content.
const (
	messageOverheadTokens        = 4
	toolCallOverheadTokens       = 8
	toolDefinitionOverheadTokens = 8
)

// Estimate approximates the prompt tokens of one request: the conversation plus
// the tool definitions, which are re-sent in full on every iteration and are
// not free once an agent has script tools.
func Estimate(msgs []llm.Message, tools []llm.ToolDefinition) int {
	total := estimateTools(tools)
	for _, msg := range msgs {
		total += EstimateMessage(msg)
	}
	return total
}

// EstimateMessage approximates the prompt tokens of a single message.
func EstimateMessage(msg llm.Message) int {
	tokens := messageOverheadTokens + (len(msg.Content)+len(msg.Name))/bytesPerToken
	for _, call := range msg.ToolCalls {
		tokens += toolCallOverheadTokens + (len(call.Name)+len(call.Arguments))/bytesPerToken
	}
	return tokens
}

func estimateTools(tools []llm.ToolDefinition) int {
	total := 0
	for _, tool := range tools {
		total += toolDefinitionOverheadTokens + (len(tool.Name)+len(tool.Description))/bytesPerToken
		if schema, err := json.Marshal(tool.Parameters); err == nil {
			total += len(schema) / bytesPerToken
		}
	}
	return total
}

// Calibration corrects the estimator against what the provider charged for the
// prompt that was actually sent. One report is enough to remove most of the
// error, and the clamp keeps a single odd response — a cached prefix billed at
// a discount, a provider that counts images — from moving the budget far.
type Calibration struct {
	scalePermille int64
}

const (
	unitScalePermille = 1000
	minScalePermille  = 750
	maxScalePermille  = 2000
)

// Apply scales a raw estimate by everything observed so far.
func (c Calibration) Apply(estimate int) int {
	scale := c.scalePermille
	if scale == 0 {
		scale = unitScalePermille
	}
	return int(int64(estimate) * scale / unitScalePermille)
}

// Observe returns the calibration implied by a request whose raw estimate was
// estimated and which the provider billed as reported prompt tokens. A response
// that carried no usage leaves the calibration untouched.
func (c Calibration) Observe(estimated, reported int) Calibration {
	if estimated <= 0 || reported <= 0 {
		return c
	}
	measured := int64(reported) * unitScalePermille / int64(estimated)
	if measured < minScalePermille {
		measured = minScalePermille
	}
	if measured > maxScalePermille {
		measured = maxScalePermille
	}
	return Calibration{scalePermille: measured}
}

// Bound restates a budget expressed in provider tokens as one expressed in raw
// estimates, so windowing can stay in the units it counts while the operator
// configures the units they are billed in.
func (c Calibration) Bound(budget Budget) Budget {
	scale := c.scalePermille
	if scale == 0 {
		scale = unitScalePermille
	}
	return Budget{
		High: int(int64(budget.High) * unitScalePermille / scale),
		Low:  int(int64(budget.Low) * unitScalePermille / scale),
	}
}

// Scale is the correction factor being applied, for logging.
func (c Calibration) Scale() float64 {
	if c.scalePermille == 0 {
		return 1
	}
	return float64(c.scalePermille) / unitScalePermille
}
