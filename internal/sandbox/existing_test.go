package sandbox_test

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
)

const (
	eksContext = "arn:aws:eks:us-west-2:111122223333:cluster/demo"
	eksIssuer  = "https://oidc.eks.us-west-2.amazonaws.com/id/EXAMPLE"
	eksRanges  = "10.0.0.0/16,172.20.0.0/16"
)

// existing returns the settings for a sandbox on the existing EKS cluster.
func existing() sandbox.Settings {
	return sandbox.Settings{
		sandbox.KeyKubeContext: eksContext, sandbox.KeyClusterCIDRs: eksRanges,
		sandbox.KeyZoneURL: zoneURL, sandbox.KeyUserEmail: email,
	}
}

// eksTool fakes kubectl for the EKS cluster. Its kubeconfig lists the k3d
// context and eksContext, and the discovery document names eksIssuer. Other
// commands go to fake.
func eksTool(fake fakeSandbox) string {
	return `case "$FAKE_TOOL $*" in
"kubectl config get-contexts --output=name") printf 'k3d-keycard-sandbox-local\n` + eksContext + `\n'; exit 0 ;;
*"get --raw /.well-known/openid-configuration")
	echo '{"issuer":"` + eksIssuer + `","jwks_uri":"` + eksIssuer + `/keys"}'; exit 0 ;;
esac
` + fake.script()
}

// TestExistingRefuses checks that bootstrap, down, tunnel and mirror fail
// with ErrExisting for an existing cluster before any tool runs.
func TestExistingRefuses(t *testing.T) {
	t.Parallel()
	steps := map[string]func(*sandbox.Local) error{
		"bootstrap": func(l *sandbox.Local) error { return l.Bootstrap(t.Context(), "", "", false) },
		"down":      func(l *sandbox.Local) error { return l.Down(t.Context(), "", true) },
		"tunnel":    func(l *sandbox.Local) error { return l.Tunnel(t.Context(), listen) },
		"mirror":    func(l *sandbox.Local) error { return l.Mirror(t.Context(), "", "", listen) },
	}
	for name, step := range steps {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			local, calls := newLocal(t, existing(), "")
			checkErr(t, step(local), []error{sandbox.ErrExisting}, name+" is for a k3d cluster")
			if got := readCalls(t, calls); got != "" {
				t.Fatalf("calls = %q, want none", got)
			}
		})
	}
}

// TestRunExistingSettings checks that Run refuses settings it cannot use
// for an existing cluster. Run reads the cluster's issuer before it checks
// the settings, so each case expects that one kubectl call.
func TestRunExistingSettings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, key, value string
		want             error
		wantText         string
	}{
		{
			"zone on this machine",
			sandbox.KeyZoneOnHost,
			"true",
			sandbox.ErrExisting,
			"KEYCARD_LOCAL_ZONE_ON_HOST is for a k3d cluster",
		},
		{"no ranges", sandbox.KeyClusterCIDRs, "", sandbox.ErrSettings, "KEYCARD_LOCAL_CLUSTER_CIDRS is empty"},
		{
			"a range without a prefix",
			sandbox.KeyClusterCIDRs,
			"10.0.0.0",
			sandbox.ErrSettings,
			`"10.0.0.0" is not an address range`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			settings := existing()
			settings[tc.key] = tc.value
			local, calls := newLocal(t, settings, eksTool(healthy()))
			checkErr(t, local.Run(t.Context(), sandbox.RunOptions{}), []error{tc.want}, tc.wantText)
			want := "kubectl KUBERC=off --context " + eksContext + " get --raw /.well-known/openid-configuration\n"
			if got := readCalls(t, calls); got != want {
				t.Fatalf("calls = %q, want %q", got, want)
			}
		})
	}
}

// TestRunExisting runs the whole flow on an existing cluster. It checks that
// Run does not call k3d or docker, that helm uses the cluster's context, that
// both values files name the cluster's ranges, and that the pod runs the
// images the charts pin.
func TestRunExisting(t *testing.T) {
	t.Parallel()
	local, calls := newLocal(t, existing(), eksTool(healthy()))
	linkCharts(t, local.Root)
	writeFile(t, filepath.Join(local.Root, "local", "out", "zone-setup.yaml"), zoneSetupOutput)
	if err := local.Run(t.Context(), sandbox.RunOptions{SkipZoneSetup: true}); err != nil {
		t.Fatal(err)
	}
	got := readCalls(t, calls)
	if strings.Contains(got, "k3d ") || strings.Contains(got, "docker ") {
		t.Errorf("calls = %q, want no k3d or docker", got)
	}
	if want := " --kube-context " + eksContext + " --namespace agents "; !strings.Contains(got, want) {
		t.Errorf("calls lack %q\n%s", want, got)
	}
	for _, name := range []string{"install.values.yaml", "sandbox.values.yaml"} {
		path := filepath.Join(local.Root, "local", "out", name)
		raw, err := os.ReadFile(path) //nolint:gosec // G304: the path is the test's own file.
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"- 10.0.0.0/16", "- 172.20.0.0/16"} {
			if !strings.Contains(string(raw), want) {
				t.Errorf("%s lacks %q\n%s", name, want, raw)
			}
		}
	}
	checkImages(t, local.Root, pinnedImages())
}

