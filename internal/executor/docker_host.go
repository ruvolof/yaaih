package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/ruvolof/yaaih/internal/logging"
)

// dockerHost runs job processes inside the sandbox container over the Docker
// exec API. Each exec is hijacked so stdin and both output streams share a
// single connection.
type dockerHost struct {
	cli         *client.Client
	containerID string
}

// shellEnv is added to the container's environment for every job, and so
// becomes part of the baseline state.
var shellEnv = []string{
	"TERM=dumb",
	"PAGER=cat",
}

// startJob starts the bootstrap as the exec's own process. runc starts every
// exec'd process as a session leader, so the job shell's pid is also its
// process group, the one a kill is later aimed at.
func (d dockerHost) startJob(ctx context.Context, script string) (*jobProcess, error) {
	execID, err := d.cli.ContainerExecCreate(ctx, d.containerID, container.ExecOptions{
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Tty:          false,
		Cmd:          []string{"bash", "--norc", "--noprofile", "-c", jobBootstrap},
		WorkingDir:   ContainerWorkspace,
		Env:          shellEnv,
	})
	if err != nil {
		return nil, fmt.Errorf("executor: exec create: %w", err)
	}
	hijack, err := d.cli.ContainerExecAttach(ctx, execID.ID, container.ExecAttachOptions{Tty: false})
	if err != nil {
		return nil, fmt.Errorf("executor: exec attach: %w", err)
	}
	if _, err := io.WriteString(hijack.Conn, script); err != nil {
		hijack.Close()
		return nil, fmt.Errorf("executor: send job script: %w", err)
	}
	if err := hijack.CloseWrite(); err != nil {
		hijack.Close()
		return nil, fmt.Errorf("executor: close job stdin: %w", err)
	}

	// Without a TTY the hijacked stream is frame-multiplexed: stdout carries
	// the command's output, stderr the job shell's records.
	outputR, outputW := io.Pipe()
	metaR, metaW := io.Pipe()
	go func() {
		defer logging.Recover("executor.stdcopy")
		_, copyErr := stdcopy.StdCopy(outputW, metaW, hijack.Reader)
		_ = outputW.CloseWithError(copyErr)
		_ = metaW.CloseWithError(copyErr)
	}()

	return &jobProcess{
		output: outputR,
		meta:   metaR,
		close:  hijack.Close,
	}, nil
}

// runHelper runs script as an exec of its own, alongside whatever jobs are
// running.
func (d dockerHost) runHelper(ctx context.Context, script string) (string, error) {
	execID, err := d.cli.ContainerExecCreate(ctx, d.containerID, container.ExecOptions{
		AttachStdout: true,
		AttachStderr: true,
		Cmd:          []string{"bash", "-c", script},
	})
	if err != nil {
		return "", fmt.Errorf("executor: exec create: %w", err)
	}
	hijack, err := d.cli.ContainerExecAttach(ctx, execID.ID, container.ExecAttachOptions{Tty: false})
	if err != nil {
		return "", fmt.Errorf("executor: exec attach: %w", err)
	}
	defer hijack.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = hijack.Conn.SetDeadline(deadline)
	}

	var output bytes.Buffer
	if _, err := stdcopy.StdCopy(&output, &output, hijack.Reader); err != nil {
		return output.String(), fmt.Errorf("executor: read helper output: %w", err)
	}
	return output.String(), nil
}

// startTTY starts cmd on a pseudo-terminal of cols×rows. With a TTY the
// hijacked stream is raw: the terminal's output, not frames.
func (d dockerHost) startTTY(ctx context.Context, cmd, env []string, cols, rows int) (*OperatorShell, error) {
	size := &[2]uint{uint(max(rows, 1)), uint(max(cols, 1))}
	execID, err := d.cli.ContainerExecCreate(ctx, d.containerID, container.ExecOptions{
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Tty:          true,
		ConsoleSize:  size,
		Cmd:          cmd,
		Env:          env,
		WorkingDir:   ContainerWorkspace,
	})
	if err != nil {
		return nil, fmt.Errorf("executor: exec create: %w", err)
	}
	hijack, err := d.cli.ContainerExecAttach(ctx, execID.ID, container.ExecAttachOptions{Tty: true, ConsoleSize: size})
	if err != nil {
		return nil, fmt.Errorf("executor: exec attach: %w", err)
	}
	return &OperatorShell{
		Output: hijack.Reader,
		Input:  hijack.Conn,
		resize: func(cols, rows int) error {
			resizeCtx, cancel := context.WithTimeout(context.Background(), helperTimeout)
			defer cancel()
			return d.cli.ContainerExecResize(resizeCtx, execID.ID, container.ResizeOptions{
				Height: uint(max(rows, 1)),
				Width:  uint(max(cols, 1)),
			})
		},
		close: hijack.Close,
	}, nil
}
