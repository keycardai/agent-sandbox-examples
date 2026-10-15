package sandbox_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"slices"
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
	// leftoversCall lists the containers labeled with the cluster's name.
	leftoversCall = "docker KUBERC= ps --all --filter label=k3d.cluster=asp --format {{.Names}}\n"
	// leftover prints a container left over from an earlier cluster asp.
	leftover = `[[ "$FAKE_TOOL $1" == "docker ps" ]] && echo k3d-asp-tools` + "\n"
	// removeQuestion is the question Bootstrap asks about leftover, and its
	// default.
	removeQuestion = "Remove k3d-asp-tools, left over from an earlier k3d cluster asp? [default false]"
	// createFails is a k3d create that fails. With LOG_COLORS=false it logs
	// logfmt, as k3d does, and otherwise it logs nothing.
	createFails = `[[ "$FAKE_TOOL $1 $2" == "k3d cluster create" ]] && {
	if [[ ${LOG_COLORS:-} == false ]]; then
		at='time="2026-10-15T11:56:37-04:00"'
		printf '%s level=info msg="Prep: Network"\n' "$at"
		printf '%s level=error msg="Failed Cluster Start: port \\"6443\\" is in use"\n' "$at"
		printf '%s level=fatal msg="Cluster creation FAILED, all changes have been rolled back!"\n' "$at"
	fi
	exit 1
}
`
	// createFailsBare is a k3d create that fails with a message logfmt does
	// not quote.
	createFailsBare = `[[ "$FAKE_TOOL $1 $2" == "k3d cluster create" ]] && {
	printf 'time="2026-10-15T11:56:38-04:00" level=fatal msg=refused\n'
	exit 1
}
`
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

// deleteAdvice ends the error from Bootstrap for a cluster with another
// issuer. up asks to delete the cluster instead, so it must not print this.
const deleteAdvice = "k3d sets them only when it creates a cluster. Delete the cluster with\n" +
	"    go run ./cmd/sandbox down --cluster asp, or bootstrap with its issuer"

// noContext makes kubectl fail as it does for a kubeconfig without the
// cluster's context.
const noContext = `case "$*" in *"get --raw /.well-known/openid-configuration")
	echo 'error: context "k3d-asp" does not exist' >&2; exit 1 ;;
esac
`

// brokenAdvice ends the error from Bootstrap for a cluster without a
// kubeconfig context. It does not suggest the cluster's issuer.
const brokenAdvice = ". Delete the cluster with\n" +
	"    go run ./cmd/sandbox down --cluster asp"

// TestBootstrap checks that Bootstrap creates a missing cluster or checks an
// existing one, then checks the iss of a minted token. A nil ask means stdin
// is not a terminal.
func TestBootstrap(t *testing.T) {
	t.Parallel()
	const other = "https://other.example.com"

	tests := []struct {
		name, list, discovered, minted, before string
		ask                                    *confirmAsker
		want                                   []error
		wantText, wantCalls, wantOut           string
		wantAsked                              []string
	}{
		{
			name: "absent", list: `[{"name":"other"}]`, minted: testIssuer,
			wantCalls: listCall + leftoversCall + createCall + mintCalls,
			wantOut:   "Created the k3d cluster asp, and added the context k3d-asp to your kubeconfig.\n",
		},
		{
			name: "left over, answered yes", list: "[]", minted: testIssuer, before: leftover,
			ask:       &confirmAsker{answer: true},
			wantCalls: listCall + leftoversCall + deleteCall + createCall + mintCalls,
			wantOut:   "Removing k3d-asp-tools, left over from an earlier k3d cluster asp.\n",
			wantAsked: []string{removeQuestion},
		},
		{
			name: "left over, answered no", list: "[]", before: leftover, ask: &confirmAsker{},
			want:      []error{sandbox.ErrConfirm},
			wantText:  "nothing removed. k3d cannot create the cluster asp while k3d-asp-tools exists",
			wantCalls: listCall + leftoversCall,
			wantAsked: []string{removeQuestion},
		},
		{
			name: "left over, no terminal", list: "[]", before: leftover,
			want:      []error{sandbox.ErrConfirm, sandbox.ErrNoTerminal},
			wantText:  "Remove it with\n    k3d cluster delete asp",
			wantCalls: listCall + leftoversCall,
		},
		{
			name: "create fails", list: "[]", before: createFails,
			wantText:  `k3d cluster create asp: Failed Cluster Start: port "6443" is in use: exit status 1`,
			wantCalls: listCall + leftoversCall + createCall,
			wantOut:   "Cluster creation FAILED",
		},
		{
			name: "create fails with a bare message", list: "[]", before: createFailsBare,
			wantText:  "k3d cluster create asp: refused: exit status 1",
			wantCalls: listCall + leftoversCall + createCall,
		},
		{
			name: "present with the issuer", list: present, discovered: testIssuer, minted: testIssuer,
			wantCalls: listCall + discoveryCall + mintCalls,
		},
		{
			name: "present with another issuer", list: present, discovered: other,
			want:      []error{sandbox.ErrIssuer, sandbox.ErrOtherIssuer},
			wantText:  deleteAdvice,
			wantCalls: listCall + discoveryCall,
		},
		{
			name: "present without a kubeconfig context", list: present, before: noContext,
			want:      []error{sandbox.ErrBrokenCluster},
			wantText:  "your kubeconfig has no context k3d-asp" + brokenAdvice,
			wantCalls: listCall + discoveryCall,
		},
		{
			name: "token names another issuer", list: "[]", minted: other,
			want: []error{sandbox.ErrIssuer}, wantText: `the cluster mints tokens with iss "` + other + `"`,
			wantCalls: listCall + leftoversCall + createCall + mintCalls,
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
			local, calls := newLocal(t, sandbox.Settings{}, tc.before+clusterTool(tc.list, tc.discovered, tc.minted))
			out := &bytes.Buffer{}
			local.Out = out
			tc.ask.use(local)
			err := local.Bootstrap(t.Context(), cluster, testIssuer, false)
			if tc.want == nil && tc.wantText != "" {
				if err == nil || err.Error() != tc.wantText {
					t.Fatalf("err = %v, want %q", err, tc.wantText)
				}
			} else {
				checkErr(t, err, tc.want, tc.wantText)
			}
			if got := readCalls(t, calls); got != tc.wantCalls {
				t.Fatalf("calls = %q, want %q", got, tc.wantCalls)
			}
			if !strings.Contains(out.String(), tc.wantOut) {
				t.Errorf("output lacks %q\n%s", tc.wantOut, out)
			}
			if got := tc.ask.questions(); !slices.Equal(got, tc.wantAsked) {
				t.Errorf("asked %q, want %q", got, tc.wantAsked)
			}
		})
	}
}

