package sandbox

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// quickURLTimeout bounds the wait for a quick tunnel's URL.
	quickURLTimeout = 60 * time.Second
	// stopTimeout is how long cloudflared has to exit after SIGTERM.
	stopTimeout = 10 * time.Second
)

var (
	listenAddr = regexp.MustCompile(`^[0-9a-z.-]+:[0-9]+$`)
	// hostIssuer is https://<hostname> with no port or path.
	hostIssuer = regexp.MustCompile(`^https://[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
	tunnelName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	// quickURL matches a quick tunnel URL. The required dash excludes
	// api.trycloudflare.com, which cloudflared also logs.
	quickURL = regexp.MustCompile(`https://[a-z0-9]+(-[a-z0-9]+)+\.trycloudflare\.com`)
)

// Tunnel publishes the mirror on listen at the issuer's https URL through a
// Cloudflare tunnel, so a Keycard zone on the internet can fetch the
// discovery document and JWKS. It runs cloudflared until ctx ends.
//
// With KEYCARD_LOCAL_TUNNEL set, it runs that named tunnel from the
// operator's Cloudflare account. KEYCARD_LOCAL_ISSUER must then be
// https://<hostname>, and the tunnel must have a DNS route for that hostname.
//
// With KEYCARD_LOCAL_TUNNEL empty, it starts a quick tunnel, and
// KEYCARD_LOCAL_ISSUER must be empty. A quick tunnel gets a new
// https://<name>.trycloudflare.com URL on every start. Tunnel writes the URL
// to QuickIssuerFile, locks the file while it runs, and removes the file when
// it returns. It refuses to start while another tunnel has the file locked.
// It deletes a file that is not locked. The issuer is fixed when the cluster
// is created, so a new URL needs a new cluster.
func (l *Local) Tunnel(ctx context.Context, listen string) error {
	if err := l.checkTunnel(listen); err != nil {
		return err
	}
	name, issuerURL := l.Settings[KeyTunnel], l.Settings[KeyIssuer]
	if name == "" {
		return l.quickTunnel(ctx, listen)
	}
	cmd, err := l.cloudflared(ctx, "tunnel", "--no-autoupdate", "run", "--url", "http://"+listen, name)
	if err != nil {
		return err
	}
	cmd.Stdout, cmd.Stderr = l.Out, l.Out
	_, _ = fmt.Fprintf(l.Out, "running the named tunnel %s for %s, forwarding to http://%s\n", name, issuerURL, listen)
	return stopped(ctx, cmd.Run())
}

// checkTunnel checks listen and the tunnel settings that Tunnel needs.
func (l *Local) checkTunnel(listen string) error {
	if !listenAddr.MatchString(listen) {
		return fmt.Errorf("%w: --listen must be HOST:PORT (got %q)", ErrTunnel, listen)
	}
	name, issuerURL := l.Settings[KeyTunnel], l.Settings[KeyIssuer]
	switch {
	case name == "" && issuerURL != "":
		return fmt.Errorf("%w: %s is set, but a quick tunnel picks its own URL. "+
			"Clear %s, or set %s to the name of your Cloudflare tunnel", ErrIssuer, KeyIssuer, KeyIssuer, KeyTunnel)
	case name == "":
		return nil
	case !hostIssuer.MatchString(issuerURL):
		return fmt.Errorf("%w: the named tunnel %s needs %s set to https://<hostname>, with no port or path (got %q)",
			ErrIssuer, name, KeyIssuer, issuerURL)
	case !tunnelName.MatchString(name):
		return fmt.Errorf("%w: %s must be a tunnel name of letters, digits, dots, dashes and underscores (got %q)",
			ErrTunnel, KeyTunnel, name)
	}
	return nil
}

// quickTunnel runs a quick tunnel and keeps its URL in QuickIssuerFile while
// cloudflared runs. It removes only a file it wrote.
func (l *Local) quickTunnel(ctx context.Context, listen string) error {
	issuerFile := l.QuickIssuerFile()
	if err := checkNoQuickURL(issuerFile, l.Out); err != nil {
		return err
	}
	config, err := writeQuickConfig()
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(config) }()

	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	cmd, err := l.cloudflared(runCtx, "tunnel", "--no-autoupdate", "--config", config, "--url", "http://"+listen)
	if err != nil {
		return err
	}
	// cloudflared reads its flags from TUNNEL_* variables. TUNNEL_NAME turns
	// a quick tunnel into a named tunnel in the operator's Cloudflare account.
	cmd.Env = slices.DeleteFunc(cmd.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "TUNNEL_") })
	logs, logWriter := io.Pipe()
	cmd.Stdout, cmd.Stderr = logWriter, logWriter
	if err = cmd.Start(); err != nil {
		return fmt.Errorf("%w: starting cloudflared: %w", ErrTunnel, err)
	}
	exited := make(chan error, 1)
	go func() {
		waitErr := cmd.Wait()
		_ = logWriter.Close()
		exited <- waitErr
	}()
	out := &lockedWriter{w: l.Out}
	found, scanned := make(chan string, 1), make(chan struct{})
	go func() {
		scanQuickURL(logs, out, found)
		close(scanned)
	}()
	published := ""
	var lock *os.File
	// Every return below waits for cloudflared to exit, which ends the scan.
	defer func() { forgetQuickURL(issuerFile, published, lock, out, scanned) }()

	timer := time.NewTimer(quickURLTimeout)
	defer timer.Stop()
	select {
	case url := <-found:
		if lock, err = publishQuickURL(issuerFile, url); err != nil {
			stop()
			<-exited
			return err
		}
		published = url
		_, _ = fmt.Fprintf(out, "the issuer is %s. The other steps read it from %s while this tunnel runs.\n",
			url, issuerFile)
	case err = <-exited:
		return exitedEarly(ctx, err)
	case <-timer.C:
		stop()
		<-exited
		return fmt.Errorf("%w: cloudflared did not print a trycloudflare.com URL within %s", ErrTunnel, quickURLTimeout)
	}
	return stopped(ctx, <-exited)
}

