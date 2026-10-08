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
	// serviceStart bounds the wait for the tunnel's URL and the mirror's
	// listener.
	serviceStart = 90 * time.Second
	// issuerRetry is the pause between checks of a published issuer that
	// does not answer yet. A new quick tunnel's name takes a few seconds to
	// resolve.
	issuerRetry = 5 * time.Second
	pollEvery   = 200 * time.Millisecond
	// sandboxCommand is the command that runs a step by hand.
	sandboxCommand = "go run ./cmd/sandbox"
)

// ErrService is a tunnel or mirror that stopped or did not start.
var ErrService = errors.New("background step")

// UpOptions change what Up does.
type UpOptions struct {
	Run RunOptions
	// Listen is the mirror's address, which the tunnel forwards to.
	Listen string
	// EnvFile is the settings file the printed commands name. When it is
	// empty, the commands do not include --env.
	EnvFile string
	// Print prints the steps as commands and does not run them.
	Print bool
	// IssuerWait bounds how long Up retries a published issuer that does
	// not answer yet.
	IssuerWait time.Duration
	// Answers holds the operator's answers: y or n when the cluster has
	// another issuer, and an empty line to run the checks again.
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
	// quickTunnel is set when Up started a quick tunnel. Its URL stops
	// working when Up stops. bootstrapped is set when bootstrap succeeded.
	quickTunnel, bootstrapped bool
}

// Up runs every step in order in one process: the tunnel and the mirror in
// the background, then bootstrap, check-issuer and run. Before each step it
// prints the command that runs the step by hand. When a step fails or the
// operator stops Up, it prints the commands that are left.
//
// After run, Up keeps the tunnel and the mirror serving the issuer until ctx
// ends. An empty line on opts.Answers runs the checks again. The cluster
// stays when Up stops.
//
// For an existing cluster, Up runs only check-issuer and run, and returns
// after run.
//
// Up does not start a tunnel when KEYCARD_LOCAL_ISSUER is set and
// KEYCARD_LOCAL_TUNNEL is empty, because the operator publishes the issuer.
// It also does not start one while a quick tunnel the operator started is
// running.
func (l *Local) Up(ctx context.Context, opts UpOptions) error {
	steps := l.upSteps(opts)
	if opts.Print {
		l.printSteps(steps, opts.EnvFile)
		return nil
	}
	state := &upRun{opts: opts, answers: bufio.NewReader(opts.Answers), services: make(chan error, len(steps))}
	serviceCtx, stopServices := context.WithCancel(ctx)
	// The tunnel removes its issuer file and stops cloudflared as it
	// returns, so Up waits for both background steps.
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

// upSteps returns Up's steps for the settings.
func (l *Local) upSteps(opts UpOptions) []upStep {
	listen := []string{"--listen", opts.Listen}
	var steps []upStep
	if l.needsTunnel() {
		steps = append(steps, upStep{
			args: append([]string{"tunnel"}, listen...), note: "in a terminal of its own", background: true,
			run: l.upTunnel,
		})
	}
	runArgs := []string{"run"}
	if opts.Run.SkipZoneSetup {
		runArgs = append(runArgs, "--skip-zone-setup")
	}
	if opts.Run.ReplaceActive {
		runArgs = append(runArgs, "--replace-active-policy-set")
	}
	return append(steps,
		upStep{args: []string{"bootstrap"}, run: l.upBootstrap},
		upStep{
			args: append([]string{"mirror"}, listen...), note: "in another terminal", background: true,
			run: l.upMirror,
		},
		upStep{args: []string{"check-issuer"}, run: l.upCheckIssuer},
		upStep{args: runArgs, run: func(ctx context.Context, state *upRun) error {
			err := l.Run(ctx, state.opts.Run)
			if errors.Is(err, ErrCheck) {
				// The sandbox is up. serve reports the failed checks and
				// runs them again on request.
				_, _ = fmt.Fprintf(l.Out, "\n%v\n", err)
				return nil
			}
			return err
		}},
	)
}

// needsTunnel reports whether Up starts a tunnel. Up starts a named tunnel
// when KEYCARD_LOCAL_TUNNEL is set. It starts a quick tunnel when
// KEYCARD_LOCAL_ISSUER is empty and QuickIssuerFile does not exist. If the
// file exists, a quick tunnel the operator started is running, and Up uses
// it.
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

// upBootstrap runs bootstrap. A k3d cluster created with another issuer is
// deleted and created again unless the answer starts with n. With no answer
// at all, as when stdin is empty, the cluster stays.
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
// out. Only a published issuer that does not answer is retried.
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

// waitFor polls ready until it holds, a service stops, ctx ends, or
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

// serve keeps the background steps running until ctx ends or one stops.
// An empty line on the answers runs the checks again.
func (l *Local) serve(ctx context.Context, state *upRun) error {
	_, _ = fmt.Fprintln(l.Out, "\nThe sandbox is up. Leave this running so the zone can read the cluster's issuer\n"+
		"through the tunnel and the mirror.\n"+
		"Press Enter to run the checks again, or Ctrl-C to stop. Stopping does not delete the cluster.")
	lines := make(chan struct{})
	go func() {
		for {
			if _, err := state.answers.ReadString('\n'); err != nil {
				return
			}
			lines <- struct{}{}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-state.services:
			return err
		case <-lines:
			target, err := l.CheckTargetFor("", "", "", "")
			if err == nil {
				err = l.Check(ctx, target)
			}
			if err != nil {
				_, _ = fmt.Fprintf(l.Out, "%v\n", err)
			}
		}
	}
}

func (l *Local) printSteps(steps []upStep, envFile string) {
	for _, step := range steps {
		l.printStep(step, envFile)
	}
}

// printLeft prints the commands that finish the run by hand from steps[from].
// The background steps stopped with Up, so it lists them again.
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
