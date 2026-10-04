package executor

import (
	"context"
	"fmt"
	"io"

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