// The lines Down prints when the cluster is absent and when it deletes it.
const (
	noCluster = "There is no k3d cluster named asp. Nothing to delete.\n"
	// nothingDeleted is the error text for an answer other than yes.
	nothingDeleted = "nothing deleted"
	deleted        = "Deleted the k3d cluster asp, and removed the context k3d-asp from your kubeconfig.\n"
	// deleteQuestion is the question Down asks and its default, as
	// confirmAsker records them.
	deleteQuestion = "Delete the k3d cluster asp? [default false]"
	// noTerminal is the error text when stdin is not a terminal.
	noTerminal = "no terminal to ask. Pass --yes"
)

// TestDown checks that Down deletes the cluster only when it exists and the
// operator confirms with --yes or answers yes to a question whose default is
// no. A nil ask means stdin is not a terminal.
func TestDown(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, list string
		yes        bool
		ask        *confirmAsker
		want       []error
		wantText   string
		wantOut    string
		wantAsked  []string
		wantCalls  string
	}{
		{
			name: "absent", list: `[{"name":"asp-other"},{"name":"as"}]`,
			wantOut: noCluster, wantCalls: listCall,
		},
		{name: "empty list", list: "[]", wantOut: noCluster, wantCalls: listCall},
		{name: "yes", list: present, yes: true, wantOut: deleted, wantCalls: listCall + deleteCall},
		{
			name: "answered yes", list: present, ask: &confirmAsker{answer: true},
			wantOut: deleted, wantAsked: []string{deleteQuestion}, wantCalls: listCall + deleteCall,
		},
		{
			name: "answered no", list: present, ask: &confirmAsker{},
			want: []error{sandbox.ErrConfirm}, wantText: nothingDeleted, wantAsked: []string{deleteQuestion},
			wantCalls: listCall,
		},
		{
			name: "no terminal", list: present,
			want: []error{sandbox.ErrConfirm, sandbox.ErrNoTerminal}, wantText: noTerminal, wantCalls: listCall,
		},
		{
			name: "interrupted", list: present, ask: &confirmAsker{err: sandbox.ErrInterrupted},
			want: []error{sandbox.ErrConfirm, sandbox.ErrInterrupted}, wantText: "nothing deleted: interrupted",
			wantAsked: []string{deleteQuestion}, wantCalls: listCall,
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
			tc.ask.use(local)
			err := local.Down(t.Context(), "", tc.yes)
			checkErr(t, err, tc.want, tc.wantText)
			if !strings.Contains(out.String(), tc.wantOut) {
				t.Errorf("output = %q, want it to contain %q", out, tc.wantOut)
			}
			if got := tc.ask.questions(); !slices.Equal(got, tc.wantAsked) {
				t.Errorf("asked %q, want %q", got, tc.wantAsked)
			}
			if got := readCalls(t, calls); got != tc.wantCalls {
				t.Fatalf("calls = %q, want %q", got, tc.wantCalls)
			}
		})
	}
}

// confirmAsker answers every yes or no question with answer and err. It
// records each question with its default answer. A nil confirmAsker means
// stdin is not a terminal. Any other kind of question panics, because the
// embedded Asker is nil.
type confirmAsker struct {
	sandbox.Asker

	answer bool
	err    error
	asked  []string
}

func (a *confirmAsker) Confirm(_ context.Context, question string, value bool) (bool, error) {
	a.asked = append(a.asked, fmt.Sprintf("%s [default %t]", question, value))
	return a.answer, a.err
}

// use sets local.Ask to a. A nil a leaves local.Ask nil.
func (a *confirmAsker) use(local *sandbox.Local) {
	if a != nil {
		local.Ask = a
	}
}

func (a *confirmAsker) questions() []string {
	if a == nil {
		return nil
	}
	return a.asked
}
