package sandbox

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// serviceStart is how long Up waits for the tunnel's URL and the mirror's
	// listener.
	serviceStart = 90 * time.Second
	// issuerRetry is the pause between check-issuer attempts. A new quick
	// tunnel's hostname takes a few seconds to resolve.
	issuerRetry = 5 * time.Second
	pollEvery   = 200 * time.Millisecond
	// sandboxCommand is the command that runs a step by hand.
	sandboxCommand = "go run ./cmd/sandbox"
)

// ErrService is a tunnel or mirror that stopped or did not start.
var ErrService = errors.New("background step")

// UpOptions configure Up.
type UpOptions struct {
	Run RunOptions
	// Listen is the mirror's address, which the tunnel forwards to.
	Listen string
	// EnvFile is the --env value in the printed commands. If it is empty,
	// the commands omit --env.
	EnvFile string
	// Print prints the steps as commands and does not run them.
	Print bool
	// IssuerWait is how long Up retries check-issuer.
	IssuerWait time.Duration
	// Answers holds the operator's answers: y or n when the cluster has
	// another issuer, then the lines that serve reads. If Answers is an
	// *os.File, the shell-mode command reads from it too.
	Answers io.Reader
}

// upStep is one step of Up and the command that runs it by hand.
type upStep struct {
	args []string
	// note says where to run the command by hand.
	note string
	// background steps run until Up stops.
	background bool
	run        func(ctx context.Context, state *upRun) error
}

// upRun is the state of one Up.
type upRun struct {
	opts     UpOptions
	answers  *bufio.Reader
	services chan error
	wg       sync.WaitGroup
	// quickTunnel is true when Up started a quick tunnel. Its URL stops
	// working when Up stops. bootstrapped is true when bootstrap succeeded.
	quickTunnel, bootstrapped bool
}

