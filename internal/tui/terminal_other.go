//go:build !unix

package tui

import (
	"time"

	"golang.org/x/term"
)

// watchResize calls resize with the terminal's new size each time it changes,
// until stop is closed. Without SIGWINCH the size is polled.
func watchResize(stop <-chan struct{}, fd int, resize func(cols, rows int)) {
	lastCols, lastRows, _ := term.GetSize(fd)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			cols, rows, err := term.GetSize(fd)
			if err != nil || (cols == lastCols && rows == lastRows) {
				continue
			}
			lastCols, lastRows = cols, rows
			resize(cols, rows)
		}
	}
}
