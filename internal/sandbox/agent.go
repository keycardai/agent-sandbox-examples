package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/charmbracelet/x/term"
)

// ErrAgent means a command in the agent container failed, or the agent
// exited.
var ErrAgent = errors.New("agent")

// Shell runs the scenario's run.command in the agent container, or sh when
// the scenario has none, and returns when it exits. The command reads
// l.Stdin and writes to l.Out. If l.Stdin is a terminal, the command gets a
// terminal too.
func (l *Local) Shell(ctx context.Context, target CheckTarget) error {
	scenario, err := l.scenario()
	if err != nil {
		return err
	}
	command := scenario.Run.Command
	if len(command) == 0 {
		command = []string{"sh"}
	}
	flags := "-i"
	if file, ok := l.Stdin.(*os.File); ok && term.IsTerminal(file.Fd()) {
		flags = "-it"
	}
	cmd, err := l.kubectlCommand(ctx, target.Cluster, slices.Concat([]string{
		"--namespace", target.Namespace, "exec", flags, target.Pod, "-c", agentContainer, "--",
	}, command)...)
	if err != nil {
		return err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = l.Stdin, l.Out, l.Out
	if err = cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s exited: %w", ErrAgent, command[0], err)
	}
	return nil
}

// Logs writes the agent container's log to l.Out and follows it until ctx
// ends or the agent exits. If the log ends while the agent runs, Logs follows
// it again from the end. When the agent exits, Logs returns ErrAgent with the
// exit code.
func (l *Local) Logs(ctx context.Context, target CheckTarget) error {
	from := "--tail=-1"
	for {
		cmd, err := l.kubectlCommand(ctx, target.Cluster, "--namespace", target.Namespace,
			"logs", "--follow", from, target.Pod, "-c", agentContainer)
		if err != nil {
			return err
		}
		cmd.Stdout, cmd.Stderr = l.Out, l.Out
		runErr := cmd.Run()
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		out, err := l.kubectl(ctx, target.Cluster, nil, "--namespace", target.Namespace, "get", "pod", target.Pod,
			"-o", `jsonpath={.status.containerStatuses[?(@.name=="agent")].state.terminated.exitCode}`)
		switch code := strings.TrimSpace(string(out)); {
		case err != nil:
			return fmt.Errorf("%w: reading the agent's state: %w", ErrAgent, err)
		case code != "":
			return fmt.Errorf("%w: the agent exited with code %s. Its log is above", ErrAgent, code)
		case runErr != nil:
			return fmt.Errorf("%w: following the agent's log: %w", ErrAgent, runErr)
		}
		from = "--tail=0"
	}
}
