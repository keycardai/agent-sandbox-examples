package chart_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path"
	"slices"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// `egress-proxy settings` prints the binary's defaults as JSON. It does not
// need a document or a socket. docs/containers.md in keycardai/egress-proxy
// describes the report. The tests in this file check the rendered pods and
// documents against the report.
//
// The pod and the binary must agree on each value checked here. The values
// are in different objects: init container arguments, a container
// securityContext, a pod sysctl, probe commands and a ConfigMap document.
// When they differ, the sidecar refuses to start.

// egressProxyEnv names an egress-proxy binary built from
// keycardai/egress-proxy. The tests skip when it is unset.
const egressProxyEnv = "EGRESS_PROXY"

// settingsSchema is the report format this file reads. The report keeps the
// number when it adds a field, and raises it when a field changes meaning or
// is removed.
const settingsSchema = 1

// portStartSysctl is the lowest port a process without
// CAP_NET_BIND_SERVICE may bind.
const portStartSysctl = "net.ipv4.ip_unprivileged_port_start"

// settings is the part of the report a pod must agree with. The decode is
// not strict, because the report grows.
type settings struct {
	Schema    int              `json:"schema"`
	EnvPrefix string           `json:"env_prefix"`
	Settings  []settingsEntry  `json:"settings"`
	Probe     settingsProbe    `json:"probe"`
	Netsetup  settingsNetsetup `json:"netsetup"`
}

// settingsEntry is one setting. A null default is a setting the parser
// leaves empty.
type settingsEntry struct {
	Env     string `json:"env"`
	Flag    string `json:"flag"`
	Default any    `json:"default"`
}

type settingsProbe struct {
	Subcommand string   `json:"subcommand"`
	Routes     []string `json:"routes"`
}

// settingsNetsetup holds the values egress-netsetup programs. egress-netsetup
// reads only flags, so the chart sets each one.
type settingsNetsetup struct {
	ProxyUID     int64  `json:"proxy_uid"`
	RedirectPort int    `json:"redirect_port"`
	CapturePorts []int  `json:"capture_ports"`
	ReceiptPath  string `json:"receipt_path"`
}

// defaultPath returns the default of the setting named env. The test fails
// when the report does not list it, because the chart relies on that default.
func (s settings) defaultPath(t *testing.T, env string) string {
	t.Helper()
	for _, entry := range s.Settings {
		if entry.Env != env {
			continue
		}
		value, ok := entry.Default.(string)
		if !ok {
			t.Fatalf("%s has default %v, want a path", env, entry.Default)
		}
		return value
	}
	t.Fatalf("`egress-proxy settings` does not list %s", env)
	return ""
}

// readSettings runs the binary EGRESS_PROXY names and parses the report.
// NEVER fall back to egress-proxy on PATH. The developer chooses the binary.
func readSettings(t *testing.T) settings {
	t.Helper()
	binary := os.Getenv(egressProxyEnv)
	if binary == "" {
		t.Skipf("%s is not set. Build a binary from keycardai/egress-proxy with "+
			"`go build -o <path> ./cmd/egress-proxy` and set %s to its path", egressProxyEnv, egressProxyEnv)
	}
	//nolint:gosec // G204: the binary is the one EGRESS_PROXY names, and the developer chooses it.
	out, err := exec.CommandContext(t.Context(), binary, "settings").Output()
	if err != nil {
		t.Fatalf("%s settings: %v", binary, err)
	}
	var report settings
	if err = json.Unmarshal(out, &report); err != nil {
		t.Fatalf("%s settings printed something this test cannot parse: %v\n%s", binary, err, out)
	}
	if report.Schema != settingsSchema {
		t.Fatalf("%s settings reports schema %d, want %d. Read docs/containers.md in "+
			"keycardai/egress-proxy and update this file", binary, report.Schema, settingsSchema)
	}
	return report
}

// TestSettingsAgreement checks the install chart's pod against the report,
// with the example values and with the local values. assertPodsMatch checks
// that the sandbox chart renders the same pod, so this test does not render
// the sandbox chart.
func TestSettingsAgreement(t *testing.T) {
	t.Parallel()
	report := readSettings(t)
	cases := []struct {
		name    string
		objects func(*testing.T) map[string][]byte
	}{
		{"example values", func(t *testing.T) map[string][]byte { t.Helper(); return render(t) }},
		{"local values", func(t *testing.T) map[string][]byte { t.Helper(); return renderLocalInstall(t, true) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var tmpl sandboxTemplate
			decode(t, tc.objects(t), "SandboxTemplate/"+release, &tmpl)
			spec := tmpl.Spec.PodTemplate.Spec
			assertNetsetupArgs(t, report, spec)
			assertProxyUID(t, report, spec)
			assertRedirectPortIsPrivileged(t, report, spec)
			assertProbeSubcommand(t, report, spec)
			assertDefaultPathsAreMounted(t, report, spec)
		})
	}
}

