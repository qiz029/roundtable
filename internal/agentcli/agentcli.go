package agentcli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
)

type ExecFunc func(ctx context.Context, command string, stdin []byte) ([]byte, []byte, error)

type Options struct {
	HomeDir string
	Stdout  io.Writer
	Stderr  io.Writer
	Exec    ExecFunc
}

func Run(ctx context.Context, args []string, opts Options) error {
	if len(args) == 0 {
		return errors.New("command is required")
	}
	opts = opts.withDefaults()

	switch args[0] {
	case "login":
		return runLogin(args[1:], opts)
	case "run":
		return runLoop(ctx, args[1:], opts)
	case "invitations":
		return runProxyList(ctx, args[1:], opts, "invitations", "/api/v1/agent/invitations")
	case "questions":
		return runProxyList(ctx, args[1:], opts, "questions", "/api/v1/agent/questions")
	case "answers":
		return runAnswers(ctx, args[1:], opts)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func (o Options) withDefaults() Options {
	if o.Stdout == nil {
		o.Stdout = os.Stdout
	}
	if o.Stderr == nil {
		o.Stderr = os.Stderr
	}
	if o.Exec == nil {
		o.Exec = shellExec
	}
	return o
}
