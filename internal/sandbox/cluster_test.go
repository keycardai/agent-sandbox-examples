package sandbox_test

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
)

const (
	testIssuer = "https://asp.example.com"
	listCall   = "k3d KUBERC= cluster list -o json\n"
	createCall = "k3d KUBERC= cluster create asp --image rancher/k3s:v1.36.4-k3s1" +
		" --kubeconfig-update-default=true --kubeconfig-switch-context=false" +
		" --k3s-arg --kube-apiserver-arg=service-account-issuer=" + testIssuer + "@server:*" +
		" --k3s-arg --kube-apiserver-arg=service-account-jwks-uri=" + testIssuer + "/openid/v1/jwks@server:*" +
		" --k3s-arg --disable=traefik@server:* --k3s-arg --disable=servicelb@server:*\n"
	discoveryCall = "kubectl KUBERC=off --context k3d-asp get --raw /.well-known/openid-configuration\n"
	mintCalls     = "kubectl KUBERC=off --context k3d-asp --namespace default" +
		" wait --for=create serviceaccount/default --timeout=120s\n" +
		"kubectl KUBERC=off --context k3d-asp --namespace default create token default --duration 10m\n"
	deleteCall = "k3d KUBERC= cluster delete asp\n"
	// present is a k3d listing that includes the cluster asp.
	present = `[{"name":"other"},{"name":"asp"}]`
)

// clusterTool fakes k3d and kubectl. `k3d cluster list` prints list, or
// fails when list is FAIL. Other k3d calls succeed. kubectl prints a
// discovery document with issuer discovered and a token with iss minted.
func clusterTool(list, discovered, minted string) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"` + minted + `"}`))
	discovery := fmt.Sprintf(`{"issuer":%q,"jwks_uri":%q}`, discovered, discovered+"/openid/v1/jwks")
	return fmt.Sprintf(`case "$FAKE_TOOL $*" in