// writeQuickConfig writes a cloudflared config file for a quick tunnel and
// returns its path. With its own config file, cloudflared does not read
// ~/.cloudflared/config.yml. That file's keys would apply to the quick tunnel,
// and its ingress rules would replace --url.
func writeQuickConfig() (string, error) {
	config, err := os.CreateTemp("", "cloudflared-*.yml")
	if err != nil {
		return "", fmt.Errorf("creating the cloudflared config: %w", err)
	}
	_, err = config.WriteString("no-autoupdate: true\n")
	if closeErr := config.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(config.Name())
		return "", fmt.Errorf("writing the cloudflared config: %w", err)
	}
	return config.Name(), nil
}

// checkNoQuickURL creates the directory of issuerFile. It fails if a running
// tunnel has issuerFile locked, and deletes issuerFile if it is not locked.
func checkNoQuickURL(issuerFile string, out io.Writer) error {
	if err := os.MkdirAll(filepath.Dir(issuerFile), dirMode); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(issuerFile), err)
	}
	_, running, err := readQuickIssuer(issuerFile)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("%w: %w", ErrTunnel, err)
	case running:
		return fmt.Errorf("%w: another quick tunnel is running and holds %s. Stop it first", ErrTunnel, issuerFile)
	}
	if err = os.Remove(issuerFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %w", ErrTunnel, err)
	}
	_, _ = fmt.Fprintf(out, "Deleted %s. The quick tunnel that wrote it is not running.\n", issuerFile)
	return nil
}

