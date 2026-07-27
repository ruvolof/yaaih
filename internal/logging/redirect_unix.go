//go:build !windows

package logging

import (
	"os"
	"syscall"
)

// redirectStderr points fd 2 at f using dup2 so Go's runtime panic dump (which
// writes directly to fd 2) lands in the log file as well. The os.Stderr
// wrapper continues to write to fd 2, so user-space writes follow.
func redirectStderr(f *os.File) error {
	return syscall.Dup2(int(f.Fd()), int(os.Stderr.Fd()))
}
