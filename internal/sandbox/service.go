package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The background services. Each runs `sandbox <name>` as a process of its
// own, so the tunnel's URL and the mirror outlive the TUI that started them.
const (
	ServiceTunnel = "tunnel"
	ServiceMirror = "mirror"
	// serviceStop bounds the wait for a service to exit after SIGTERM.
	serviceStop = 15 * time.Second
	// listenFlag is the flag that gives a service its address.
	listenFlag = "--listen"
)

// ErrNotRunning means the service's pid file is missing or does not name
// the running service.
var ErrNotRunning = errors.New("not running")

// ServiceState is what a service's pid file says, and whether that process
// still runs.
type ServiceState struct {
	Name    string
	PID     int
	Running bool
	// LogPath is the service's log, which holds the output of its last start.
	LogPath string
}

// ServiceStatus reads local/out/<name>.pid and reports whether the process
// it names is still that service. A pid the system gave to another program
// since does not count, even when that program's arguments name the
// service.
func (l *Local) ServiceStatus(ctx context.Context, name string) ServiceState {
	state := ServiceState{Name: name, LogPath: l.serviceFile(name, ".log")}
	raw, err := os.ReadFile(l.serviceFile(name, ".pid"))
	if err != nil {
		return state
	}
	if state.PID, err = strconv.Atoi(strings.TrimSpace(string(raw))); err != nil || state.PID <= 0 {
		state.PID = 0
		return state
	}
	state.Running = processRuns(ctx, state.PID, name)
	return state
}

// spawnService starts `sandbox <name> --listen <listen>` in a session of
// its own, writes its pid to local/out/<name>.pid, and returns without
// waiting for it. The service gets the settings in its environment, and no
// config file, so that it runs with this session's settings, saved or not.
// Its output replaces local/out/<name>.log. A service that already runs is
// stopped first.
func (l *Local) spawnService(ctx context.Context, name, listen string) error {
	if err := l.StopService(ctx, name); err != nil && !errors.Is(err, ErrNotRunning) {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("%w: finding this program to start %s: %w", ErrService, name, err)
	}
	args := []string{name, listenFlag, listen}
	log, err := openLog(l.serviceFile(name, ".log"))
	if err != nil {
		return err
	}
	defer func() { _ = log.Close() }()
	// The service must outlive ctx, which ends when the TUI quits.
	cmd := exec.Command(self, args...) //nolint:gosec,noctx // G204: this program and its own subcommand.
	cmd.Stdout, cmd.Stderr = log, log
	cmd.Env = append(os.Environ(), l.serviceEnv()...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err = cmd.Start(); err != nil {
		return fmt.Errorf("%w: starting %s: %w", ErrService, name, err)
	}
	pid := cmd.Process.Pid
	// Wait reaps the service if it exits while this process runs, so that a
	// zombie does not look like a running service.
	go func() { _ = cmd.Wait() }()
	pidFile := l.serviceFile(name, ".pid")
	if err = os.WriteFile(pidFile, []byte(strconv.Itoa(pid)+"\n"), fileMode); err != nil {
		_ = syscall.Kill(-pid, syscall.SIGTERM)
		return fmt.Errorf("writing %s: %w", pidFile, err)
	}
	return nil
}

// StopService sends SIGTERM to the service's process group, which holds
// cloudflared too, and waits for it to exit. After serviceStop it sends
// SIGKILL. It removes the pid file. A service that does not run returns
// ErrNotRunning, and StopService signals nothing.
func (l *Local) StopService(ctx context.Context, name string) error {
	state := l.ServiceStatus(ctx, name)
	defer func() { _ = os.Remove(l.serviceFile(name, ".pid")) }()
	if !state.Running {
		return fmt.Errorf("%w: %s", ErrNotRunning, name)
	}
	_ = syscall.Kill(-state.PID, syscall.SIGTERM)
	deadline := time.Now().Add(serviceStop)
	for processRuns(ctx, state.PID, name) {
		if time.Now().After(deadline) {
			_ = syscall.Kill(-state.PID, syscall.SIGKILL)
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("stopping %s: %w", name, ctx.Err())
		case <-time.After(pollEvery):
		}
	}
	if name == ServiceTunnel {
		// A tunnel killed before it cleaned up leaves its URL behind.
		_ = os.Remove(l.QuickIssuerFile())
	}
	return nil
}

// serviceEnv is the environment that gives a service this session's
// settings: no config file, and each setting as its variable.
func (l *Local) serviceEnv() []string {
	env := []string{EnvConfig + "=" + NoConfig}
	for _, key := range settingKeys() {
		if value := l.Settings[key]; value != "" {
			env = append(env, key+"="+value)
		}
	}
	return env
}

func (l *Local) serviceFile(name, suffix string) string {
	return filepath.Join(l.outDir(), name+suffix)
}

// processRuns reports whether pid runs the service name as spawnService
// starts it: it leads a process group of its own, and its arguments end
// with `<name> --listen <address>`.
func processRuns(ctx context.Context, pid int, name string) bool {
	if pgid, err := syscall.Getpgid(pid); err != nil || pgid != pid {
		return false
	}
	//nolint:gosec // G204: ps is fixed, and the pid is a number this package wrote.
	out, err := exec.CommandContext(ctx, "ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	fields := strings.Fields(string(out))
	last := len(fields) - 1
	return last >= 3 && fields[last-2] == name && fields[last-1] == listenFlag
}
