package sandbox

import (
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
	"time"

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
	// namespaceFlag is kubectl's flag for the namespace.
	namespaceFlag = "--namespace"
	imageRegistry = "ghcr.io/keycardai/"
)

var (
	// ErrImages means Docker does not have an image the sandbox runs, or the
	// install chart does not name one.
	ErrImages = errors.New("images missing")
	// ErrBuild means a demo's image did not build from its Dockerfile.
	ErrBuild = errors.New("the demo's image did not build")
	// ErrZoneSetup is a failed zone-setup run, or zone-setup output that
	// lacks what the sandbox needs.
	ErrZoneSetup = errors.New("zone-setup")
	// ErrPod is a sandbox pod that did not become ready.
	ErrPod = errors.New("sandbox pod")
)

var (
	imageToken   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9./:@_-]*$`)
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
	scenario     *Scenario
	// scenarioValues is the scenario's values file for the install chart,
	// with the answers for email.
	scenarioValues []byte
	// answers are the answers each agent's values file is made from.
	answers scenarioAnswers
}

// Run runs the sandbox flow end to end: the k3d cluster and the images,
// zone-setup against the Keycard zone, the install chart, a sandbox for
// each user in the values file that runs the agent of
// KEYCARD_LOCAL_SCENARIO, and the checks in each. It deletes the sandboxes
// of users no longer in the values file. It writes to the cluster and to
// the zone.
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
// images. The issuer comes from the cluster's discovery document. Run
// requires demo.images to name a prebuilt registry image for a scenario
// with a Dockerfile there.
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
	if _, err = userAccess(out, cfg.email); err != nil {
		return err
	}
	return l.deploy(ctx, cfg, out)
}

// deploy installs the chart and each user's sandbox from the zone-setup
// output, waits for each pod, and runs the checks in each.
func (l *Local) deploy(ctx context.Context, cfg runSettings, out *zonesetup.Output) error {
	namespace := out.K8sSandbox.Namespace
	l.step("Writing the Helm values for this cluster")
	if err := l.writeLocalValues(ctx, cfg); err != nil {
		return err
	}
	l.step("Installing the agent-sandbox controller and the keycard-sandbox chart into " + namespace)
	if err := l.install(ctx, cfg.cluster, namespace); err != nil {
		return err
	}
	if err := l.createSandboxes(ctx, cfg, out); err != nil {
		return err
	}
	for _, access := range out.SandboxUserAccess {
		if err := l.waitForAgent(ctx, cfg.cluster, out, access); err != nil {
			return err
		}
	}
	return l.checkAgents(ctx, cfg.cluster, out, out.SandboxUserAccess)
}

// createSandboxes creates or updates the sandbox of each user in the
// zone-setup output, and deletes the sandboxes of users no longer in it.
func (l *Local) createSandboxes(ctx context.Context, cfg runSettings, out *zonesetup.Output) error {
	for _, access := range out.SandboxUserAccess {
		if err := l.createSandbox(ctx, cfg, out, access); err != nil {
			return err
		}
	}
	return l.pruneSandboxes(ctx, cfg.cluster, out.K8sSandbox.Namespace, out.SandboxUserAccess)
}

// createSandbox creates or updates one user's sandbox, and replaces its pod
// when it no longer matches the sandbox.
func (l *Local) createSandbox(ctx context.Context, cfg runSettings, out *zonesetup.Output,
	access zonesetup.UserAccess,
) error {
	namespace := out.K8sSandbox.Namespace
	l.step("Creating the sandbox for " + access.UserEmail + " (user " + access.UserID + ")")
	generation := l.sandboxGeneration(ctx, cfg.cluster, namespace, access.UserID)
	if err := l.newSandbox(ctx, cfg, out, access); err != nil {
		return err
	}
	return l.replaceStalePod(ctx, cfg.cluster, namespace, access.UserID, generation)
}

// waitForAgent waits for one user's sandbox pod.
func (l *Local) waitForAgent(ctx context.Context, cluster string, out *zonesetup.Output,
	access zonesetup.UserAccess,
) error {
	l.step("Waiting for the pod of " + access.UserEmail + "'s agent to become ready")
	return l.waitForPod(ctx, cluster, out.K8sSandbox.Namespace, access.UserID)
}

// checkAgents runs the checks in the sandbox of each user in agents, and
// returns what failed in any of them.
func (l *Local) checkAgents(ctx context.Context, cluster string, out *zonesetup.Output,
	agents []zonesetup.UserAccess,
) error {
	var failed []error
	for _, access := range agents {
		l.step("Running the end-to-end checks in " + access.UserEmail + "'s agent")
		if err := l.Check(ctx, l.agentTarget(cluster, out, access)); err != nil {
			failed = append(failed, err)
		}
	}
	return errors.Join(failed...)
}

// agentTarget is one user's sandbox pod. The agent of the person setup
// picked must show the GitHub login the settings name.
func (l *Local) agentTarget(cluster string, out *zonesetup.Output, access zonesetup.UserAccess) CheckTarget {
	target := CheckTarget{
		Cluster:   cluster,
		Namespace: out.K8sSandbox.Namespace,
		Pod:       access.UserID,
		UserID:    access.UserID,
	}
	if access.UserEmail == l.Settings[KeyUserEmail] {
		target.GitHubLogin = l.Settings[KeyGitHubLogin]
	}
	return target
}

// sandboxSelector selects the objects of the sandbox chart: each user's
// Sandbox, ConfigMap and ServiceAccount. userLabel holds the user's ID.
const sandboxSelector, userLabel = "app.kubernetes.io/name=keycard-sandbox-user", "keycard.ai/user-id"

// pruneSandboxes deletes each sandbox in namespace whose user is not in
// keep. zone-setup has archived the policies of such a user, so their
// agent can reach nothing.
func (l *Local) pruneSandboxes(ctx context.Context, cluster, namespace string, keep []zonesetup.UserAccess) error {
	raw, err := l.kubectl(ctx, cluster, nil, namespaceFlag, namespace, "get", "sandbox", "-l", sandboxSelector,
		"-o", `jsonpath={range .items[*]}{.metadata.labels.keycard\.ai/user-id}{"\n"}{end}`)
	if err != nil {
		return fmt.Errorf("%w: listing the sandboxes: %w", ErrPod, err)
	}
	for id := range strings.FieldsSeq(string(raw)) {
		if slices.ContainsFunc(keep, func(access zonesetup.UserAccess) bool { return access.UserID == id }) {
			continue
		}
		if err = l.deleteSandbox(ctx, cluster, namespace, id); err != nil {
			return err
		}
	}
	return nil
}

// deleteSandbox deletes one user's sandbox, with its ConfigMap and
// ServiceAccount. The controller then deletes the pod.
func (l *Local) deleteSandbox(ctx context.Context, cluster, namespace, userID string) error {
	if _, err := l.kubectl(ctx, cluster, nil, namespaceFlag, namespace, "delete",
		"sandbox,configmap,serviceaccount", "-l", sandboxSelector+","+userLabel+"="+userID,
		"--ignore-not-found", "--wait=true"); err != nil {
		return fmt.Errorf("%w: deleting the sandbox of user %s: %w", ErrPod, userID, err)
	}
	_, _ = fmt.Fprintf(l.Out, "Deleted the sandbox of user %s, and its pod.\n", userID)
	return nil
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

// k3dCluster creates or checks the k3d cluster and imports the images that
// come from Docker.
func (l *Local) k3dCluster(ctx context.Context, cfg runSettings) error {
	l.step("Creating or checking the k3d cluster " + cfg.cluster)
	if err := l.Bootstrap(ctx, cfg.cluster, cfg.issuerURL, false); err != nil {
		return err
	}
	return l.importImages(ctx, cfg.cluster, cfg.imageTag, cfg.scenario)
}

// runSettings validates the settings before it reads the issuer. For an
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
		return cfg, fmt.Errorf("%w: %s is empty. Run %s init and pick who the agent acts as",
			ErrSettings, KeyUserEmail, l.Command("init"))
	}
	if cfg.zoneOnHost, err = l.zoneOnHost(); err != nil {
		return cfg, err
	}
	if cfg.caFile, err = l.caFile(); err != nil {
		return cfg, err
	}
	// Kubernetes sets imagePullPolicy to Always for an image tagged latest
	// (https://kubernetes.io/docs/concepts/containers/images/#imagepullpolicy-defaulting).
	// On k3d the node would then pull instead of running the images Run
	// imports.
	if cfg.imageTag = l.Settings[KeyImageTag]; cfg.imageTag == "latest" {
		return cfg, fmt.Errorf("%w: %s must not be latest. Leave it empty to run the images the charts pin",
			ErrSettings, KeyImageTag)
	}
	if cfg.scenario, cfg.scenarioValues, err = l.scenarioValues(); err != nil {
		return cfg, err
	}
	cfg.answers = l.answers()
	if cfg, err = l.existingSettings(cfg); err != nil {
		return cfg, err
	}
	cfg.issuerURL, err = l.clusterIssuer(ctx, cfg.cluster, "")
	return cfg, err
}

// zoneOnHost reads KEYCARD_LOCAL_ZONE_ON_HOST, which is true or empty.
func (l *Local) zoneOnHost() (bool, error) {
	switch onHost := l.Settings[KeyZoneOnHost]; onHost {
	case "":
		return false, nil
	case settingTrue:
		return true, nil
	default:
		return false, fmt.Errorf("%w: %s must be true or empty (got %q)", ErrSettings, KeyZoneOnHost, onHost)
	}
}

func (l *Local) existingSettings(cfg runSettings) (runSettings, error) {
	if !l.Existing() {
		return cfg, nil
	}
	if cfg.zoneOnHost {
		return cfg, fmt.Errorf("%w: %s is for a k3d cluster", ErrExisting, KeyZoneOnHost)
	}
	var image string
	if config := l.config(); config != nil {
		image = config.Demo.Images[cfg.scenario.Name]
	}
	switch {
	case image != "":
		if !imageToken.MatchString(image) {
			return cfg, fmt.Errorf("%w: demo.images.%s must start with a letter or digit and contain only "+
				"ASCII letters, digits and . / : @ _ - (got %q)", ErrSettings, cfg.scenario.Name, image)
		}
		cfg.scenario.Image = image
		var err error
		if cfg.scenarioValues, err = cfg.scenario.Values(cfg.answers.forUser(cfg.email)); err != nil {
			return cfg, err
		}
	case cfg.scenario.Image != "":
		return cfg, fmt.Errorf("%w: the scenario %s runs %s, which is built on this machine. "+
			"Set demo.images.%s to a prebuilt registry image, or run this scenario on a k3d cluster",
			ErrExisting, cfg.scenario.Name, cfg.scenario.Image, cfg.scenario.Name)
	}
	var bad string
	if cfg.clusterCIDRs, bad = parseCIDRs(l.Settings[KeyClusterCIDRs]); bad != "" {
		return cfg, fmt.Errorf("%w: %s: %q is not an address range such as 10.0.0.0/16",
			ErrSettings, KeyClusterCIDRs, bad)
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

// parseCIDRs parses a comma-separated list of address ranges, each a prefix
// such as 10.0.0.0/16. It also returns the first item that is not a prefix,
// or "" when every item is one.
func parseCIDRs(list string) ([]string, string) {
	var out []string
	for item := range strings.SplitSeq(list, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(item)
		if err != nil {
			return nil, item
		}
		out = append(out, prefix.Masked().String())
	}
	return out, ""
}

// importImages imports images from Docker into the cluster: the three
// images, when tag is set, and the scenario's agent image, which it builds
// first. It fails before the build if Docker does not have one of the
// three.
func (l *Local) importImages(ctx context.Context, cluster, tag string, scenario *Scenario) error {
	var all []string
	if tag != "" {
		if err := l.inspectImages(ctx, tag); err != nil {
			return err
		}
		all = images(tag)
	}
	if scenario.Image != "" {
		if err := l.buildImage(ctx, tag, scenario); err != nil {
			return err
		}
		all = append(all, scenario.Image)
	}
	if len(all) == 0 {
		return nil
	}
	l.step("Importing the images into the cluster")
	if err := l.k3d(ctx, l.Out, slices.Concat([]string{"image", "import", "--cluster", cluster}, all)...); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(l.Out, "Imported %s into the k3d cluster %s.\n", strings.Join(all, ", "), cluster)
	return nil
}

// inspectImages fails if Docker does not have one of the three images,
// tagged tag.
func (l *Local) inspectImages(ctx context.Context, tag string) error {
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
	return nil
}

// buildImage builds the scenario's agent image. The build gets the
// sandbox-base image as BASE_IMAGE: at tag when it is set, or the one the
// install chart pins.
func (l *Local) buildImage(ctx context.Context, tag string, scenario *Scenario) error {
	base := imageRegistry + "sandbox-base:" + tag
	if tag == "" {
		var err error
		if base, err = l.pinnedBase(); err != nil {
			return err
		}
	}
	l.step("Building " + scenario.Image + " from " + filepath.Join(scenariosDir, scenario.Name))
	cmd, err := l.Tool(ctx, "docker", "build", "--tag", scenario.Image, "--build-arg", "BASE_IMAGE="+base,
		filepath.Join(l.Root, scenariosDir, scenario.Name))
	if err != nil {
		return err
	}
	cmd.Stdout, cmd.Stderr = l.Out, l.Out
	if err = cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrBuild, scenario.Image, err)
	}
	_, _ = fmt.Fprintf(l.Out, "Built the image %s.\n", scenario.Image)
	return nil
}

// pinnedBase returns the sandbox-base image the install chart pins. The
// chart's images.agent defaults to sandbox-base itself.
func (l *Local) pinnedBase() (string, error) {
	raw, err := os.ReadFile(filepath.Join(l.Root, "install", "values.yaml"))
	if err != nil {
		return "", fmt.Errorf("%w: reading the install chart's values: %w", ErrImages, err)
	}
	var values struct {
		Images struct {
			Agent string `json:"agent"`
		} `json:"images"`
	}
	if err = yaml.Unmarshal(raw, &values); err != nil {
		return "", fmt.Errorf("%w: install/values.yaml: %w", ErrImages, err)
	}
	if values.Images.Agent == "" {
		return "", fmt.Errorf("%w: install/values.yaml does not set images.agent", ErrImages)
	}
	return values.Images.Agent, nil
}

// zoneSetup runs zone-setup and writes its output to local/out/zone-setup.yaml,
// or reads that file when opts.SkipZoneSetup is set.
func (l *Local) zoneSetup(ctx context.Context, cfg runSettings, opts RunOptions) (*zonesetup.Output, error) {
	outFile := filepath.Join(l.outDir(), "zone-setup.yaml")
	if opts.SkipZoneSetup {
		out, err := l.readZoneSetup()
		if err != nil {
			return nil, fmt.Errorf("%w. Run without --skip-zone-setup", err)
		}
		return out, nil
	}
	l.step("Running zone-setup against " + cfg.zoneURL)
	values, err := l.zoneSetupValues(cfg.issuerURL, "the cluster's issuer")
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrZoneSetup, err)
	}
	l.useAccountEnv(&values.ManagementServiceAccount)
	secrets, err := l.loadSecrets(values)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrZoneSetup, err)
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
// a field it does not know and an empty field the sandbox needs. When no run
// wrote one, the error wraps fs.ErrNotExist.
func (l *Local) readZoneSetup() (*zonesetup.Output, error) {
	outFile := filepath.Join(l.outDir(), "zone-setup.yaml")
	raw, err := os.ReadFile(outFile) //nolint:gosec // G304: the path is under this checkout's local/out.
	switch {
	case err != nil:
		return nil, fmt.Errorf("%w: %w", ErrZoneSetup, err)
	case len(raw) == 0:
		return nil, fmt.Errorf("%w: %s is empty", ErrZoneSetup, outFile)
	}
	var out zonesetup.Output
	if err = yaml.UnmarshalStrict(raw, &out); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrZoneSetup, outFile, err)
	}
	if key := missingField(&out); key != "" {
		return nil, fmt.Errorf("%w: %s has no %s", ErrZoneSetup, outFile, key)
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
		"%w: %s is not in the zone-setup output. Give them an agent in init", ErrZoneSetup, email,
	)
}

// writeLocalValues writes the values files LocalValues returns, and the
// scenario's values file, to local/out.
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
	for name, body := range map[string][]byte{
		"install.values.yaml": install, "sandbox.values.yaml": sandbox, "scenario.values.yaml": cfg.scenarioValues,
	} {
		if err = os.WriteFile(filepath.Join(l.outDir(), name), body, fileMode); err != nil {
			return fmt.Errorf("writing %s: %w", name, err)
		}
	}
	_, _ = fmt.Fprintln(l.Out, "Wrote local/out/install.values.yaml, local/out/sandbox.values.yaml "+
		"and local/out/scenario.values.yaml.")
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
			filepath.Join(l.outDir(), "scenario.values.yaml"),
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
// the scenario's values file with the user's answers and the manifests to
// local/out/agents/<user ID>.
func (l *Local) newSandbox(
	ctx context.Context,
	cfg runSettings,
	out *zonesetup.Output,
	access zonesetup.UserAccess,
) error {
	dir := filepath.Join(l.outDir(), "agents", access.UserID)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	values, err := cfg.scenario.Values(cfg.answers.forUser(access.UserEmail))
	if err != nil {
		return fmt.Errorf("%s's agent: %w", access.UserEmail, err)
	}
	if err = os.WriteFile(filepath.Join(dir, "scenario.values.yaml"), values, fileMode); err != nil {
		return fmt.Errorf("writing the scenario values of %s: %w", access.UserEmail, err)
	}
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
			filepath.Join(dir, "scenario.values.yaml"),
		},
		Apply: true,
		Kube: func(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
			return l.kubectl(ctx, cfg.cluster, stdin, args...)
		},
		Log: l.Out,
	}
	manifests, err := sandbox.Run(ctx)
	if err != nil {
		return fmt.Errorf("new-sandbox: %w", err)
	}
	if err = os.WriteFile(filepath.Join(dir, "sandbox.yaml"), manifests, fileMode); err != nil {
		return fmt.Errorf("writing sandbox.yaml: %w", err)
	}
	_, _ = fmt.Fprintf(l.Out, "Wrote the applied manifests to local/out/agents/%s/sandbox.yaml.\n", access.UserID)
	return nil
}

// sandboxGeneration returns the metadata.generation of the Sandbox name,
// or "" when it does not exist or cannot be read.
func (l *Local) sandboxGeneration(ctx context.Context, cluster, namespace, name string) string {
	out, err := l.kubectl(ctx, cluster, nil, "--namespace", namespace,
		"get", "sandbox", name, "--ignore-not-found", "-o", "jsonpath={.metadata.generation}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// replaceStalePod deletes the pod of the Sandbox name when the pod has
// finished or the Sandbox's generation differs from before. The controller
// then creates the pod again from the template. The controller does not
// update an existing pod, and the pod's restartPolicy is Never.
func (l *Local) replaceStalePod(ctx context.Context, cluster, namespace, name, before string) error {
	out, err := l.kubectl(ctx, cluster, nil, "--namespace", namespace,
		"get", "pod", name, "--ignore-not-found", "-o", "jsonpath={.status.phase}")
	if err != nil {
		return fmt.Errorf("%w: %w", ErrPod, err)
	}
	phase := strings.TrimSpace(string(out))
	finished := phase == "Failed" || phase == "Succeeded"
	changed := before != "" && l.sandboxGeneration(ctx, cluster, namespace, name) != before
	if phase == "" || (!finished && !changed) {
		return nil
	}
	if _, err = l.kubectl(ctx, cluster, nil, "--namespace", namespace, "delete", "pod", name, "--wait=true"); err != nil {
		return fmt.Errorf("%w: %w", ErrPod, err)
	}
	_, _ = fmt.Fprintf(l.Out, "Deleted the pod %s, so the controller creates it again from the sandbox's template.\n",
		name)
	return nil
}

// waitForPod waits for the sandbox pod to exist and become Ready, and on a
// k3d cluster for the cluster's DNS too. If the pod does not become Ready,
// it prints the last 40 lines of each container's log.
func (l *Local) waitForPod(ctx context.Context, cluster, namespace, pod string) error {
	_, err := l.kubectl(ctx, cluster, nil, "--namespace", namespace,
		"wait", "--for=create", "pod/"+pod, "--timeout=120s")
	if err != nil {
		return fmt.Errorf("%w: %w", ErrPod, err)
	}
	if err = l.waitReady(ctx, cluster, namespace, pod); err == nil {
		return l.waitForDNS(ctx, cluster)
	}
	for _, container := range []string{"egress-netsetup", "egress-proxy", agentContainer} {
		logs, _ := l.kubectl(ctx, cluster, nil, "--namespace", namespace, "logs", pod, "-c", container, "--tail=40")
		_, _ = fmt.Fprintf(l.Out, "--- %s\n%s", container, logs)
	}
	return err
}

// podReadyRound is how long each kubectl wait lasts, and podReadyWait how
// long waitReady waits in all.
const (
	podReadyRound = 5 * time.Second
	podReadyWait  = 5 * time.Minute
)

// waitReady waits up to podReadyWait for pod to become ready. It returns
// early when the pod has stopped, as when the agent exits at startup, and the
// error then has the agent's last log line.
func (l *Local) waitReady(ctx context.Context, cluster, namespace, pod string) error {
	deadline := time.Now().Add(podReadyWait)
	for {
		_, err := l.kubectl(ctx, cluster, nil, "--namespace", namespace,
			"wait", "--for=condition=Ready", "pod/"+pod, "--timeout="+podReadyRound.String())
		if err == nil {
			return nil
		}
		out, phaseErr := l.kubectl(ctx, cluster, nil, "--namespace", namespace,
			"get", "pod", pod, "-o", "jsonpath={.status.phase}")
		switch phase := strings.TrimSpace(string(out)); {
		case phaseErr == nil && (phase == "Failed" || phase == "Succeeded"):
			stopped := fmt.Sprintf("%s stopped (%s) before it was ready", pod, phase)
			last, _ := l.kubectl(ctx, cluster, nil, "--namespace", namespace, "logs", pod, "-c", "agent", "--tail=1")
			if line := strings.TrimSpace(string(last)); line != "" {
				stopped += ". The agent's last log line: " + line
			}
			return fmt.Errorf("%w: %s", ErrPod, stopped)
		case phaseErr != nil, ctx.Err() != nil:
			return fmt.Errorf("%w: %s did not become ready: %w", ErrPod, pod, err)
		case time.Now().After(deadline):
			return fmt.Errorf("%w: %s did not become ready within %s", ErrPod, pod, podReadyWait)
		}
	}
}

// waitForDNS waits for CoreDNS on a k3d cluster. The proxy resolves the
// names it allows through it, so the agent cannot reach any host before it
// runs. On a new cluster, CoreDNS can start minutes after the sandbox pod,
// while its image downloads.
func (l *Local) waitForDNS(ctx context.Context, cluster string) error {
	if l.Existing() {
		return nil
	}
	_, _ = fmt.Fprintln(l.Out, "Waiting for the cluster's DNS (CoreDNS) to be ready.")
	if _, err := l.kubectl(ctx, cluster, nil, namespaceFlag, "kube-system",
		"rollout", "status", "deployment/coredns", "--timeout=300s"); err != nil {
		return fmt.Errorf("%w: the cluster's DNS (CoreDNS) did not become ready: %w", ErrPod, err)
	}
	return nil
}

func (l *Local) outDir() string {
	return filepath.Join(l.Root, "local", "out")
}

func (l *Local) step(title string) {
	_, _ = fmt.Fprintf(l.Out, "\n==> %s\n", title)
}
