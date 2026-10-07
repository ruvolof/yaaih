//go:build !unix

package executor

import (
	"errors"
	"os/exec"
)

func startInOwnProcessGroup(cmd *exec.Cmd) {}

func killLocalProcessGroup(pgid int) error { return errors.ErrUnsupported }
