package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"

	"golang.org/x/term"
)

// ErrAgent means a command in the agent container failed.
var ErrAgent = errors.New("agent")

// Shell runs the scenario's run.command in the agent container, or sh when
// the scenario has none, and returns when it exits. stdin goes to the
// command and its output goes to l.Out. If stdin is a terminal, the command
// gets a terminal too.
func (l *Local) Shell(ctx context.Context, target CheckTarget, stdin io.Reader) error {
	scenario, err := l.scenario()
	if err != nil {
		return err
	}
	command := scenario.Run.Command
	if len(command) == 0 {
		command = []string{"sh"}
	}
	flags := "-i"
	if file, ok := stdin.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		flags = "-it"
	}
	cmd, err := l.kubectlCommand(ctx, target.Cluster, slices.Concat([]string{
		"--namespace", target.Namespace, "exec", flags, target.Pod, "-c", agentContainer, "--",
	}, command)...)
	if err != nil {
		return err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, l.Out, l.Out
	if err = cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s exited: %w", ErrAgent, command[0], err)
	}
	return nil
}
