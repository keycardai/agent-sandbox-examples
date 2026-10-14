package sandbox

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/keycardai/agent-sandbox-examples/internal/issuer"
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
	services chan error
	wg       sync.WaitGroup
	// quickTunnel is true when Up started a quick tunnel. Its URL stops
	// working when Up stops. bootstrapped is true when bootstrap succeeded.
	quickTunnel, bootstrapped bool
	// checks is the error from the last checks that finished, or nil if they
	// passed.
	checks error
}

// Up runs every step in one process: tunnel, bootstrap, mirror, check-issuer
// and run. The tunnel and the mirror run in the background. Before each step,
// Up prints the command that runs the step by hand. When a step fails or ctx
// ends, it prints the commands that are left and returns an error.
//
// After run, Up keeps the tunnel and the mirror running until ctx ends. On a
// terminal, it asks whether to run the checks again, and stops on no. Failed
// checks do not stop Up, but when Up stops it returns the last checks' error.
// Up does not delete the cluster when it stops.
//
// Up does not start a tunnel when the operator publishes the issuer or
// already runs a quick tunnel.
func (l *Local) Up(ctx context.Context, opts UpOptions) error {
	steps := l.upSteps(opts)
	if opts.Print {
		l.printSteps(steps, opts.EnvFile)
		return nil
	}
	state := &upRun{opts: opts, services: make(chan error, len(steps))}
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
				return fmt.Errorf("interrupted: %w", ctx.Err())
			}
			return err
		}
	}
	return l.serve(ctx, steps, state)
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
				_, _ = fmt.Fprintf(l.Out, "\n%v\n", err)
				state.checks = err
				return nil
			}
			return err
		}},
	)
}

// needsTunnel reports whether Up starts a tunnel. Up starts a named tunnel
// when KEYCARD_LOCAL_TUNNEL is set. It starts a quick tunnel when
// KEYCARD_LOCAL_ISSUER is empty and no running quick tunnel has
// QuickIssuerFile locked. Otherwise Up uses the running tunnel.
func (l *Local) needsTunnel() bool {
	if l.Settings[KeyTunnel] != "" {
		return true
	}
	if l.Settings[KeyIssuer] != "" {
		return false
	}
	_, running, err := readQuickIssuer(l.QuickIssuerFile())
	return err != nil || !running
}

func (l *Local) upTunnel(ctx context.Context, state *upRun) error {
	if err := l.checkTunnel(state.opts.Listen); err != nil {
		return err
	}
	state.quickTunnel = l.Settings[KeyTunnel] == ""
	l.startService(ctx, state, "tunnel", func(ctx context.Context, service *Local) error {
		return service.Tunnel(ctx, state.opts.Listen)
	})
	if !state.quickTunnel {
		return nil
	}
	return waitFor(ctx, state, "the quick tunnel's URL", func() bool {
		_, running, err := readQuickIssuer(l.QuickIssuerFile())
		return err == nil && running
	})
}

// upBootstrap runs bootstrap. If the cluster has another issuer, it runs down,
// which asks before it deletes the cluster. After a delete, it runs bootstrap
// again.
func (l *Local) upBootstrap(ctx context.Context, state *upRun) error {
	err := l.Bootstrap(ctx, "", "", false)
	if errors.Is(err, ErrOtherIssuer) {
		_, _ = fmt.Fprintf(l.Out, "%v\n\n", err)
		if err = l.Down(ctx, "", false); err != nil {
			return err
		}
		err = l.Bootstrap(ctx, "", "", false)
	}
	state.bootstrapped = err == nil
	return err
}

// upMirror listens on opts.Listen before it starts the mirror, so the step
// fails when another process has the port.
func (l *Local) upMirror(ctx context.Context, state *upRun) error {
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", state.opts.Listen)
	if err != nil {
		return fmt.Errorf("%w: mirror: %w", ErrService, err)
	}
	l.startService(ctx, state, "mirror", func(ctx context.Context, service *Local) error {
		return service.mirror(ctx, "", "", listener)
	})
	// The listener accepts connections before the mirror serves, so wait for
	// an HTTP response with any status.
	client := &http.Client{Transport: &http.Transport{}, Timeout: pollEvery}
	return waitFor(ctx, state, "the mirror on "+state.opts.Listen, func() bool {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+state.opts.Listen+"/", nil)
		if err != nil {
			return false
		}
		resp, err := client.Do(req)
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return true
	})
}

// upCheckIssuer runs check-issuer until it passes or opts.IssuerWait runs
// out. It retries only a failed discovery fetch, the error a tunnel gives
// while it starts. It returns any other error at once.
func (l *Local) upCheckIssuer(ctx context.Context, state *upRun) error {
	deadline := time.Now().Add(state.opts.IssuerWait)
	for {
		err := l.CheckIssuer(ctx, "", "")
		if err == nil || !errors.Is(err, issuer.ErrFetch) || time.Now().Add(issuerRetry).After(deadline) {
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
		switch {
		case ctx.Err() != nil:
		case err == nil:
			state.services <- fmt.Errorf("%w: %s stopped. Its log is %s", ErrService, name, logPath)
		default:
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

// serve keeps the background steps running until ctx ends or one stops. On a
// terminal, it asks whether to run the checks again until the operator
// answers no or presses Ctrl-C. Without a terminal, it does not ask. When the
// operator stops it, it returns state.checks.
func (l *Local) serve(ctx context.Context, steps []upStep, state *upRun) error {
	_, _ = fmt.Fprintln(l.Out, "\nThe sandbox is up. Leave this running so the zone can read the cluster's issuer\n"+
		"through the tunnel and the mirror. Ctrl-C stops up. Stopping does not delete the cluster.")
	// waiting ends when ctx ends or a background step stops. The step's
	// error is its cause.
	waiting, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	go func() {
		select {
		case err := <-state.services:
			stop(err)
		case <-waiting.Done():
		}
	}()
	for {
		again, err := l.confirm(waiting, "Run the checks again? No stops up.", true)
		if err == nil && again {
			l.recheck(ctx, state)
			continue
		}
		if errors.Is(err, ErrNoTerminal) {
			<-waiting.Done()
		}
		switch {
		case err == nil, ctx.Err() != nil, errors.Is(err, ErrInterrupted):
			return state.checks
		case waiting.Err() != nil:
			l.printLeft(steps, len(steps), state)
			return context.Cause(waiting) //nolint:wrapcheck // The cause is the background step's error from startService.
		}
		return err
	}
}

// recheck runs the checks and prints any failure. It stores the result in
// state.checks unless ctx ended during the checks.
func (l *Local) recheck(ctx context.Context, state *upRun) {
	err := l.CheckSandbox(ctx, "", "", "", "")
	if err != nil {
		_, _ = fmt.Fprintf(l.Out, "%v\n", err)
	}
	if ctx.Err() == nil {
		state.checks = err
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
