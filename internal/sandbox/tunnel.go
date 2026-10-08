package sandbox

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	// quickURL matches a quick tunnel URL, whose name is words joined by
	// dashes. Requiring a dash keeps it from matching api.trycloudflare.com,
	// which cloudflared logs.
	quickURL = regexp.MustCompile(`https://[a-z0-9]+(-[a-z0-9]+)+\.trycloudflare\.com`)
)

// Tunnel publishes the mirror on listen at the issuer's https URL through a
// Cloudflare tunnel, so that a Keycard zone on the internet can fetch the
// discovery document and JWKS. It runs cloudflared until ctx ends.
//
// With KEYCARD_LOCAL_TUNNEL set, it runs that named tunnel from the
// operator's Cloudflare account. KEYCARD_LOCAL_ISSUER must then be
// https://<hostname>, and the tunnel must have a DNS route for that hostname.
//
// With KEYCARD_LOCAL_TUNNEL empty, it starts a quick tunnel. A quick tunnel
// does not need a Cloudflare account, and gets a new
// https://<name>.trycloudflare.com URL every time it starts. KEYCARD_LOCAL_ISSUER must then be empty. Tunnel writes the
// URL to QuickIssuerFile and removes the file when it returns. The issuer is
// fixed when the cluster is created, so a new URL needs a new cluster.
func (l *Local) Tunnel(ctx context.Context, listen string) error {
	if !listenAddr.MatchString(listen) {
		return fmt.Errorf("%w: --listen must be HOST:PORT (got %q)", ErrTunnel, listen)
	}
	name, issuerURL := l.Settings[KeyTunnel], l.Settings[KeyIssuer]
	if name == "" {
		if issuerURL != "" {
			return fmt.Errorf("%w: %s is set, but a quick tunnel picks its own URL. "+
				"Clear %s, or set %s to the name of your Cloudflare tunnel", ErrIssuer, KeyIssuer, KeyIssuer, KeyTunnel)
		}
		return l.quickTunnel(ctx, listen)
	}
	if !hostIssuer.MatchString(issuerURL) {
		return fmt.Errorf("%w: the named tunnel %s needs %s set to https://<hostname>, with no port or path (got %q)",
			ErrIssuer, name, KeyIssuer, issuerURL)
	}
	if !tunnelName.MatchString(name) {
		return fmt.Errorf("%w: %s must be a tunnel name of letters, digits, dots, dashes and underscores (got %q)",
			ErrTunnel, KeyTunnel, name)
	}
	cmd, err := l.cloudflared(ctx, "tunnel", "--no-autoupdate", "run", "--url", "http://"+listen, name)
	if err != nil {
		return err
	}
	cmd.Stdout, cmd.Stderr = l.Out, l.Out
	_, _ = fmt.Fprintf(l.Out, "running the named tunnel %s for %s, forwarding to http://%s\n", name, issuerURL, listen)
	return stopped(ctx, cmd.Run())
}

// quickTunnel runs a quick tunnel and keeps its URL in QuickIssuerFile while
// cloudflared runs.
func (l *Local) quickTunnel(ctx context.Context, listen string) error {
	issuerFile := l.QuickIssuerFile()
	if err := os.MkdirAll(filepath.Dir(issuerFile), dirMode); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(issuerFile), err)
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
	// Every return below waits for cloudflared to exit, which ends the scan.
	defer forgetQuickURL(issuerFile, out, scanned)

	timer := time.NewTimer(quickURLTimeout)
	defer timer.Stop()
	select {
	case url := <-found:
		if err = os.WriteFile(issuerFile, []byte(url+"\n"), fileMode); err != nil {
			stop()
			<-exited
			return fmt.Errorf("writing %s: %w", issuerFile, err)
		}
		_, _ = fmt.Fprintf(out, "the issuer is %s. The other steps read it from %s while this tunnel runs.\n",
			url, issuerFile)
	case err = <-exited:
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("%w: cloudflared exited before it printed a trycloudflare.com URL: %w", ErrTunnel, err)
	case <-timer.C:
		stop()
		<-exited
		return fmt.Errorf("%w: cloudflared did not print a trycloudflare.com URL within %s", ErrTunnel, quickURLTimeout)
	}
	return stopped(ctx, <-exited)
}

// writeQuickConfig writes a cloudflared config file for a quick tunnel and
// returns its path. With its own config file, cloudflared does not read
// ~/.cloudflared/config.yml, which could make the quick tunnel a named one.
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

// forgetQuickURL waits for the log scan to end, then deletes the file that
// holds the quick tunnel's URL.
func forgetQuickURL(issuerFile string, out io.Writer, scanned <-chan struct{}) {
	<-scanned
	if os.Remove(issuerFile) == nil {
		_, _ = fmt.Fprintln(out, "Deleted local/out/issuer. The quick tunnel's URL has stopped working.")
	}
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
// so that it can close its connections to Cloudflare. It gets SIGKILL if it
// has not exited after stopTimeout.
func (l *Local) cloudflared(ctx context.Context, args ...string) (*exec.Cmd, error) {
	cmd, err := l.Tool(ctx, "cloudflared", args...)
	if err != nil {
		return nil, err
	}
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = stopTimeout
	return cmd, nil
}

// stopped returns nil when ctx has ended, because the operator stopped
// cloudflared. Otherwise it returns err wrapped in ErrTunnel.
func stopped(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return nil //nolint:nilerr // The operator stopped cloudflared, so its exit status does not matter.
	}
	return fmt.Errorf("%w: cloudflared: %w", ErrTunnel, err)
}