// TestUpExistingPrint prints only check-issuer and run for an existing
// cluster.
func TestUpExistingPrint(t *testing.T) {
	t.Parallel()
	local, _ := newLocal(t, existing(), "")
	out := &bytes.Buffer{}
	local.Out = out
	if err := local.Up(t.Context(), sandbox.UpOptions{Listen: listen, Print: true}); err != nil {
		t.Fatal(err)
	}
	if want := "  go run ./cmd/sandbox check-issuer\n  go run ./cmd/sandbox run\n"; out.String() != want {
		t.Fatalf("output = %q, want %q", out, want)
	}
}

// TestUpExisting runs Up on an existing cluster. Up returns after the run
// step, so a failed check is its error.
func TestUpExisting(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		sandbox  fakeSandbox
		want     []error
		wantText string
	}{
		{name: "checks pass", sandbox: healthy()},
		{
			name:     "a check fails",
			sandbox:  with(func(f *fakeSandbox) { f.allowed = "403" }),
			want:     []error{sandbox.ErrCheck},
			wantText: oneFailed,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			issuerURL, caFile, token := servedIssuer(t)
			settings := existing()
			settings[sandbox.KeyCAFile] = caFile
			local, _ := newLocal(t, settings, `case "$FAKE_TOOL $*" in
*"get --raw /.well-known/openid-configuration") echo '{"issuer":"`+issuerURL+`"}'; exit 0 ;;
*"create token default"*) echo '`+token+`'; exit 0 ;;
esac
`+tc.sandbox.script())
			linkCharts(t, local.Root)
			writeFile(t, filepath.Join(local.Root, "local", "out", "zone-setup.yaml"), zoneSetupOutput)
			err := local.Up(t.Context(), sandbox.UpOptions{
				Run: sandbox.RunOptions{SkipZoneSetup: true},
			})
			checkErr(t, err, tc.want, tc.wantText)
		})
	}
}

// servedIssuer serves an issuer's discovery document and JWKS over TLS. It
// returns the issuer's URL, a CA file that trusts it, and a token it signed
// for the subject and audience check-issuer asks for.
func servedIssuer(t *testing.T) (string, string, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	b64 := base64.RawURLEncoding.EncodeToString
	mux := http.NewServeMux()
	server := httptest.NewTLSServer(mux)
	t.Cleanup(server.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"issuer":%q,"jwks_uri":%q}`, server.URL, server.URL+"/jwks")
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"keys":[{"kty":"RSA","alg":"RS256","use":"sig","kid":"k","n":%q,"e":%q}]}`,
			b64(key.N.Bytes()), b64(big.NewInt(int64(key.E)).Bytes()))
	})
	now := time.Now()
	input := b64([]byte(`{"alg":"RS256","kid":"k","typ":"JWT"}`)) + "." + b64(fmt.Appendf(nil,
		`{"iss":%q,"sub":"system:serviceaccount:default:default","aud":["oidc-check"],"iat":%d,"exp":%d}`,
		server.URL, now.Unix(), now.Add(time.Hour).Unix()))
	digest := sha256.Sum256([]byte(input))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return server.URL, trustFile(t, server), input + "." + b64(signature)
}

// TestInitExisting chooses the EKS context. Init asks for the ranges, and
// does not ask how to publish the issuer. Init refuses a zone
// on this machine, because the cluster cannot dial it.
func TestInitExisting(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, zoneURL string
		want          []error
		wantText      string
	}{
		{name: "zone on the internet", zoneURL: "https://192.0.2.10"},
		{
			name: "zone on this machine", zoneURL: "https://localhost",
			want: []error{sandbox.ErrExisting}, wantText: "an existing cluster cannot connect to it",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server, caFile := fakeManagement(t, tc.zoneURL, gitHubProviders, "")
			local := initLocal(t, eksTool(healthy()))
			answers := signInAnswers(server.URL, caFile, goodSecret)
			answers["Which cluster runs the sandbox?"] = []string{eksContext}
			answers["The cluster's pod and service ranges"] = []string{eksRanges}
			ask := &scriptedAsker{t: t, answers: answers}
			local.Ask = ask

			_, err := local.Init(t.Context())
			checkErr(t, err, tc.want, tc.wantText)
			if tc.want != nil {
				return
			}
			for _, question := range ask.asked {
				if strings.HasPrefix(question, "How does the zone reach") {
					t.Errorf("asked %q, want no issuer question for an existing cluster", question)
				}
			}
			settings, err := sandbox.LoadSettings(local.Root, "", func(string) string { return "" })
			if err != nil {
				t.Fatal(err)
			}
			for key, want := range map[string]string{
				sandbox.KeyKubeContext: eksContext, sandbox.KeyImageTag: "",
				sandbox.KeyClusterCIDRs: eksRanges, sandbox.KeyZoneOnHost: "", sandbox.KeyIssuer: "",
			} {
				if settings[key] != want {
					t.Errorf("local.env %s = %q, want %q", key, settings[key], want)
				}
			}
		})
	}
}