// Up runs every step in one process: tunnel, bootstrap, mirror, check-issuer
// and run. The tunnel and the mirror run in the background. Before each step,
// Up prints the command that runs the step by hand. When a step fails or ctx
// ends, it prints the commands that are left.
//
// After run, Up keeps the tunnel and the mirror serving the issuer and reads
// the operator's answers until ctx ends. Up does not delete the cluster when
// it stops.
//
// For an existing cluster, Up runs only check-issuer and run. It returns
// after run, except in shell mode.
//
// Up does not start a tunnel when the operator publishes the issuer or
// already runs a quick tunnel.
func (l *Local) Up(ctx context.Context, opts UpOptions) error {
	steps := l.upSteps(opts)
	if opts.Print {
		l.printSteps(steps, opts.EnvFile)
		return nil
	}
	state := &upRun{opts: opts, answers: bufio.NewReader(opts.Answers), services: make(chan error, len(steps))}
	serviceCtx, stopServices := context.WithCancel(ctx)
	// Wait for the background steps. The tunnel removes its issuer file and
	// stops cloudflared as it returns.
	defer func() {
		stopServices()
		state.wg.Wait()
	}()
	for i, step := range steps {
		_, _ = fmt.Fprintf(l.Out, "\n$ %s\n", stepCommand(step, opts.EnvFile))
		if err := step.run(serviceCtx, state); err != nil {
			stopServices()
			l.printLeft(steps, i, state)
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
	return l.serve(ctx, state)
}

// servesAfterRun reports whether Up keeps running after the run step. It
// does on a k3d cluster, where it keeps the tunnel and the mirror serving the
// issuer, and for a scenario in any mode other than checks.
func (l *Local) servesAfterRun(scenario *Scenario) bool {
	return !l.Existing() || scenario.Run.Mode != ModeChecks
}

// upSteps returns Up's steps for the settings. An existing cluster
// publishes its own issuer and needs only check-issuer and run.
func (l *Local) upSteps(opts UpOptions) []upStep {
	listen := []string{"--listen", opts.Listen}
	runStep := l.upRunStep(opts)
	if l.Existing() {
		return []upStep{{args: []string{"check-issuer"}, run: l.upCheckIssuer}, runStep}
	}
	var steps []upStep
	if l.needsTunnel() {
		steps = append(steps, upStep{
			args: append([]string{"tunnel"}, listen...), note: "in a terminal of its own", background: true,
			run: l.upTunnel,
		})
	}
	return append(steps,
		upStep{args: []string{"bootstrap"}, run: l.upBootstrap},
		upStep{
			args: append([]string{"mirror"}, listen...), note: "in another terminal", background: true,
			run: l.upMirror,
		},
		upStep{args: []string{"check-issuer"}, run: l.upCheckIssuer},
		runStep,
	)
}

func (l *Local) upRunStep(opts UpOptions) upStep {
	args := []string{"run"}
	if opts.Run.SkipZoneSetup {
		args = append(args, "--skip-zone-setup")
	}
	if opts.Run.ReplaceActive {
		args = append(args, "--replace-active-policy-set")
	}
	return upStep{args: args, run: func(ctx context.Context, state *upRun) error {
		err := l.Run(ctx, state.opts.Run)
		if !errors.Is(err, ErrCheck) {
			return err
		}
		if scenario, scenarioErr := l.scenario(); scenarioErr == nil && l.servesAfterRun(scenario) {
			// A failed check leaves the sandbox up. Print the failure and
			// continue. serve runs the checks again on request.
			_, _ = fmt.Fprintf(l.Out, "\n%v\n", err)
			return nil
		}
		return err
	}}
}

// needsTunnel reports whether Up starts a tunnel. Up starts a named tunnel
// when KEYCARD_LOCAL_TUNNEL is set. It starts a quick tunnel when
// KEYCARD_LOCAL_ISSUER is empty and QuickIssuerFile does not exist. If the
// file exists, the operator's quick tunnel is running and Up uses it.
func (l *Local) needsTunnel() bool {
	if l.Settings[KeyTunnel] != "" {
		return true
	}
	if l.Settings[KeyIssuer] != "" {
		return false
	}
	_, err := os.Stat(l.QuickIssuerFile())
	return err != nil
}

func (l *Local) upTunnel(ctx context.Context, state *upRun) error {
	state.quickTunnel = l.Settings[KeyTunnel] == ""
	l.startService(ctx, state, "tunnel", func(ctx context.Context, service *Local) error {
		return service.Tunnel(ctx, state.opts.Listen)
	})
	if !state.quickTunnel {
		return nil
	}
	return waitFor(ctx, state, "the quick tunnel's URL", func() bool {
		_, err := os.Stat(l.QuickIssuerFile())
		return err == nil
	})
}

// upBootstrap runs bootstrap. If the cluster has another issuer, it asks,
// then deletes and recreates the cluster unless the answer starts with n. At
// EOF without an answer, it keeps the cluster.
func (l *Local) upBootstrap(ctx context.Context, state *upRun) error {
	err := l.Bootstrap(ctx, "", "", false)
	if !errors.Is(err, ErrOtherIssuer) {
		state.bootstrapped = err == nil
		return err
	}
	_, _ = fmt.Fprintf(l.Out, "%v\n\nDelete the cluster and create it again with the new issuer? [Y/n] ", err)
	answer, readErr := state.answers.ReadString('\n')
	declined := strings.HasPrefix(strings.ToLower(strings.TrimSpace(answer)), "n")
	if declined || (answer == "" && readErr != nil) {
		return fmt.Errorf("%w: the cluster has another issuer", ErrConfirm)
	}
	if err = l.Down(ctx, "", true, nil); err != nil {
		return err
	}
	err = l.Bootstrap(ctx, "", "", false)
	state.bootstrapped = err == nil
	return err
}

func (l *Local) upMirror(ctx context.Context, state *upRun) error {
	l.startService(ctx, state, "mirror", func(ctx context.Context, service *Local) error {
		return service.Mirror(ctx, "", "", state.opts.Listen)
	})
	return waitFor(ctx, state, "the mirror on "+state.opts.Listen, func() bool {
		conn, err := (&net.Dialer{Timeout: pollEvery}).DialContext(ctx, "tcp", state.opts.Listen)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	})
}

// upCheckIssuer runs check-issuer until it passes or opts.IssuerWait runs
// out. It retries only errors that wrap ErrIssuer.
func (l *Local) upCheckIssuer(ctx context.Context, state *upRun) error {
	deadline := time.Now().Add(state.opts.IssuerWait)
	for {
		err := l.CheckIssuer(ctx, "", "")
		if err == nil || !errors.Is(err, ErrIssuer) || time.Now().Add(issuerRetry).After(deadline) {
			return err
		}
		_, _ = fmt.Fprintf(l.Out, "The issuer does not answer yet. Retrying in %s.\n", issuerRetry)
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for the issuer: %w", ctx.Err())
		case err = <-state.services:
			return err
		case <-time.After(issuerRetry):
		}
	}
}

// startService runs a background step until ctx ends. Its output goes to
// local/out/<name>.log. If the step stops before ctx ends, its error goes to
// state.services.
func (l *Local) startService(ctx context.Context, state *upRun, name string, run func(context.Context, *Local) error) {
	logPath := filepath.Join(l.outDir(), name+".log")
	_, _ = fmt.Fprintf(l.Out, "This step runs in the background. Its log is %s.\n", logPath)
	state.wg.Go(func() {
		log, err := openLog(logPath)
		if err != nil {
			state.services <- err
			return
		}
		defer func() { _ = log.Close() }()
		service := *l
		service.Out = log
		err = run(ctx, &service)
		if ctx.Err() == nil {
			state.services <- fmt.Errorf("%w: %s stopped: %w. Its log is %s", ErrService, name, err, logPath)
		}
	})
}

func openLog(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
		return nil, fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	//nolint:gosec // G304: the path is under this checkout's local/out.
	log, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fileMode)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	return log, nil
}

// waitFor polls ready until it returns true, a service stops, ctx ends, or
// serviceStart runs out.
func waitFor(ctx context.Context, state *upRun, what string, ready func() bool) error {
	timeout := time.After(serviceStart)
	for !ready() {
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %s: %w", what, ctx.Err())
		case err := <-state.services:
			return err
		case <-timeout:
			return fmt.Errorf("%w: %s did not start within %s", ErrService, what, serviceStart)
		case <-time.After(pollEvery):
		}
	}
	return nil
}

