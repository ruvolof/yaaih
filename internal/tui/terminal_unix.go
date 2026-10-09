//go:build unix

package tui

import (
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/term"
)

// watchResize calls resize with the terminal's new size each time it changes,
// until stop is closed.
func watchResize(stop <-chan struct{}, fd int, resize func(cols, rows int)) {
	changed := make(chan os.Signal, 1)
	signal.Notify(changed, syscall.SIGWINCH)
	defer signal.Stop(changed)
	for {
		select {
		case <-stop:
			return
		case <-changed:
			if cols, rows, err := term.GetSize(fd); err == nil {
				resize(cols, rows)
			}
		}
	}
}
