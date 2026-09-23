package newsandbox_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/keycardai/agent-sandbox-poc/internal/newsandbox"
)

const (
	namespace = "agents"
	userID    = "igmua2g0fwapl21qll7tczdw7t"
	subject   = "system:serviceaccount:agents:igmua2g0fwapl21qll7tczdw7t"
	clientID  = "cred_01hzx"
)

// valid holds the inputs for one enrolled user.
func valid() newsandbox.Inputs {
	return newsandbox.Inputs{
		Namespace:      namespace,
		UserID:         userID,
		UserEmail:      "someone@example.com",
		ServiceAccount: subject,
		ClientID:       clientID,
	}
}

// TestInputsValidate checks each input error that stops a run before it
// renders.
func TestInputsValidate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		change func(*newsandbox.Inputs)
		want   string
	}{
		{"complete", func(*newsandbox.Inputs) {}, ""},
		{"no namespace", func(in *newsandbox.Inputs) { in.Namespace = "" }, "--namespace is required"},
		{"no user id", func(in *newsandbox.Inputs) { in.UserID = "" }, "--user-id is required"},
		{"no client id", func(in *newsandbox.Inputs) { in.ClientID = "" }, "--client-id is required"},
		{
			"no service account",
			func(in *newsandbox.Inputs) { in.ServiceAccount = "" },
			"--service-account is required",
		},
		{
			"user id is not a DNS label",
			func(in *newsandbox.Inputs) {
				in.UserID = "User_1"
				in.ServiceAccount = "system:serviceaccount:agents:User_1"
			},
			"is not a DNS label",
		},
		{
			"namespace is not a DNS label",
			func(in *newsandbox.Inputs) {
				in.Namespace = "agents\n  x: y"
				in.ServiceAccount = "system:serviceaccount:agents\n  x: y:" + userID
			},
			`--namespace "agents\n  x: y" is not a DNS label`,
		},
		{
			"subject from another namespace",
			func(in *newsandbox.Inputs) { in.ServiceAccount = "system:serviceaccount:other:" + userID },
			"this sandbox binds",
		},
		{
			"subject from another user",
			func(in *newsandbox.Inputs) { in.ServiceAccount = "system:serviceaccount:agents:someone-else" },
			"this sandbox binds",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inputs := valid()
			tc.change(&inputs)
			err := inputs.Validate()
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("err = %v, want none", err)
			case tc.want == "":
			case err == nil || !strings.Contains(err.Error(), tc.want):
				t.Fatalf("err = %v, want one naming %q", err, tc.want)
			case !errors.Is(err, newsandbox.ErrInputs):
				t.Fatalf("err = %v, want an ErrInputs", err)
			}
		})
	}
}

// fakeKube records what new-sandbox asked kubectl to do.
type fakeKube struct {
	args  [][]string
	stdin [][]byte
	out   []byte
	err   error
}

func (k *fakeKube) run(_ context.Context, stdin []byte, args ...string) ([]byte, error) {
	k.args = append(k.args, args)
	k.stdin = append(k.stdin, stdin)
	return k.out, k.err
}

// command is one run against the real chart with the example values.
func command(t *testing.T, kube *fakeKube, apply bool) *newsandbox.Command {
	t.Helper()
	dir, err := filepath.Abs("../../sandbox")
	if err != nil {
		t.Fatal(err)
	}
	return &newsandbox.Command{
		Inputs:      valid(),
		ChartDir:    dir,
		ValuesFiles: []string{filepath.Join(dir, "values.example.yaml")},
		Apply:       apply,
		Kube:        kube.run,
	}
}

// TestRunRenders checks that a run without Apply returns the four objects
// and does not call kubectl.
func TestRunRenders(t *testing.T) {
	t.Parallel()
	kube := &fakeKube{}
	manifests, err := command(t, kube, false).Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"kind: ServiceAccount",
		"kind: ConfigMap",
		"kind: Sandbox",
		"name: " + userID + "-proxy-config",
		"name: " + userID + "-agent-config",
		"serviceAccountName: " + userID,
		"client_id: " + clientID,
	} {
		if !strings.Contains(string(manifests), want) {
			t.Errorf("the manifests do not contain %q", want)
		}
	}
	if len(kube.args) != 0 {
		t.Errorf("a render ran kubectl: %v", kube.args)
	}
}

// TestRunApplies checks that Run passes the rendered manifests to kubectl
// apply on standard input, in the sandbox namespace.
func TestRunApplies(t *testing.T) {
	t.Parallel()
	kube := &fakeKube{out: []byte("sandbox.agents.x-k8s.io/" + userID + " created\n")}
	manifests, err := command(t, kube, true).Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"apply", "--namespace", namespace, "-f", "-"}}
	if !reflect.DeepEqual(kube.args, want) {
		t.Fatalf("kubectl calls = %v, want %v", kube.args, want)
	}
	if !reflect.DeepEqual(kube.stdin[0], manifests) {
		t.Error("kubectl was given something other than the rendered manifests")
	}
}

// TestRunApplyFails checks that Run returns ErrKubectl when kubectl apply
// fails.
func TestRunApplyFails(t *testing.T) {
	t.Parallel()
	kube := &fakeKube{err: errors.New("the server could not find the requested resource")} //nolint:err113 // a fixture
	_, err := command(t, kube, true).Run(t.Context())
	if !errors.Is(err, newsandbox.ErrKubectl) {
		t.Fatalf("err = %v, want an ErrKubectl", err)
	}
}

// TestRunRefusesInvalidInputs checks that Run does not call kubectl when the
// service account does not match the namespace and user id.
func TestRunRefusesInvalidInputs(t *testing.T) {
	t.Parallel()
	kube := &fakeKube{}
	cmd := command(t, kube, true)
	cmd.Inputs.ServiceAccount = "system:serviceaccount:agents:someone-else"
	if _, err := cmd.Run(t.Context()); !errors.Is(err, newsandbox.ErrInputs) {
		t.Fatalf("err = %v, want an ErrInputs", err)
	}
	if len(kube.args) != 0 {
		t.Errorf("kubectl ran anyway: %v", kube.args)
	}
}