// serve reads the operator's answers after the run step, until ctx ends or
// a background step stops. In shell mode, it opens the scenario's command at
// once. After the command exits, an empty line opens it again and any other
// line runs the checks. In checks mode, every line runs the checks. When the
// answers end, serve waits in idle.
func (l *Local) serve(ctx context.Context, state *upRun) error {
	scenario, err := l.scenario()
	if err != nil {
		return err
	}
	if !l.servesAfterRun(scenario) {
		return nil
	}
	shell := scenario.Run.Mode == ModeShell
	if !l.Existing() {
		_, _ = fmt.Fprintln(l.Out, "\nThe sandbox is up. Leave this running so the zone can read the cluster's issuer\n"+
			"through the tunnel and the mirror. Stopping does not delete the cluster.")
	}
	prompt := servePrompt(scenario)
	stdin := commandInput(state.opts.Answers)
	for open := shell; ; {
		if open {
			_, _ = fmt.Fprintf(l.Out, "\n$ %s\n", stepCommand(upStep{args: []string{"shell"}}, state.opts.EnvFile))
			l.report(l.inSandbox(ctx, func(target CheckTarget) error { return l.Shell(ctx, target, stdin) }))
		}
		_, _ = fmt.Fprintln(l.Out, "\n"+prompt)
		line, ok := readLine(ctx, state)
		if !ok {
			return l.idle(ctx, state)
		}
		open = shell && line == ""
		if !open {
			l.report(l.inSandbox(ctx, func(target CheckTarget) error { return l.Check(ctx, target) }))
		}
	}
}

// servePrompt says what serve does with each answer.
func servePrompt(scenario *Scenario) string {
	if scenario.Run.Mode == ModeShell {
		return "Press Enter to open " + scenario.Run.Command[0] + " again, type c and Enter to run the checks, " +
			"or press Ctrl-C to stop."
	}
	return "Press Enter to run the checks again, or Ctrl-C to stop."
}

// commandInput returns answers if it is an *os.File, such as the terminal,
// and nil otherwise. A nil stdin gives the command empty input. exec copies
// any other reader to the command until EOF, which would consume the answers
// meant for up.
func commandInput(answers io.Reader) io.Reader {
	if file, ok := answers.(*os.File); ok {
		return file
	}
	return nil
}

// readLine returns the next answer, trimmed. It returns false when ctx
// ends, a background step stops, or the answers end. The error of a stopped
// background step goes back on state.services, so idle returns it.
func readLine(ctx context.Context, state *upRun) (string, bool) {
	lines := make(chan string, 1)
	go func() {
		line, err := state.answers.ReadString('\n')
		if err != nil && line == "" {
			close(lines)
			return
		}
		lines <- strings.TrimSpace(line)
	}()
	select {
	case <-ctx.Done():
		return "", false
	case err := <-state.services:
		state.services <- err
		return "", false
	case line, ok := <-lines:
		return line, ok
	}
}

// idle waits until ctx ends or a background step stops. For an existing
// cluster, which has no background steps, idle returns at once.
func (l *Local) idle(ctx context.Context, state *upRun) error {
	if l.Existing() {
		return nil
	}
	select {
	case <-ctx.Done():
		return nil
	case err := <-state.services:
		return err
	}
}

// inSandbox runs do against the sandbox of the last run.
func (l *Local) inSandbox(ctx context.Context, do func(CheckTarget) error) error {
	target, err := l.CheckTargetFor("", "", "", "")
	if err != nil {
		return err
	}
	if err = do(target); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

// report prints err, if any.
func (l *Local) report(err error) {
	if err != nil {
		_, _ = fmt.Fprintf(l.Out, "%v\n", err)
	}
}

func (l *Local) printSteps(steps []upStep, envFile string) {
	for _, step := range steps {
		l.printStep(step, envFile)
	}
}

// printLeft prints the commands for steps[from:] and for every background
// step, since those stopped with Up.
func (l *Local) printLeft(steps []upStep, from int, state *upRun) {
	_, _ = fmt.Fprintln(l.Out, "\nUp stopped. To finish by hand, run these commands from the repository root:")
	for i, step := range steps {
		if i >= from || step.background {
			l.printStep(step, state.opts.EnvFile)
		}
	}
	if state.quickTunnel && state.bootstrapped {
		_, _ = fmt.Fprintf(l.Out, "\nThe cluster's issuer is the quick tunnel's URL, which stopped working with Up.\n"+
			"Start the tunnel, then run %s down and %s bootstrap\n"+
			"to create the cluster again with the new URL.\n", sandboxCommand, sandboxCommand)
	}
}

func (l *Local) printStep(step upStep, envFile string) {
	line := stepCommand(step, envFile)
	if step.note != "" {
		line += "    # " + step.note
	}
	_, _ = fmt.Fprintln(l.Out, "  "+line)
}

func stepCommand(step upStep, envFile string) string {
	args := step.args
	if envFile != "" {
		args = append([]string{"--env", envFile}, args...)
	}
	return sandboxCommand + " " + strings.Join(args, " ")
}