func container(t *testing.T, containers []corev1.Container, name string) corev1.Container {
	t.Helper()
	i := slices.IndexFunc(containers, func(c corev1.Container) bool { return c.Name == name })
	if i < 0 {
		t.Fatalf("no %s container", name)
	}
	return containers[i]
}

// argValue returns the value of the first argument with the given --flag=
// prefix, or the empty string.
func argValue(args []string, prefix string) string {
	for _, arg := range args {
		if after, found := strings.CutPrefix(arg, prefix); found {
			return after
		}
	}
	return ""
}

// assertNetsetupArgs checks the init container's arguments. The proxy
// compares the receipt with its own values and refuses to start when they
// differ.
func assertNetsetupArgs(t *testing.T, report settings, spec corev1.PodSpec) {
	t.Helper()
	netsetup := container(t, spec.InitContainers, "egress-netsetup")
	ports := make([]string, 0, len(report.Netsetup.CapturePorts))
	for _, port := range report.Netsetup.CapturePorts {
		ports = append(ports, strconv.Itoa(port))
	}
	cases := []struct{ flag, want string }{
		{"--proxy-uid=", strconv.FormatInt(report.Netsetup.ProxyUID, 10)},
		{"--redirect-port=", strconv.Itoa(report.Netsetup.RedirectPort)},
		{"--capture-ports=", strings.Join(ports, ",")},
		{"--receipt=", report.Netsetup.ReceiptPath},
	}
	for _, tc := range cases {
		if got := argValue(netsetup.Args, tc.flag); got != tc.want {
			t.Errorf("egress-netsetup %s%s, want %s%s", tc.flag, got, tc.flag, tc.want)
		}
	}
}

// assertProxyUID checks that the proxy runs as the uid the capture exempts.
// Under any other uid, the redirect captures the proxy's own upstream
// traffic.
func assertProxyUID(t *testing.T, report settings, spec corev1.PodSpec) {
	t.Helper()
	sc := container(t, spec.InitContainers, "egress-proxy").SecurityContext
	if sc == nil || sc.RunAsUser == nil {
		t.Fatal("the egress-proxy container does not set runAsUser")
	}
	if *sc.RunAsUser != report.Netsetup.ProxyUID {
		t.Errorf("egress-proxy runAsUser is %d, want proxy_uid %d", *sc.RunAsUser, report.Netsetup.ProxyUID)
	}
}

// assertRedirectPortIsPrivileged checks that the pod sets the sysctl above
// the redirect port. The agent does not hold CAP_NET_BIND_SERVICE, so it
// cannot bind the redirect port while the sidecar restarts.
func assertRedirectPortIsPrivileged(t *testing.T, report settings, spec corev1.PodSpec) {
	t.Helper()
	if spec.SecurityContext == nil {
		t.Fatal("the pod has no securityContext")
	}
	i := slices.IndexFunc(spec.SecurityContext.Sysctls, func(s corev1.Sysctl) bool { return s.Name == portStartSysctl })
	if i < 0 {
		t.Fatalf("the pod does not set %s", portStartSysctl)
	}
	start, err := strconv.Atoi(spec.SecurityContext.Sysctls[i].Value)
	if err != nil {
		t.Fatalf("%s is %q, which is not a port number", portStartSysctl, spec.SecurityContext.Sysctls[i].Value)
	}
	if start <= report.Netsetup.RedirectPort {
		t.Errorf("%s is %d, want it above redirect_port %d", portStartSysctl, start, report.Netsetup.RedirectPort)
	}
}

// assertProbeSubcommand checks each probe against the subcommand and the
// routes the binary serves. A probe of any other route fails, and the agent
// does not start.
func assertProbeSubcommand(t *testing.T, report settings, spec corev1.PodSpec) {
	t.Helper()
	proxy := container(t, spec.InitContainers, "egress-proxy")
	cases := []struct {
		name  string
		probe *corev1.Probe
	}{
		{"startupProbe", proxy.StartupProbe},
		{"livenessProbe", proxy.LivenessProbe},
		{"readinessProbe", proxy.ReadinessProbe},
	}
	for _, tc := range cases {
		if tc.probe == nil || tc.probe.Exec == nil || len(tc.probe.Exec.Command) != 3 {
			t.Errorf("egress-proxy %s is not an exec probe of binary, subcommand, route: %+v", tc.name, tc.probe)
			continue
		}
		command := tc.probe.Exec.Command
		if command[1] != report.Probe.Subcommand {
			t.Errorf("egress-proxy %s runs the subcommand %q, want %q", tc.name, command[1], report.Probe.Subcommand)
		}
		if !slices.Contains(report.Probe.Routes, command[2]) {
			t.Errorf("egress-proxy %s reads the route %q, which is not in %v", tc.name, command[2], report.Probe.Routes)
		}
	}
}