"k3d cluster list -o json") [[ '%s' == FAIL ]] && exit 1; printf '%%s\n' '%s' ;;
*"get --raw /.well-known/openid-configuration") printf '%%s\n' '%s' ;;
*"create token default --duration 10m") printf '%%s\n' 'e30.%s.c2ln' ;;
esac
`, list, list, discovery, payload)
}

// TestBootstrapDryRun checks that a dry run validates the issuer and prints
// the k3d command without running a tool.
func TestBootstrapDryRun(t *testing.T) {
	t.Parallel()
	const (
		valid  = testIssuer + ":8443/oidc"
		noHost = "the issuer must name a host of lowercase ASCII DNS labels"
	)

	tests := []struct {
		name, issuerURL, wantText string
	}{
		{"http", "http://asp.example.com", "the issuer must be https"},
		{"no host", "https:///path", noHost},
		{"space in host", "https://asp .example.com", noHost},
		{"userinfo", "https://user@asp.example.com", noHost},
		{"unicode host", "https://asp.exämple.com", noHost},
		{"port out of range", testIssuer + ":65536", "the issuer port must be 1-65535"},
		{"space in path", testIssuer + "/a b", "after the host, the issuer may have only a port and a path"},
		{"query", valid + "?a=b", "the issuer must not have a query or fragment"},
		{"fragment", valid + "#a", "the issuer must not have a query or fragment"},
		{"trailing slash", valid + "/", "the issuer must not end in a slash"},
		{"valid", valid, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			local, calls := newLocal(t, sandbox.Settings{}, "")
			out := &bytes.Buffer{}
			local.Out = out
			err := local.Bootstrap(t.Context(), cluster, tc.issuerURL, true)
			if tc.wantText != "" {
				checkErr(t, err, []error{sandbox.ErrIssuer}, tc.wantText)
			} else {
				checkErr(t, err, nil, "")
				want := "would run: k3d cluster create asp --image rancher/k3s:v1.36.4-k3s1" +
					" --kubeconfig-update-default=true --kubeconfig-switch-context=false" +
					" --k3s-arg --kube-apiserver-arg=service-account-issuer=" + valid + "@server:*" +
					" --k3s-arg --kube-apiserver-arg=service-account-jwks-uri=" + valid + "/openid/v1/jwks@server:*" +
					" --k3s-arg --disable=traefik@server:* --k3s-arg --disable=servicelb@server:*\n"
				if out.String() != want {
					t.Fatalf("output = %q, want %q", out, want)
				}
			}
			if got := readCalls(t, calls); got != "" {
				t.Fatalf("calls = %q, want none", got)
			}
		})
	}
}

// TestBootstrap checks that Bootstrap creates a missing cluster or checks an
// existing one, then checks the iss of a minted token.
func TestBootstrap(t *testing.T) {
	t.Parallel()
	const other = "https://other.example.com"

	tests := []struct {
		name, list, discovered, minted string
		want                           []error
		wantText, wantCalls, wantOut   string
	}{
		{
			name: "absent", list: `[{"name":"other"}]`, minted: testIssuer,
			wantCalls: listCall + createCall + mintCalls,
			wantOut:   "Created the k3d cluster asp, and added the context k3d-asp to your kubeconfig.\n",
		},
		{
			name: "present with the issuer", list: present, discovered: testIssuer, minted: testIssuer,
			wantCalls: listCall + discoveryCall + mintCalls,
		},
		{
			name: "present with another issuer", list: present, discovered: other,
			want: []error{sandbox.ErrIssuer}, wantText: "cluster asp was created with other settings",
			wantCalls: listCall + discoveryCall,
		},
		{
			name: "token names another issuer", list: "[]", minted: other,
			want: []error{sandbox.ErrIssuer}, wantText: `the cluster mints tokens with iss "` + other + `"`,
			wantCalls: listCall + createCall + mintCalls,
		},
		{
			name: "cannot list", list: "FAIL",
			want: []error{sandbox.ErrDocker}, wantText: "cannot list k3d clusters. Is Docker running?",
			wantCalls: listCall,
		},
		{
			name: "entry without a name", list: `[{}]`,
			want: []error{sandbox.ErrDocker}, wantText: "k3d cluster list printed something other than a cluster list",
			wantCalls: listCall,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			local, calls := newLocal(t, sandbox.Settings{}, clusterTool(tc.list, tc.discovered, tc.minted))
			out := &bytes.Buffer{}
			local.Out = out
			err := local.Bootstrap(t.Context(), cluster, testIssuer, false)
			checkErr(t, err, tc.want, tc.wantText)
			if got := readCalls(t, calls); got != tc.wantCalls {
				t.Fatalf("calls = %q, want %q", got, tc.wantCalls)
			}
			if !strings.Contains(out.String(), tc.wantOut) {
				t.Errorf("output lacks %q\n%s", tc.wantOut, out)
			}
		})
	}
}

// The lines Down prints when the cluster is absent and when it deletes it.
const (
	noCluster = "There is no k3d cluster named asp. Nothing to delete.\n"
	deleted   = "Deleted the k3d cluster asp, and removed the context k3d-asp from your kubeconfig.\n"
)

// TestDown checks that Down deletes the cluster only when it exists and the
// operator confirms with --yes or an answer that starts with y.
func TestDown(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, list string
		yes        bool
		answer     string
		want       []error
		wantText   string
		wantOut    string
		wantCalls  string
	}{
		{
			name: "absent", list: `[{"name":"asp-other"},{"name":"as"}]`,
			wantOut: noCluster, wantCalls: listCall,
		},
		{name: "empty list", list: "[]", wantOut: noCluster, wantCalls: listCall},
		{name: "yes", list: present, yes: true, wantOut: deleted, wantCalls: listCall + deleteCall},
		{name: "answered y", list: present, answer: "y\n", wantOut: deleted, wantCalls: listCall + deleteCall},
		{
			name: "answered n", list: present, answer: "n\n",
			want: []error{sandbox.ErrConfirm}, wantText: "nothing deleted", wantCalls: listCall,
		},
		{
			name: "no answer", list: present,
			want: []error{sandbox.ErrConfirm}, wantText: "no answer. Pass --yes", wantCalls: listCall,
		},
		{
			name: "cannot list", list: "FAIL", yes: true,
			want: []error{sandbox.ErrDocker}, wantText: "Is Docker running?", wantCalls: listCall,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			local, calls := newLocal(t, sandbox.Settings{sandbox.KeyCluster: cluster}, clusterTool(tc.list, "", ""))
			out := &bytes.Buffer{}
			local.Out = out
			err := local.Down(t.Context(), "", tc.yes, strings.NewReader(tc.answer))
			checkErr(t, err, tc.want, tc.wantText)
			if !strings.Contains(out.String(), tc.wantOut) {
				t.Errorf("output = %q, want it to contain %q", out, tc.wantOut)
			}
			if got := readCalls(t, calls); got != tc.wantCalls {
				t.Fatalf("calls = %q, want %q", got, tc.wantCalls)
			}
		})
	}
}
