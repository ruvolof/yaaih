//go:build windows

package logging

import (
	"os"

	"golang.org/x/sys/windows"
)

// redirectStderr swaps os.Stderr and the process-wide STD_ERROR_HANDLE so
// user-space `fmt.Fprintf(os.Stderr, ...)` calls land in the log file.
//
// Caveat: the Go runtime caches the stderr handle at startup and uses it
// directly when dumping panic stacks; we cannot change that cached value, so
// runtime crash dumps from goroutines we do not own may still go to the
// terminal. Panics in our own goroutines are captured by Recover() before the
// runtime sees them.
func redirectStderr(f *os.File) error {
	if err := windows.SetStdHandle(windows.STD_ERROR_HANDLE, windows.Handle(f.Fd())); err != nil {
		return err
	}
	os.Stderr = f
	return nil
}