// assertDefaultPathsAreMounted checks the three paths the chart leaves to
// the binary: the admin socket, the netsetup receipt and the CA output
// directory. Each default is a directory inside a volume, never the mount
// point. The kubelet creates the mount point owned by root with mode 0777,
// and the proxy cannot chmod it. Every container that uses a path must mount
// a volume above it.
func assertDefaultPathsAreMounted(t *testing.T, report settings, spec corev1.PodSpec) {
	t.Helper()
	netsetup := container(t, spec.InitContainers, "egress-netsetup")
	proxy := container(t, spec.InitContainers, "egress-proxy")
	agent := container(t, spec.Containers, "agent")

	adminDir := path.Dir(report.defaultPath(t, report.EnvPrefix+"ADMIN"))
	if _, ok := mountHolding(proxy, adminDir); !ok {
		t.Errorf("the proxy does not mount a volume above the admin socket directory %s", adminDir)
	}

	receiptDir := path.Dir(report.Netsetup.ReceiptPath)
	for _, ctr := range []corev1.Container{netsetup, proxy} {
		if _, ok := mountHolding(ctr, receiptDir); !ok {
			t.Errorf("%s does not mount a volume above the receipt directory %s", ctr.Name, receiptDir)
		}
	}

	// KEYCARD_CA_FILE in the sandbox-base image defaults to ca.crt in this
	// directory, so the agent must mount the proxy's volume there.
	caOutDir := report.defaultPath(t, report.EnvPrefix+"CA_OUT_DIR")
	volume, ok := mountHolding(proxy, caOutDir)
	if !ok {
		t.Fatalf("the proxy does not mount a volume above ca.out_dir %s", caOutDir)
	}
	if agentVolume, agentOK := mountHolding(agent, caOutDir); !agentOK || agentVolume != volume {
		t.Errorf("the agent reads %s/ca.crt from volume %q, want the proxy's %q", caOutDir, agentVolume, volume)
	}

	assertProxyTakesDefaults(t, report, proxy)
}

// mountHolding returns the volume the container mounts at a strict ancestor
// of dir. A mount at dir itself does not count, because the proxy must create
// dir to own it.
func mountHolding(ctr corev1.Container, dir string) (string, bool) {
	for _, mount := range ctr.VolumeMounts {
		if strings.HasPrefix(dir, strings.TrimSuffix(mount.MountPath, "/")+"/") {
			return mount.Name, true
		}
	}
	return "", false
}

// assertProxyTakesDefaults checks that the proxy container passes --config
// and no other flag or variable. Either would override a default, and this
// file would then compare the pod with a value the proxy does not use.
func assertProxyTakesDefaults(t *testing.T, report settings, proxy corev1.Container) {
	t.Helper()
	if len(proxy.Command) != 0 {
		t.Errorf("the proxy container replaces the entrypoint with %v", proxy.Command)
	}
	for _, arg := range proxy.Args {
		if !strings.HasPrefix(arg, "--config=") {
			t.Errorf("the proxy container passes %s, want --config alone", arg)
		}
	}
	for _, env := range proxy.Env {
		if strings.HasPrefix(env.Name, report.EnvPrefix) {
			t.Errorf("the proxy container sets %s, want no %s variable", env.Name, report.EnvPrefix)
		}
	}
}

// TestSettingsTransparentPort checks that listen.transparent_port in the
// document is the port the capture redirects to. Only the sandbox chart
// renders a document.
func TestSettingsTransparentPort(t *testing.T) {
	t.Parallel()
	report := readSettings(t)
	cases := []struct {
		name    string
		objects func(*testing.T) map[string][]byte
	}{
		{"example values", renderSandbox},
		{"local values", func(t *testing.T) map[string][]byte { t.Helper(); return renderLocalSandbox(t, true) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			doc := parseDocument(t, configDocument(t, tc.objects(t)))
			if doc.Listen.TransparentPort != report.Netsetup.RedirectPort {
				t.Errorf("listen.transparent_port is %d, want redirect_port %d",
					doc.Listen.TransparentPort, report.Netsetup.RedirectPort)
			}
		})
	}
}
