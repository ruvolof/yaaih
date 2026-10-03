package tui

import "slices"

// inputHistoryLimit is how many submitted inputs ↑ can reach back through.
const inputHistoryLimit = 200

// InputHistory is what the operator has submitted, oldest first, for recall
// with ↑ and ↓. It lives outside Model because switching agents builds a new
// Model, and what was typed to the last agent is often what the next one needs.
type InputHistory struct {
	entries []string
	// recalled indexes the entry on display while walking the history, and is
	// len(entries) when not walking it.
	recalled int
}

// NewInputHistory returns an empty history.
func NewInputHistory() *InputHistory {
	return &InputHistory{}
}

// record adds a submitted input and stops any walk in progress. Repeating the
// previous entry does not add it again, so ↑ never shows the same input twice
// in a row.
func (history *InputHistory) record(text string) {
	if text != "" && (len(history.entries) == 0 || history.entries[len(history.entries)-1] != text) {
		history.entries = append(history.entries, text)
		if excess := len(history.entries) - inputHistoryLimit; excess > 0 {
			history.entries = slices.Delete(history.entries, 0, excess)
		}
	}
	history.recalled = len(history.entries)
}

// previous returns the entry before the one on display, given what the input
// holds now. Walking starts from an empty input or continues over an entry left
// as recalled; anything the operator has typed or edited is theirs, and ok is
// false so ↑ goes on to move the cursor instead.
func (history *InputHistory) previous(current string) (entry string, ok bool) {
	switch {
	case current == "":
		history.recalled = len(history.entries)
	case !history.showing(current):
		return "", false
	}
	if history.recalled == 0 {
		return "", false
	}
	history.recalled--
	return history.entries[history.recalled], true
}

// next returns the entry after the one on display, or an empty input once the
// walk passes the newest entry. ok is false when the input does not hold an
// entry left as recalled.
func (history *InputHistory) next(current string) (entry string, ok bool) {
	if !history.showing(current) {
		return "", false
	}
	history.recalled++
	if history.recalled == len(history.entries) {
		return "", true
	}
	return history.entries[history.recalled], true
}

func (history *InputHistory) showing(current string) bool {
	return history.recalled < len(history.entries) && history.entries[history.recalled] == current
}
