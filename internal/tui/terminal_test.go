package tui

import (
	"bytes"
	"strings"
	"testing"
)

func TestForwardKeysStopsAtTheToggleWithoutPassingItOn(t *testing.T) {
	var shell bytes.Buffer

	err := forwardKeys(strings.NewReader("ls -la\r\x1dnot for the shell"), &shell)

	if err != nil {
		t.Fatalf("forwardKeys: %v", err)
	}
	if shell.String() != "ls -la\r" {
		t.Errorf("shell got %q, want the keys before the toggle", shell.String())
	}
}

func TestForwardKeysEndsWithTheKeyboard(t *testing.T) {
	var shell bytes.Buffer

	if err := forwardKeys(strings.NewReader("echo hi\r"), &shell); err != nil {
		t.Fatalf("forwardKeys: %v", err)
	}
	if shell.String() != "echo hi\r" {
		t.Errorf("shell got %q", shell.String())
	}
}

// Output that arrives while the operator is in the chat waits for them, but
// only the newest of it: a chatty process must not grow it without bound.
func TestBacklogKeepsTheNewestOutputUntilTheOperatorReturns(t *testing.T) {
	terminal := &operatorTerminal{}
	terminal.write(bytes.Repeat([]byte("a"), terminalBacklogBytes))
	terminal.write([]byte("newest"))

	var screen bytes.Buffer
	terminal.attach(&screen)

	if screen.Len() != terminalBacklogBytes || !strings.HasSuffix(screen.String(), "newest") {
		t.Errorf("replayed %d bytes ending %q, want the newest %d", screen.Len(), screen.String()[max(screen.Len()-6, 0):], terminalBacklogBytes)
	}
	terminal.write([]byte("live"))
	if !strings.HasSuffix(screen.String(), "live") {
		t.Error("output after attaching did not reach the screen")
	}
	terminal.detach()
	terminal.write([]byte("away"))
	if strings.HasSuffix(screen.String(), "away") {
		t.Error("output after detaching reached the screen")
	}
	if string(terminal.backlog) != "away" {
		t.Errorf("backlog = %q, want what arrived while away", terminal.backlog)
	}
}
