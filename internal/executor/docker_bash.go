package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/ruvolof/yaaih/internal/logging"
)

// dockerBash attaches a bash process inside the sandbox container over the
// Docker exec API. The exec instance is hijacked so stdin and stdout share a
// single connection.
type dockerBash struct {
	cli         *client.Client
	containerID string
}

// openBash starts bash as the exec's own process. runc starts every exec'd
// process as a session leader, so bash's pid is also its process group, the one
// killProcessGroup is later handed.
func (d dockerBash) openBash(ctx context.Context) (*bashProcess, error) {
	execID, err := d.cli.ContainerExecCreate(ctx, d.containerID, container.ExecOptions{
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Tty:          false,
		Cmd:          []string{"bash", "--norc", "--noprofile"},
		WorkingDir:   initialCWD,
		Env: []string{
			"TERM=dumb",
			"PAGER=cat",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("executor: exec create: %w", err)
	}
	hijack, err := d.cli.ContainerExecAttach(ctx, execID.ID, container.ExecAttachOptions{Tty: false})
	if err != nil {
		return nil, fmt.Errorf("executor: exec attach: %w", err)
	}

	// Without a TTY the hijacked stream is frame-multiplexed, so stdout and
	// stderr have to be demultiplexed back into the single ordered stream the
	// sentinel protocol reads.
	pr, pw := io.Pipe()
	go func() {
		defer logging.Recover("executor.stdcopy")
		_, copyErr := stdcopy.StdCopy(pw, pw, hijack.Reader)
		_ = pw.CloseWithError(copyErr)
	}()

	return &bashProcess{
		stdin:  hijack.Conn,
		stdout: pr,
		close:  hijack.Close,
	}, nil
}

// killProcessGroup runs the kill as an exec of its own: the abandoned bash is
// still busy with the command that hung, and closing its connection signals no
// process.
func (d dockerBash) killProcessGroup(ctx context.Context, pgid int) error {
	script := fmt.Sprintf(
		"if kill -KILL -- -%[1]d 2>/dev/null || ! kill -0 -- -%[1]d 2>/dev/null; then echo stopped; fi", pgid)
	execID, err := d.cli.ContainerExecCreate(ctx, d.containerID, container.ExecOptions{
		AttachStdout: true,
		AttachStderr: true,
		Cmd:          []string{"bash", "-c", script},
	})
	if err != nil {
		return fmt.Errorf("executor: exec create: %w", err)
	}
	hijack, err := d.cli.ContainerExecAttach(ctx, execID.ID, container.ExecAttachOptions{Tty: false})
	if err != nil {
		return fmt.Errorf("executor: exec attach: %w", err)
	}
	defer hijack.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = hijack.Conn.SetDeadline(deadline)
	}

	var output bytes.Buffer
	if _, err := stdcopy.StdCopy(&output, &output, hijack.Reader); err != nil {
		return fmt.Errorf("executor: read kill result: %w", err)
	}
	if strings.TrimSpace(output.String()) != "stopped" {
		return fmt.Errorf("executor: process group %d is still running", pgid)
	}
	return nil
}