// readQuickIssuer returns the URL in issuerFile, and whether a running quick
// tunnel has the file locked. The tunnel holds an exclusive flock on the file
// while it runs. The kernel releases the lock when the tunnel's process
// exits, even on SIGKILL. If the file does not exist, the error wraps
// fs.ErrNotExist.
func readQuickIssuer(issuerFile string) (string, bool, error) {
	file, err := os.Open(issuerFile) //nolint:gosec // G304: issuerFile is the tunnel step's own file.
	if err != nil {
		return "", false, fmt.Errorf("reading the quick tunnel's URL: %w", err)
	}
	defer func() { _ = file.Close() }()
	running := false
	switch err = syscall.Flock(int(file.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); {
	case errors.Is(err, syscall.EWOULDBLOCK):
		running = true
	case err != nil:
		return "", false, fmt.Errorf("locking %s: %w", issuerFile, err)
	}
	raw, err := io.ReadAll(file)
	if err != nil {
		return "", false, fmt.Errorf("reading %s: %w", issuerFile, err)
	}
	return strings.TrimSpace(string(raw)), running, nil
}

// exitedEarly returns the error for a cloudflared that exited with err before
// it printed a URL. It returns nil once ctx has ended.
func exitedEarly(ctx context.Context, err error) error {
	switch {
	case ctx.Err() != nil:
		return nil //nolint:nilerr // The operator stopped cloudflared, so its exit status does not matter.
	case err == nil:
		return fmt.Errorf("%w: cloudflared exited with status 0 before it printed a trycloudflare.com URL", ErrTunnel)
	}
	return fmt.Errorf("%w: cloudflared exited before it printed a trycloudflare.com URL: %w", ErrTunnel, err)
}

// forgetQuickURL waits for the log scan to end. If issuerFile still has the
// published URL, it deletes the file. Then it closes lock. Deleting first
// means a reader never finds the file unlocked while the tunnel runs. It does
// nothing when published is empty.
func forgetQuickURL(issuerFile, published string, lock *os.File, out io.Writer, scanned <-chan struct{}) {
	<-scanned
	if published == "" {
		return
	}
	defer func() { _ = lock.Close() }()
	//nolint:gosec // G304: issuerFile is the step's own file.
	if raw, err := os.ReadFile(issuerFile); err != nil || string(raw) != published+"\n" {
		return
	}
	if os.Remove(issuerFile) == nil {
		_, _ = fmt.Fprintf(out, "Deleted %s. The quick tunnel's URL has stopped working.\n", issuerFile)
	}
}

// publishQuickURL writes url to issuerFile through a hard link, so the file
// appears whole or not at all. It fails if issuerFile exists. It locks the
// file before the link, and returns the locked file. Close it after removing
// issuerFile.
func publishQuickURL(issuerFile, url string) (*os.File, error) {
	tmp, err := os.CreateTemp(filepath.Dir(issuerFile), ".issuer-*")
	if err != nil {
		return nil, fmt.Errorf("%w: writing %s: %w", ErrTunnel, issuerFile, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err = tmp.Chmod(fileMode); err == nil {
		_, err = tmp.WriteString(url + "\n")
	}
	if err == nil {
		err = syscall.Flock(int(tmp.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	}
	if err == nil {
		err = os.Link(tmp.Name(), issuerFile)
	}
	if err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("%w: writing %s: %w", ErrTunnel, issuerFile, err)
	}
	return tmp, nil
}

// scanQuickURL copies cloudflared's log lines to out, and sends the first
// quick tunnel URL in them to found.
func scanQuickURL(logs io.Reader, out io.Writer, found chan<- string) {
	lines := bufio.NewScanner(logs)
	sent := false
	for lines.Scan() {
		_, _ = fmt.Fprintln(out, lines.Text())
		if url := quickURL.FindString(lines.Text()); url != "" && !sent {
			found <- url
			sent = true
		}
	}
	// The scanner stops at a line that is too long. Copy the rest, so that
	// cloudflared does not block writing its logs.
	_, _ = io.Copy(out, logs)
}

// cloudflared returns a cloudflared command that gets SIGTERM when ctx ends,
// and SIGKILL if it has not exited stopTimeout later.
func (l *Local) cloudflared(ctx context.Context, args ...string) (*exec.Cmd, error) {
	cmd, err := l.Tool(ctx, "cloudflared", args...)
	if err != nil {
		return nil, err
	}
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = stopTimeout
	return cmd, nil
}

// stopped returns nil when err is nil or ctx has ended. Otherwise it wraps
// err in ErrTunnel.
func stopped(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return nil //nolint:nilerr // The operator stopped cloudflared, so its exit status does not matter.
	}
	return fmt.Errorf("%w: cloudflared: %w", ErrTunnel, err)
}

// lockedWriter serializes writes from the goroutines of one step.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.w.Write(p)
	if err != nil {
		return n, fmt.Errorf("writing output: %w", err)
	}
	return n, nil
}
