package sandbox

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/keycardai/agent-sandbox-examples/internal/install"
	"github.com/keycardai/agent-sandbox-examples/internal/newsandbox"
	"github.com/keycardai/agent-sandbox-examples/internal/pinned"
	"github.com/keycardai/agent-sandbox-examples/internal/zonesetup"
)

const (
	// zoneTrustConfigMap is the ConfigMap that holds KEYCARD_LOCAL_CA_FILE
	// for the proxy.
	zoneTrustConfigMap = "egress-zone-trust"
	imageRegistry      = "ghcr.io/keycardai/"
)

var (
	// ErrImages means Docker does not have an image the sandbox runs.
	ErrImages = errors.New("images missing")
	// ErrZoneSetup is a failed zone-setup run, or zone-setup output that
	// lacks what the sandbox needs.
	ErrZoneSetup = errors.New("zone-setup")
	// ErrPod is a sandbox pod that did not become ready.
	ErrPod = errors.New("sandbox pod")
)

var (
	zoneURLShape = regexp.MustCompile(`^https://[a-z0-9.-]+$`)
	ipv4         = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$`)
)

// images returns the images the sandbox runs, tagged tag.
func images(tag string) []string {
	return []string{
		imageRegistry + "egress-netsetup:" + tag,
		imageRegistry + "egress-proxy:" + tag,
		imageRegistry + "sandbox-base:" + tag,
	}
}

// Tool returns a command that runs name with args. docker comes from PATH.
// Any other name is the tool mise.toml pins.
func Tool(ctx context.Context, name string, args ...string) (*exec.Cmd, error) {
	if name == "docker" {
		//nolint:gosec // G204: docker is fixed and the caller chooses its arguments.
		return exec.CommandContext(ctx, "docker", args...), nil
	}
	cmd, err := pinned.Command(ctx, name, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return cmd, nil
}

// RunOptions change what Run does with the zone.
type RunOptions struct {
	// SkipZoneSetup reuses local/out/zone-setup.yaml from an earlier run.
	SkipZoneSetup bool
	// ReplaceActive replaces any other active policy set in the zone.
	ReplaceActive bool
}

// runSettings holds the validated settings Run needs.
type runSettings struct {
	cluster, issuerURL, zoneURL, email, caFile string
	zoneOnHost                                 bool
	// imageTag is KEYCARD_LOCAL_IMAGE_TAG. Empty runs the images the charts
	// pin.
	imageTag string
	// clusterCIDRs is set for an existing cluster.
	clusterCIDRs []string
}

// Run runs the sandbox flow end to end: the k3d cluster and the three
// images, zone-setup against the Keycard zone, the install chart, a sandbox
// for KEYCARD_LOCAL_USER_EMAIL, and the checks. It writes to the cluster and
// to the zone.
//
// Run does not publish the issuer. Before the checks run, start the mirror,
// and the tunnel or your own proxy that forwards the issuer's https URL to
// the mirror.
//
// The sandbox pod dials the zone at the address its host name resolves to.
// With KEYCARD_LOCAL_ZONE_ON_HOST=true, it dials the gateway of the
// cluster's Docker network instead, for a zone that answers on the Docker
// host.
//
// The pod runs the images the charts pin by digest, and the cluster pulls
// them from ghcr.io/keycardai. With KEYCARD_LOCAL_IMAGE_TAG set, the pod
// runs the ghcr.io/keycardai images at that tag instead, and for k3d, Run
// imports them from Docker.
//
// For an existing cluster, Run does not create the cluster or import
// images. The issuer comes from the cluster's discovery document.
func (l *Local) Run(ctx context.Context, opts RunOptions) error {
	cfg, err := l.runSettings(ctx)
	if err != nil {
		return err
	}
	if !l.Existing() {
		if err = l.checkDocker(ctx); err != nil {
			return err
		}
	}
	if err = os.MkdirAll(l.outDir(), dirMode); err != nil {
		return fmt.Errorf("creating %s: %w", l.outDir(), err)
	}
	if l.Existing() {
		l.step("Using the existing cluster " + cfg.cluster + " with issuer " + cfg.issuerURL)
	} else if err = l.k3dCluster(ctx, cfg); err != nil {
		return err
	}
	out, err := l.zoneSetup(ctx, cfg, opts)
	if err != nil {
		return err
	}
	access, err := userAccess(out, cfg.email)
	if err != nil {
		return err
	}
	return l.deploy(ctx, cfg, out, access)
}

// deploy installs the chart and the user's sandbox from the zone-setup
// output, waits for the pod, and runs the checks.
func (l *Local) deploy(ctx context.Context, cfg runSettings, out *zonesetup.Output, access zonesetup.UserAccess) error {
	namespace := out.K8sSandbox.Namespace
	l.step("Writing the Helm values for this cluster")
	if err := l.writeLocalValues(ctx, cfg); err != nil {
		return err
	}
	l.step("Installing the agent-sandbox controller and the keycard-sandbox chart into " + namespace)
	if err := l.install(ctx, cfg.cluster, namespace); err != nil {
		return err
	}
	l.step("Creating the sandbox for " + cfg.email + " (user " + access.UserID + ")")
	if err := l.newSandbox(ctx, cfg.cluster, out, access); err != nil {
		return err
	}
	l.step("Waiting for the sandbox pod to become ready")
	if err := l.waitForPod(ctx, cfg.cluster, namespace, access.UserID); err != nil {
		return err
	}
	l.step("Running the end-to-end checks")
	return l.Check(
		ctx,
		CheckTarget{Cluster: cfg.cluster, Namespace: namespace, Pod: access.UserID, UserID: access.UserID},
	)
}

// LocalValuesInput is what LocalValues writes into the values files.
type LocalValuesInput struct {
	// ZoneURL is the zone the proxy dials.
	ZoneURL string
	// Gateway, when set, is the Docker host's address in the cluster: the
	// zone's host maps to it, the NetworkPolicy allows it on 443, and the
	// proxy may dial it.
	Gateway string
	// CAPEM, when set, holds the roots the proxy trusts for the zone in
	// addition to the system roots.
	CAPEM []byte
	// ImageTag, when set, is the tag of the ghcr.io/keycardai images the pod
	// runs. Empty leaves the images the charts pin.
	ImageTag string
	// ClusterCIDRs, when set, are the cluster's pod and service ranges.
	ClusterCIDRs []string
}

// LocalValues returns the values files for the install chart and the
// sandbox chart that point a pod at the zone.
func LocalValues(input LocalValuesInput) ([]byte, []byte, error) {
	type hostAlias struct {
		IP        string   `json:"ip"`
		Hostnames []string `json:"hostnames"`
	}
	type zoneTrust struct {
		ConfigMap   string `json:"configMap"`
		Certificate string `json:"certificate,omitempty"`
	}
	type cidrs struct {
		ClusterCIDRs       []string `json:"clusterCIDRs,omitempty"`
		AllowCIDRs         []string `json:"allowCIDRs,omitempty"`
		DenyListExceptions []string `json:"denyListExceptions,omitempty"`
	}
	type imageValues struct {
		Netsetup string `json:"netsetup"`
		Proxy    string `json:"proxy"`
		Agent    string `json:"agent"`
	}
	type values struct {
		ZoneURL     string       `json:"zoneURL"`
		ZoneTrust   *zoneTrust   `json:"zoneTrust,omitempty"`
		HostAliases []hostAlias  `json:"hostAliases,omitempty"`
		Images      *imageValues `json:"images,omitempty"`
		Network     *cidrs       `json:"network,omitempty"`
		Policy      *cidrs       `json:"policy,omitempty"`
	}
	install, sandbox := values{ZoneURL: input.ZoneURL}, values{ZoneURL: input.ZoneURL}
	install.Network, sandbox.Policy = &cidrs{ClusterCIDRs: input.ClusterCIDRs}, &cidrs{ClusterCIDRs: input.ClusterCIDRs}
	if len(input.CAPEM) > 0 {
		install.ZoneTrust = &zoneTrust{ConfigMap: zoneTrustConfigMap, Certificate: string(input.CAPEM)}
		sandbox.ZoneTrust = &zoneTrust{ConfigMap: zoneTrustConfigMap}
	}
	if input.ImageTag != "" {
		tagged := &imageValues{
			Netsetup: imageRegistry + "egress-netsetup:" + input.ImageTag,
			Proxy:    imageRegistry + "egress-proxy:" + input.ImageTag,
			Agent:    imageRegistry + "sandbox-base:" + input.ImageTag,
		}
		install.Images, sandbox.Images = tagged, tagged
	}
	if input.Gateway != "" {
		host := strings.TrimPrefix(input.ZoneURL, "https://")
		aliases := []hostAlias{{IP: input.Gateway, Hostnames: []string{host}}}
		install.HostAliases, sandbox.HostAliases = aliases, aliases
		install.Network.AllowCIDRs = []string{input.Gateway + "/32"}
		// The gateway is in a private range, which is on the proxy's
		// platform deny list. The exception lets the proxy dial the zone.
		sandbox.Policy.DenyListExceptions = []string{input.Gateway + "/32"}
	}
	if len(input.ClusterCIDRs) == 0 && input.Gateway == "" {
		install.Network, sandbox.Policy = nil, nil
	}
	installYAML, err := yaml.Marshal(install)
	if err != nil {
		return nil, nil, fmt.Errorf("encoding the install values: %w", err)
	}
	sandboxYAML, err := yaml.Marshal(sandbox)
	if err != nil {
		return nil, nil, fmt.Errorf("encoding the sandbox values: %w", err)
	}
	return installYAML, sandboxYAML, nil
}

// k3dCluster creates or checks the k3d cluster, and imports the images
// when KEYCARD_LOCAL_IMAGE_TAG is set.
func (l *Local) k3dCluster(ctx context.Context, cfg runSettings) error {
	l.step("Creating or checking the k3d cluster " + cfg.cluster)
	if err := l.Bootstrap(ctx, cfg.cluster, cfg.issuerURL, false); err != nil {
		return err
	}
	if cfg.imageTag == "" {
		return nil
	}
	l.step("Importing the three images into the cluster")
	return l.importImages(ctx, cfg.cluster, cfg.imageTag)
}

// runSettings validates local.env before it reads the issuer. For an
// existing cluster, reading the issuer is the first call to the cluster.
func (l *Local) runSettings(ctx context.Context) (runSettings, error) {
	var cfg runSettings
	var err error
	if cfg.cluster, err = l.cluster(""); err != nil {
		return cfg, err
	}
	cfg.zoneURL, cfg.email = l.Settings[KeyZoneURL], l.Settings[KeyUserEmail]
	if !zoneURLShape.MatchString(cfg.zoneURL) {
		return cfg, fmt.Errorf("%w: %s must be https://<zone host> (got %q). Set it to the URL of an existing zone",
			ErrSettings, KeyZoneURL, cfg.zoneURL)
	}
	if cfg.email == "" {
		return cfg, fmt.Errorf("%w: %s is empty. Set it to the email of a user in zone-setup.yaml",
			ErrSettings, KeyUserEmail)
	}
	switch onHost := l.Settings[KeyZoneOnHost]; onHost {
	case "":
	case settingTrue:
		cfg.zoneOnHost = true
	default:
		return cfg, fmt.Errorf("%w: %s must be true or empty (got %q)", ErrSettings, KeyZoneOnHost, onHost)
	}
	if cfg.caFile, err = l.caFile(); err != nil {
		return cfg, err
	}
	cfg.imageTag = l.Settings[KeyImageTag]
	if cfg, err = l.existingSettings(cfg); err != nil {
		return cfg, err
	}
	cfg.issuerURL, err = l.clusterIssuer(ctx, cfg.cluster, "")
	return cfg, err
}

// existingSettings adds the cluster's ranges for an existing cluster, and
// returns cfg unchanged for k3d. KEYCARD_LOCAL_ZONE_ON_HOST works only with
// k3d.
func (l *Local) existingSettings(cfg runSettings) (runSettings, error) {
	if !l.Existing() {
		return cfg, nil
	}
	if cfg.zoneOnHost {
		return cfg, fmt.Errorf("%w: %s is for a k3d cluster", ErrExisting, KeyZoneOnHost)
	}
	var err error
	if cfg.clusterCIDRs, err = ParseCIDRs(l.Settings[KeyClusterCIDRs]); err != nil {
		return cfg, err
	}
	if len(cfg.clusterCIDRs) == 0 {
		return cfg, fmt.Errorf("%w: %s is empty. Set it to the cluster's pod and service ranges",
			ErrSettings, KeyClusterCIDRs)
	}
	return cfg, nil
}

// checkDocker runs docker version. It fails when the docker CLI is not on
// PATH or cannot connect to the Docker daemon.
func (l *Local) checkDocker(ctx context.Context) error {
	cmd, err := l.Tool(ctx, "docker", "version", "--format", "{{.Server.Version}}")
	if err != nil {
		return err
	}
	if _, err = pinned.Run(cmd, nil); err != nil {
		return fmt.Errorf("%w: %w. Install Docker, and start OrbStack or Docker Desktop", ErrDocker, err)
	}
	return nil
}

// ParseCIDRs parses a comma-separated list of address ranges. Each one must
// be a prefix such as 10.0.0.0/16.
func ParseCIDRs(list string) ([]string, error) {
	var out []string
	for item := range strings.SplitSeq(list, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(item)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %q is not an address range such as 10.0.0.0/16",
				ErrSettings, KeyClusterCIDRs, item)
		}
		out = append(out, prefix.Masked().String())
	}
	return out, nil
}

// importImages imports the three images, tagged tag, from Docker into the
// cluster. It fails before the import if Docker does not have one of them.
// A tag other than latest defaults imagePullPolicy to IfNotPresent, so the
// node runs the imported images and does not pull.
func (l *Local) importImages(ctx context.Context, cluster, tag string) error {
	var missing []string
	for _, image := range images(tag) {
		cmd, err := l.Tool(ctx, "docker", "image", "inspect", image)
		if err != nil {
			return err
		}
		if _, err = pinned.Run(cmd, nil); err != nil {
			missing = append(missing, image)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: %s. Build them in the egress-proxy repository and tag them with these names",
			ErrImages, strings.Join(missing, ", "))
	}
	if err := l.k3d(
		ctx,
		l.Out,
		slices.Concat([]string{"image", "import", "--cluster", cluster}, images(tag))...,
	); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(l.Out, "Imported %d images into the k3d cluster %s.\n", len(images(tag)), cluster)
	return nil
}

// zoneSetup runs zone-setup and writes its output to local/out/zone-setup.yaml,
// or reads that file when opts.SkipZoneSetup is set.
func (l *Local) zoneSetup(ctx context.Context, cfg runSettings, opts RunOptions) (*zonesetup.Output, error) {
	outFile := filepath.Join(l.outDir(), "zone-setup.yaml")
	if opts.SkipZoneSetup {
		return l.readZoneSetup()
	}
	l.step("Running zone-setup against " + cfg.zoneURL)
	valuesFile := filepath.Join(l.Root, cmp.Or(l.Settings[KeyZoneSetupValues], "zone-setup.yaml"))
	if _, err := os.Stat(valuesFile); err != nil {
		return nil, fmt.Errorf("%w: %s does not exist. Copy zone-setup.example.yaml to it and edit it",
			ErrZoneSetup, valuesFile)
	}
	values, err := zonesetup.ReadValuesFile(valuesFile, cfg.issuerURL, "the cluster's issuer")
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrZoneSetup, err)
	}
	secrets, err := zonesetup.LoadSecrets(values, os.LookupEnv, os.ReadFile)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrZoneSetup, valuesFile, err)
	}
	roots, err := rootsFrom(cfg.caFile)
	if err != nil {
		return nil, err
	}
	out, err := zonesetup.Run(ctx, values, secrets, &zonesetup.Options{
		ReplaceActive: opts.ReplaceActive, Log: l.Out, Roots: roots,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrZoneSetup, err)
	}
	raw, err := yaml.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("encoding the zone-setup output: %w", err)
	}
	if err = os.WriteFile(outFile, raw, fileMode); err != nil {
		return nil, fmt.Errorf("writing %s: %w", outFile, err)
	}
	_, _ = fmt.Fprintln(l.Out, "Wrote local/out/zone-setup.yaml.")
	return out, nil
}

// readZoneSetup reads the zone-setup output an earlier run wrote. It refuses
// a field it does not know and an empty field the sandbox needs.
func (l *Local) readZoneSetup() (*zonesetup.Output, error) {
	outFile := filepath.Join(l.outDir(), "zone-setup.yaml")
	raw, err := os.ReadFile(outFile) //nolint:gosec // G304: the path is under this checkout's local/out.
	if err != nil || len(raw) == 0 {
		return nil, fmt.Errorf("%w: %s is missing or empty. Run without --skip-zone-setup", ErrZoneSetup, outFile)
	}
	var out zonesetup.Output
	if err = yaml.UnmarshalStrict(raw, &out); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrZoneSetup, outFile, err)
	}
	if key := missingField(&out); key != "" {
		return nil, fmt.Errorf("%w: %s has no %s. Run without --skip-zone-setup", ErrZoneSetup, outFile, key)
	}
	return &out, nil
}

// missingField returns the YAML key of the first empty field in out, or "".
func missingField(out *zonesetup.Output) string {
	type field struct{ key, value string }
	for _, required := range []field{
		{"k8s_sandbox.application_id", out.K8sSandbox.ApplicationID},
		{"k8s_sandbox.application_identifier", out.K8sSandbox.ApplicationIdentifier},
		{"k8s_sandbox.namespace", out.K8sSandbox.Namespace},
		{"k8s_sandbox.provider_id", out.K8sSandbox.ProviderID},
	} {
		if required.value == "" {
			return required.key
		}
	}
	for i, access := range out.SandboxUserAccess {
		for _, required := range []field{
			{"user_email", access.UserEmail},
			{"user_id", access.UserID},
			{"service_account", access.ServiceAccount},
			{"client_id", access.ClientID},
			{"namespace", access.Namespace},
		} {
			if required.value == "" {
				return fmt.Sprintf("sandbox_user_access[%d].%s", i, required.key)
			}
		}
	}
	return ""
}

// userAccess returns the zone-setup output for email.
func userAccess(out *zonesetup.Output, email string) (zonesetup.UserAccess, error) {
	for _, access := range out.SandboxUserAccess {
		if access.UserEmail == email {
			return access, nil
		}
	}
	return zonesetup.UserAccess{}, fmt.Errorf(
		"%w: %s is not in the zone-setup output. Add it to users in zone-setup.yaml", ErrZoneSetup, email,
	)
}

// writeLocalValues writes the values files LocalValues returns to local/out.
func (l *Local) writeLocalValues(ctx context.Context, cfg runSettings) error {
	gateway := ""
	if cfg.zoneOnHost {
		var err error
		if gateway, err = l.gateway(ctx, cfg.cluster); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(l.Out, "    %s maps to %s, the k3d-%s gateway\n",
			strings.TrimPrefix(cfg.zoneURL, "https://"), gateway, cfg.cluster)
	}
	var caPEM []byte
	if cfg.caFile != "" {
		var err error
		if caPEM, err = os.ReadFile(cfg.caFile); err != nil {
			return fmt.Errorf("%w: %w", ErrCAFile, err)
		}
	}
	install, sandbox, err := LocalValues(LocalValuesInput{
		ZoneURL: cfg.zoneURL, Gateway: gateway, CAPEM: caPEM, ImageTag: cfg.imageTag, ClusterCIDRs: cfg.clusterCIDRs,
	})
	if err != nil {
		return err
	}
	for name, body := range map[string][]byte{"install.values.yaml": install, "sandbox.values.yaml": sandbox} {
		if err = os.WriteFile(filepath.Join(l.outDir(), name), body, fileMode); err != nil {
			return fmt.Errorf("writing %s: %w", name, err)
		}
	}
	_, _ = fmt.Fprintln(l.Out, "Wrote local/out/install.values.yaml and local/out/sandbox.values.yaml.")
	return nil
}

// gateway returns the first IPv4 gateway of the cluster's Docker network,
// which is the Docker host's address inside the cluster. A recreated cluster
// can get another subnet, so Run reads it every time.
func (l *Local) gateway(ctx context.Context, cluster string) (string, error) {
	cmd, err := l.Tool(ctx, "docker", "network", "inspect", "k3d-"+cluster,
		"--format", "{{range .IPAM.Config}}{{println .Gateway}}{{end}}")
	if err != nil {
		return "", err
	}
	out, err := pinned.Run(cmd, nil)
	if err == nil {
		for line := range strings.Lines(string(out)) {
			if line = strings.TrimSpace(line); ipv4.MatchString(line) {
				return line, nil
			}
		}
	}
	return "", fmt.Errorf("%w: the Docker network k3d-%s has no IPv4 gateway. Does the cluster exist?",
		ErrDocker, cluster)
}

// install installs the controller and the chart with the local values.
func (l *Local) install(ctx context.Context, cluster, namespace string) error {
	installer := &install.Install{
		Root:      l.Root,
		Context:   l.kubeContext(cluster),
		Namespace: namespace,
		Values: []string{
			filepath.Join(l.Root, "local", "values", "install.yaml"),
			filepath.Join(l.outDir(), "install.values.yaml"),
		},
		Tool: l.Tool,
		Out:  l.Out,
	}
	if err := installer.Run(ctx); err != nil {
		return fmt.Errorf("installing into %s: %w", namespace, err)
	}
	return nil
}

// newSandbox renders the sandbox chart for one user, applies it, and writes
// the manifests to local/out/sandbox.yaml.
func (l *Local) newSandbox(
	ctx context.Context,
	cluster string,
	out *zonesetup.Output,
	access zonesetup.UserAccess,
) error {
	sandbox := &newsandbox.Command{
		Inputs: newsandbox.Inputs{
			Namespace:             out.K8sSandbox.Namespace,
			ProviderID:            out.K8sSandbox.ProviderID,
			ApplicationID:         out.K8sSandbox.ApplicationID,
			ApplicationIdentifier: out.K8sSandbox.ApplicationIdentifier,
			UserEmail:             access.UserEmail,
			UserID:                access.UserID,
			ServiceAccount:        access.ServiceAccount,
			ClientID:              access.ClientID,
		},
		ChartDir: filepath.Join(l.Root, "sandbox"),
		ValuesFiles: []string{
			filepath.Join(l.Root, "sandbox", "values.example.yaml"),
			filepath.Join(l.Root, "local", "values", "sandbox.yaml"),
			filepath.Join(l.outDir(), "sandbox.values.yaml"),
		},
		Apply: true,
		Kube: func(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
			return l.kubectl(ctx, cluster, stdin, args...)
		},
		Log: l.Out,
	}
	manifests, err := sandbox.Run(ctx)
	if err != nil {
		return fmt.Errorf("new-sandbox: %w", err)
	}
	if err = os.WriteFile(filepath.Join(l.outDir(), "sandbox.yaml"), manifests, fileMode); err != nil {
		return fmt.Errorf("writing sandbox.yaml: %w", err)
	}
	_, _ = fmt.Fprintln(l.Out, "Wrote the applied manifests to local/out/sandbox.yaml.")
	return nil
}

// waitForPod waits for the sandbox pod to exist and become Ready. If the pod
// does not become Ready, it prints the last 40 lines of each container's log.
func (l *Local) waitForPod(ctx context.Context, cluster, namespace, pod string) error {
	_, err := l.kubectl(ctx, cluster, nil, "--namespace", namespace,
		"wait", "--for=create", "pod/"+pod, "--timeout=120s")
	if err != nil {
		return fmt.Errorf("%w: %w", ErrPod, err)
	}
	_, err = l.kubectl(ctx, cluster, nil, "--namespace", namespace,
		"wait", "--for=condition=Ready", "pod/"+pod, "--timeout=300s")
	if err == nil {
		return nil
	}
	for _, container := range []string{"egress-netsetup", "egress-proxy", "agent"} {
		logs, _ := l.kubectl(ctx, cluster, nil, "--namespace", namespace, "logs", pod, "-c", container, "--tail=40")
		_, _ = fmt.Fprintf(l.Out, "--- %s\n%s", container, logs)
	}
	return fmt.Errorf("%w: %s did not become ready: %w", ErrPod, pod, err)
}

func (l *Local) outDir() string {
	return filepath.Join(l.Root, "local", "out")
}

func (l *Local) step(title string) {
	_, _ = fmt.Fprintf(l.Out, "\n==> %s\n", title)
}
