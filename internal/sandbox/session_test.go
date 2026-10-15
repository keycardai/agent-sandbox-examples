package sandbox_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
)

// publishedIssuer is an issuer the operator publishes.
const publishedIssuer = "https://published.example.org"

// stepIDs returns the IDs of a session's steps.
func stepIDs(session *sandbox.Session) []sandbox.StepID {
	ids := make([]sandbox.StepID, 0, len(session.Steps()))
	for _, step := range session.Steps() {
		ids = append(ids, step.ID)
	}
	return ids
}

// TestSessionSteps lists the steps for each way of publishing the issuer.
func TestSessionSteps(t *testing.T) {
	t.Parallel()
	deploy := []sandbox.StepID{
		sandbox.StepZone,
		sandbox.StepInstall,
		sandbox.StepSandbox,
		sandbox.StepPod,
		sandbox.StepChecks,
	}
	k3d := []sandbox.StepID{sandbox.StepCluster, sandbox.StepMirror, sandbox.StepIssuer, sandbox.StepImages}
	tests := []struct {
		name     string
		settings sandbox.Settings
		want     []sandbox.StepID
	}{
		{"a quick tunnel", sandbox.Settings{}, slices.Concat([]sandbox.StepID{sandbox.StepTunnel}, k3d, deploy)},
		{
			"a named tunnel",
			sandbox.Settings{sandbox.KeyTunnel: namedTunnel, sandbox.KeyIssuer: publishedIssuer},
			slices.Concat([]sandbox.StepID{sandbox.StepTunnel}, k3d, deploy),
		},
		{
			"issuer the operator publishes",
			sandbox.Settings{sandbox.KeyIssuer: publishedIssuer},
			slices.Concat(k3d, deploy),
		},
		{"existing cluster", existing(), slices.Concat([]sandbox.StepID{sandbox.StepIssuer}, deploy)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			local, _ := newLocal(t, tc.settings, "")
			if got := stepIDs(local.NewSession("127.0.0.1:8480", time.Second)); !slices.Equal(got, tc.want) {
				t.Errorf("steps = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSessionProblems finds what would stop a bring-up before any step
// runs: no settings, and a resource without a credential.
func TestSessionProblems(t *testing.T) {
	t.Parallel()
	local, _ := newLocal(t, sandbox.Settings{}, "")
	problems := local.NewSession("127.0.0.1:8480", time.Second).Problems()
	if len(problems) != 1 || !errors.Is(problems[0], sandbox.ErrSettings) {
		t.Errorf("problems with no settings = %v, want one ErrSettings", problems)
	}

	run := firstRun(t)
	session := run.local.NewSession("127.0.0.1:8480", time.Second)
	if problems = session.Problems(); len(problems) != 0 {
		t.Errorf("problems after init = %v, want none", problems)
	}
	if err := run.keychain.Delete(openRouterSecret); err != nil {
		t.Fatal(err)
	}
	problems = session.Problems()
	if len(problems) != 1 || !strings.Contains(problems[0].Error(), "resources[2]") {
		t.Errorf("problems = %v, want one about resources[2]", problems)
	}
}

// TestSessionPods reads the pods and their containers from kubectl.
func TestSessionPods(t *testing.T) {
	t.Parallel()
	pods := `{"items":[{"metadata":{"name":"` + userID + `","creationTimestamp":"2026-10-09T10:00:00Z"},
"status":{"phase":"Running",
"initContainerStatuses":[{"name":"egress-netsetup","ready":false,"restartCount":0,
  "state":{"terminated":{"reason":"Completed","exitCode":0}}}],
"containerStatuses":[{"name":"agent","ready":false,"restartCount":3,
  "state":{"waiting":{"reason":"CrashLoopBackOff"}}},
  {"name":"egress-proxy","ready":true,"restartCount":0,"state":{"running":{}}}]}}]}`
	local, _ := newLocal(t, sandbox.Settings{sandbox.KeyCluster: cluster, sandbox.KeyUserEmail: email},
		`case "$*" in *"get pods -o json") echo '`+strings.ReplaceAll(pods, "\n", "")+`' ;; esac`)
	writeFile(t, filepath.Join(local.Root, "local", "out", "zone-setup.yaml"), zoneSetupOutput)
	got, err := local.NewSession("127.0.0.1:8480", time.Second).Pods(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != userID {
		t.Fatalf("pods = %+v, want the user's pod", got)
	}
	want := []sandbox.ContainerStatus{
		{Name: "egress-netsetup", Init: true, State: "terminated", Reason: "Completed"},
		{Name: "agent", Restarts: 3, State: "waiting", Reason: "CrashLoopBackOff"},
		{Name: "egress-proxy", Ready: true, State: "running"},
	}
	if !slices.Equal(got[0].Containers, want) {
		t.Errorf("containers = %+v, want %+v", got[0].Containers, want)
	}
}

// TestMain runs the test binary as a service when the tunnel or mirror step
// starts it, as `<binary> <service> --listen <address>`, and runs the tests
// otherwise.
func TestMain(m *testing.M) {
	if len(os.Args) == 4 && os.Args[2] == "--listen" {
		os.Exit(fakeService(os.Args[1], os.Args[3]))
	}
	os.Exit(m.Run())
}

// fakeService stands in for `sandbox tunnel` and `sandbox mirror`. It
// prints the settings that tell which tunnel runs, and the mirror listens
// on address. It runs until it is stopped, for a minute at most.
func fakeService(name, address string) int {
	_, _ = fmt.Fprintf(os.Stdout, "%s=%s %s=%s\n", sandbox.EnvConfig, os.Getenv(sandbox.EnvConfig),
		sandbox.KeyTunnel, os.Getenv(sandbox.KeyTunnel))
	if name == sandbox.ServiceMirror {
		listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", address)
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer func() { _ = listener.Close() }()
	}
	time.Sleep(time.Minute)
	return 0
}

// startStandIn starts sh with args after its script, which sleeps, and
// returns its pid. setsid makes it the leader of a process group of its
// own, as the services are.
func startStandIn(t *testing.T, setsid bool, args ...string) int {
	t.Helper()
	// The second command keeps sh from replacing itself with sleep.
	//nolint:gosec // G204: a fixed script; the arguments only name a service.
	cmd := exec.CommandContext(t.Context(), "sh", append([]string{"-c", "sleep 30; :"}, args...)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: setsid}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = cmd.Wait() }()
	return cmd.Process.Pid
}

// TestServiceStatus reads a service's pid file. A pid that runs the
// service as the steps start it counts, and StopService stops it.
func TestServiceStatus(t *testing.T) {
	t.Parallel()
	local, _ := newLocal(t, sandbox.Settings{}, "")
	pidFile := filepath.Join(local.Root, "local", "out", sandbox.ServiceTunnel+".pid")
	if state := local.ServiceStatus(t.Context(), sandbox.ServiceTunnel); state.Running {
		t.Errorf("status with no pid file = %+v, want not running", state)
	}

	service := startStandIn(t, true, sandbox.ServiceTunnel, "--listen", "127.0.0.1:8480")
	writeFile(t, pidFile, strconv.Itoa(service)+"\n")
	if state := local.ServiceStatus(t.Context(), sandbox.ServiceTunnel); !state.Running {
		t.Fatalf("status = %+v, want running", state)
	}
	if state := local.ServiceStatus(t.Context(), sandbox.ServiceMirror); state.Running {
		t.Errorf("mirror status = %+v, want not running", state)
	}
	if err := local.StopService(t.Context(), sandbox.ServiceTunnel); err != nil {
		t.Fatal(err)
	}
	if state := local.ServiceStatus(t.Context(), sandbox.ServiceTunnel); state.Running {
		t.Errorf("status after stop = %+v, want not running", state)
	}
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Errorf("stat %s: %v, want it removed", pidFile, err)
	}
}

// TestServiceStatusOtherProgram does not count a pid of another program as
// the service, even one whose arguments name the service, and StopService
// leaves that program alone.
func TestServiceStatusOtherProgram(t *testing.T) {
	t.Parallel()
	local, _ := newLocal(t, sandbox.Settings{}, "")
	pidFile := filepath.Join(local.Root, "local", "out", sandbox.ServiceTunnel+".pid")
	for name, pid := range map[string]int{
		"arguments that name the service":     startStandIn(t, true, sandbox.ServiceTunnel),
		"not the leader of its process group": startStandIn(t, false, sandbox.ServiceTunnel, "--listen", "127.0.0.1:8480"),
	} {
		writeFile(t, pidFile, strconv.Itoa(pid)+"\n")
		if state := local.ServiceStatus(t.Context(), sandbox.ServiceTunnel); state.Running {
			t.Errorf("%s: status = %+v, want not running", name, state)
		}
		if err := local.StopService(t.Context(), sandbox.ServiceTunnel); !errors.Is(err, sandbox.ErrNotRunning) {
			t.Errorf("%s: StopService = %v, want ErrNotRunning", name, err)
		}
		if err := syscall.Kill(pid, 0); err != nil {
			t.Errorf("%s: the process is gone after StopService: %v", name, err)
		}
	}
}

// TestSessionTunnel starts the tunnel the settings name, with the settings
// in its environment and no config file. It keeps that tunnel while it
// runs, and starts another for a tunnel of another name.
func TestSessionTunnel(t *testing.T) {
	t.Parallel()
	local, _ := newLocal(t, sandbox.Settings{}, "")
	t.Cleanup(func() { _ = local.StopService(context.Background(), sandbox.ServiceTunnel) })
	run := func(tunnel string) (string, sandbox.ServiceState) {
		t.Helper()
		named := *local
		named.Settings = sandbox.Settings{sandbox.KeyTunnel: tunnel, sandbox.KeyIssuer: publishedIssuer}
		out := &bytes.Buffer{}
		if err := named.NewSession("127.0.0.1:8480", time.Second).Run(t.Context(), sandbox.StepTunnel, out); err != nil {
			t.Fatal(err)
		}
		return out.String(), local.ServiceStatus(t.Context(), sandbox.ServiceTunnel)
	}

	_, first := run(namedTunnel)
	out, kept := run(namedTunnel)
	if !strings.Contains(out, "The tunnel "+namedTunnel+" is running.") || kept.PID != first.PID {
		t.Errorf("second run: pid %d, then %d; output %q; want the running tunnel kept", first.PID, kept.PID, out)
	}
	out, other := run("other-tunnel")
	if !strings.Contains(out, "Started the tunnel.") || !other.Running || other.PID == first.PID {
		t.Errorf("run with another tunnel: %+v, output %q; want a new tunnel", other, out)
	}
	want := sandbox.EnvConfig + "=" + sandbox.NoConfig + " " + sandbox.KeyTunnel + "=other-tunnel\n"
	if got := waitForFile(t, other.LogPath); got != want {
		t.Errorf("the tunnel's log = %q, want %q", got, want)
	}
}

// waitForFile returns the content of path once it has some, and fails the
// test after 30 seconds.
func waitForFile(t *testing.T, path string) string {
	t.Helper()
	// The whole package's tests start fake tools at once, which can slow
	// one down a lot, so the wait is long.
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		//nolint:gosec // G304: the path is the test's own file.
		if raw, err := os.ReadFile(path); err == nil && len(raw) > 0 {
			return string(raw)
		}
	}
	t.Fatalf("%s had nothing in it within 30 seconds", path)
	return ""
}

// TestSessionQuickTunnelHeld refuses to start a quick tunnel while another
// one, such as sandbox up in another terminal, holds the issuer file, and
// leaves that file in place.
func TestSessionQuickTunnelHeld(t *testing.T) {
	t.Parallel()
	local, _ := newLocal(t, sandbox.Settings{}, "")
	t.Cleanup(func() { _ = local.StopService(context.Background(), sandbox.ServiceTunnel) })
	holdIssuerFile(t, local.QuickIssuerFile(), "https://quick-brown-fox.trycloudflare.com\n")
	err := local.NewSession("127.0.0.1:8480", time.Second).Run(t.Context(), sandbox.StepTunnel, &bytes.Buffer{})
	checkErr(t, err, []error{sandbox.ErrTunnel}, "another quick tunnel is running")
	if _, err = os.Stat(local.QuickIssuerFile()); err != nil {
		t.Errorf("the issuer file: %v; want it kept", err)
	}
}

// TestSessionMirror starts the mirror on the session's address. It refuses
// while something else answers there, such as a mirror started in another
// terminal, because the step would pass with that one.
func TestSessionMirror(t *testing.T) {
	t.Parallel()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	local, _ := newLocal(t, sandbox.Settings{}, "")
	t.Cleanup(func() { _ = local.StopService(context.Background(), sandbox.ServiceMirror) })
	err = local.NewSession(address, time.Second).Run(t.Context(), sandbox.StepMirror, &bytes.Buffer{})
	checkErr(t, err, []error{sandbox.ErrService}, "something else answers on "+address)
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}

	// Another test can take a free port before the mirror listens on it, so
	// the mirror gets up to three.
	for range 3 {
		err = local.NewSession(freeAddr(t), time.Second).Run(t.Context(), sandbox.StepMirror, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "something else answers") {
			break
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	if state := local.ServiceStatus(t.Context(), sandbox.ServiceMirror); !state.Running {
		t.Errorf("mirror status = %+v, want running", state)
	}
}

// fakeAPIStops is how zone-setup fails against the fake management API,
// which cannot create a provider. A step that gets this far ran zone-setup.
const fakeAPIStops = "create provider: returned HTTP status 405"

// TestSessionZoneIssuer runs the zone step with the issuer that is
// published now. Once the quick tunnel stops, the step refuses for want of
// an issuer.
func TestSessionZoneIssuer(t *testing.T) {
	t.Parallel()
	local := firstRun(t).local
	session := local.NewSession("127.0.0.1:8480", time.Second)
	holdIssuerFile(t, local.QuickIssuerFile(), "https://quick-brown-fox.trycloudflare.com\n")
	// zone-setup takes the issuer, then stops at the fake API.
	err := session.Run(t.Context(), sandbox.StepZone, &bytes.Buffer{})
	checkErr(t, err, []error{sandbox.ErrZoneSetup}, fakeAPIStops)
	if err = os.Remove(local.QuickIssuerFile()); err != nil {
		t.Fatal(err)
	}
	err = session.Run(t.Context(), sandbox.StepZone, &bytes.Buffer{})
	checkErr(t, err, []error{sandbox.ErrIssuer}, "no issuer")
}

// TestSessionReload reloads the settings while other goroutines read what
// runs, as the TUI does. The race detector fails it when a reload changes
// settings that a reader reads.
func TestSessionReload(t *testing.T) {
	t.Parallel()
	local, _ := newLocal(t, sandbox.Settings{}, "")
	local.Store = sandbox.NewStore(sandbox.NoConfig, &sandbox.Config{}, nil)
	session := local.NewSession("127.0.0.1:8480", time.Second)
	var readers sync.WaitGroup
	for range 2 {
		readers.Go(func() {
			for range 20 {
				_ = session.Status(t.Context())
				_ = session.Agents()
			}
		})
	}
	for range 20 {
		session.Reload()
	}
	readers.Wait()
}
