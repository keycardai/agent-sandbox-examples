package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
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

// ErrNotRunning means no service holds the lock on its pid file.
var ErrNotRunning = errors.New("not running")

// ServiceState is what a service's pid file says, and whether the service
// still runs.
type ServiceState struct {
	Name    string
	PID     int
	Running bool
	// LogPath is the service's log, which holds the output of its last start.
	LogPath string
}

// ServiceStatus reads local/out/<name>.pid. A running service holds an
// exclusive lock on that file, and the kernel releases the lock when the
// service exits, even on SIGKILL. A pid file nobody locks names no running
// service, whatever program has that pid now.
func (l *Local) ServiceStatus(name string) ServiceState {
	state := ServiceState{Name: name, LogPath: l.serviceFile(name, ".log")}
	file, err := os.Open(l.serviceFile(name, ".pid"))
	if err != nil {
		return state
	}
	defer func() { _ = file.Close() }()
	if err = syscall.Flock(int(file.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); !errors.Is(err, syscall.EWOULDBLOCK) {
		return state
	}
	raw, err := io.ReadAll(file)
	if err != nil {
		return state
	}
	if state.PID, err = strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && state.PID > 0 {
		state.Running = true
	}
	return state
}

// spawnService starts `sandbox <name> --listen <listen>` in a session of
// its own, and returns without waiting for it. The service holds an
// exclusive lock on local/out/<name>.pid, which names its pid, until it
// exits. It gets the settings in its environment, and no config file, so
// that it runs with this session's settings, saved or not. Its output
// replaces local/out/<name>.log. The caller stops a running service first.
func (l *Local) spawnService(name, listen string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("%w: finding this program to start %s: %w", ErrService, name, err)
	}
	args := []string{name, listenFlag, listen}
	log, err := newLog(l.serviceFile(name, ".log"))
	if err != nil {
		return err
	}
	defer func() { _ = log.Close() }()
	lock, err := lockedPIDFile(l.outDir(), name)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(lock.Name()); _ = lock.Close() }()
	// The service must outlive this process, which ends when the TUI quits.
	cmd := exec.Command(self, args...) //nolint:gosec,noctx // G204: this program and its own subcommand.
	cmd.Stdout, cmd.Stderr = log, log
	cmd.Env = append(os.Environ(), l.serviceEnv()...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// The service inherits the locked file, and so holds the lock until it
	// exits. Its children, such as cloudflared, inherit it too, which is why
	// StopService signals the whole process group.
	cmd.ExtraFiles = []*os.File{lock}
	if err = cmd.Start(); err != nil {
		return fmt.Errorf("%w: starting %s: %w", ErrService, name, err)
	}
	pid := cmd.Process.Pid
	// Wait reaps the service if it exits while this process runs.
	go func() { _ = cmd.Wait() }()
	// The rename makes the pid file appear whole, and already locked.
	pidFile := l.serviceFile(name, ".pid")
	if _, err = fmt.Fprintf(lock, "%d\n", pid); err == nil {
		err = os.Rename(lock.Name(), pidFile)
	}
	if err != nil {
		l.signalService(ServiceState{Name: name, PID: pid}, syscall.SIGTERM, "SIGTERM")
		return fmt.Errorf("writing %s: %w", pidFile, err)
	}
	return nil
}

// newLog puts a new, empty file at path in place of the file there. It
// makes the new file before the old one goes, so that the two never share
// an inode, and a reader of the old file sees the file change.
func newLog(path string) (*os.File, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return nil, fmt.Errorf("%w: creating %s: %w", ErrService, dir, err)
	}
	file, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err == nil {
		err = os.Rename(file.Name(), path)
	}
	if err != nil {
		if file != nil {
			_ = os.Remove(file.Name())
			_ = file.Close()
		}
		return nil, fmt.Errorf("%w: replacing %s: %w", ErrService, path, err)
	}
	return file, nil
}

// lockedPIDFile creates a new file in dir for the pid of the service name,
// with an exclusive lock on it.
func lockedPIDFile(dir, name string) (*os.File, error) {
	file, err := os.CreateTemp(dir, "."+name+"-*.pid")
	if err != nil {
		return nil, fmt.Errorf("%w: starting %s: %w", ErrService, name, err)
	}
	if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = os.Remove(file.Name())
		_ = file.Close()
		return nil, fmt.Errorf("%w: locking the pid file of %s: %w", ErrService, name, err)
	}
	return file, nil
}

// StopService stops a running service with SIGTERM to its process group,
// which holds cloudflared too, and with SIGKILL when it still runs after
// serviceStop. A service that does not run returns ErrNotRunning, and
// StopService signals nothing. It says on l.Out when a step of the stop
// fails.
func (l *Local) StopService(ctx context.Context, name string) error {
	state := l.ServiceStatus(name)
	if !state.Running {
		return fmt.Errorf("%w: %s", ErrNotRunning, name)
	}
	l.signalService(state, syscall.SIGTERM, "SIGTERM")
	deadline := time.Now().Add(serviceStop)
	for l.ServiceStatus(name).Running {
		if time.Now().After(deadline) {
			l.signalService(state, syscall.SIGKILL, "SIGKILL")
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("stopping %s: %w", name, ctx.Err())
		case <-time.After(pollEvery):
		}
	}
	if name == ServiceTunnel {
		l.removeLeftURL()
	}
	return nil
}

// removeLeftURL deletes the quick tunnel's URL file that a tunnel killed
// before it cleaned up leaves behind. It keeps the file while another
// tunnel, such as sandbox up's, holds it.
func (l *Local) removeLeftURL() {
	file := l.QuickIssuerFile()
	if _, running, err := readQuickIssuer(file); err != nil || running {
		return
	}
	if err := os.Remove(file); err != nil && !errors.Is(err, fs.ErrNotExist) {
		_, _ = fmt.Fprintf(l.Out, "Could not delete %s, which the tunnel left: %v\n", file, err)
	}
}

// StopServices stops the mirror and then the tunnel, and says on l.Out
// which it stopped. A service that does not run is not an error.
func (l *Local) StopServices(ctx context.Context) error {
	for _, name := range []string{ServiceMirror, ServiceTunnel} {
		err := l.StopService(ctx, name)
		switch {
		case err == nil:
			_, _ = fmt.Fprintf(l.Out, "Stopped the %s.\n", name)
		case !errors.Is(err, ErrNotRunning):
			return err
		}
	}
	return nil
}

// signalService sends sig, named name, to the service's process group, and
// says on l.Out when that fails.
func (l *Local) signalService(state ServiceState, sig syscall.Signal, name string) {
	if err := syscall.Kill(-state.PID, sig); err != nil {
		_, _ = fmt.Fprintf(l.Out, "Could not send %s to the %s, process group %d: %v\n", name, state.Name, state.PID, err)
	}
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
