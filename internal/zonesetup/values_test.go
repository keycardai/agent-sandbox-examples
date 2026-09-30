package zonesetup_test

import (
	"bytes"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/keycardai/agent-sandbox-poc/internal/zonesetup"
)

const openRouterKey = "OPENROUTER_API_KEY"

const minimal = `
management_api_url: https://api.keycard.ai
zone_url: https://acme.keycard.cloud
service_account:
  client_id: sa-1
issuer_url: https://oidc.eks.us-west-2.amazonaws.com/id/EXAMPLE
`

// TestParseExample parses the documented example so it cannot drift from the
// format ParseValues accepts.
func TestParseExample(t *testing.T) {
	t.Parallel()
	example, err := os.ReadFile("../../zone-setup.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	got, err := zonesetup.ParseValues(bytes.NewReader(example))
	if err != nil {
		t.Fatal(err)
	}
	want := &zonesetup.Values{
		ManagementAPIURL: "https://api.keycard.ai",
		ZoneURL:          "https://acme.keycard.cloud",
		ServiceAccount: zonesetup.ServiceAccountValues{
			ClientID: "replace-me", ClientSecretEnv: zonesetup.DefaultManagementSecretEnv,
		},
		IssuerURL: "https://oidc.eks.us-west-2.amazonaws.com/id/EXAMPLE",
		Namespace: "agents",
		Users:     []zonesetup.UserValues{{Email: "someone@example.com"}},
		Resources: []zonesetup.ResourceValues{
			{Identifier: "https://api.github.com/", Name: "GitHub", Prefix: true, CredentialProvider: "github"},
			{Identifier: "https://openrouter.ai/api/v1", Name: "OpenRouter", Prefix: true, SecretEnv: openRouterKey},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("values = %+v\nwant %+v", got, want)
	}
}

// withAPI is the minimal document with another management_api_url.
func withAPI(url string) string {
	return strings.Replace(minimal, "https://api.keycard.ai", url, 1)
}

const aksIssuer = "https://eastus.oic.prod-aks.azure.com/tenant/cluster/"

// The refusals of a file that holds more than one document.
const (
	afterFirst  = "content after the first document"
	moreThanOne = "more than one document"
)

func TestParseValues(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		doc       string
		wantField string
		check     func(*zonesetup.Values) bool
	}{
		{
			name: "both zone fields",
			doc:  minimal + "zone_id: zone-1\n",
			check: func(v *zonesetup.Values) bool {
				return v.ZoneURL == "https://acme.keycard.cloud" && v.ZoneID == "zone-1"
			},
		},
		{
			name:  "an AKS issuer keeps its trailing slash",
			doc:   strings.Replace(minimal, "https://oidc.eks.us-west-2.amazonaws.com/id/EXAMPLE", aksIssuer, 1),
			check: func(v *zonesetup.Values) bool { return v.IssuerURL == aksIssuer },
		},
		{
			name:  "a loopback host may use http",
			doc:   withAPI("http://127.0.0.1:8080"),
			check: func(v *zonesetup.Values) bool { return v.ManagementAPIURL == "http://127.0.0.1:8080" },
		},
		{
			name:  "namespace defaults to agents",
			doc:   minimal,
			check: func(v *zonesetup.Values) bool { return v.Namespace == zonesetup.DefaultNamespace },
		},
		{
			name:      "neither zone field",
			doc:       strings.Replace(minimal, "zone_url: https://acme.keycard.cloud\n", "", 1),
			wantField: "zone_url or zone_id",
		},
		{name: "an unknown key", doc: minimal + "policy_set: Sandboxes\n", wantField: `"policy_set"`},
		{
			name:      "a plain http zone url, even beside a zone id",
			doc:       strings.Replace(minimal, "https://acme", "http://acme", 1) + "zone_id: z\n",
			wantField: "zone_url",
		},
		{name: "a bad namespace", doc: minimal + "namespace: Agents_1\n", wantField: "namespace"},
		{name: "a case-variant key", doc: minimal + "MANAGEMENT_API_URL: x\n", wantField: "MANAGEMENT_API_URL"},
		{name: "a port out of range", doc: withAPI("https://api.keycard.ai:65536"), wantField: "management_api_url port"},
		{name: "a non-numeric port", doc: withAPI("https://api.keycard.ai:https"), wantField: "management_api_url"},
		{name: "a port with no host", doc: withAPI("https://:443"), wantField: "management_api_url"},
		{name: "an empty management port", doc: withAPI(`"https://api.keycard.ai:"`), wantField: "management_api_url port"},
		{
			name: "an empty issuer port",
			doc: strings.Replace(
				minimal,
				"https://oidc.eks.us-west-2.amazonaws.com/id/EXAMPLE",
				`"https://i.example:"`,
				1,
			),
			wantField: "issuer_url port",
		},
		{name: "a second YAML document", doc: minimal + "---\nnamespace: other\n", wantField: moreThanOne},
		{
			name:      "a document after an end marker",
			doc:       minimal + "...\nnamespace: other\n",
			wantField: afterFirst,
		},
		{
			name:      "a second document with bare CR line endings",
			doc:       strings.ReplaceAll(minimal+"---\nnamespace: other\n", "\n", "\r"),
			wantField: moreThanOne,
		},
		{
			name:      "two adjacent flow mappings",
			doc:       "{zone_id: z}{namespace: other}",
			wantField: afterFirst,
		},
		{
			name:      "two adjacent JSON objects",
			doc:       `{"zone_id": "z"}{"namespace": "other"}`,
			wantField: afterFirst,
		},
		{name: "a repeated key", doc: minimal + "namespace: a\nnamespace: b\n", wantField: `"namespace"`},
		{
			name:      "a user listed twice",
			doc:       minimal + "users: [{email: a@x.io}, {email: a@x.io}]\n",
			wantField: "users[1].email",
		},
		{
			name:      "a resource listed twice",
			doc:       minimal + "resources: [{identifier: r, secret_env: K}, {identifier: r, secret_env: K}]\n",
			wantField: "resources[1].identifier",
		},
		{
			name:      "a resource with both credential sources",
			doc:       minimal + "resources: [{identifier: r, secret_env: K, credential_provider: p}]\n",
			wantField: "resources[0]: set exactly one",
		},
		{
			name:      "a resource with no credential source",
			doc:       minimal + "resources: [{identifier: r}]\n",
			wantField: "resources[0]: set exactly one",
		},
		{
			name:      "an anchor and an alias",
			doc:       minimal + "namespace: &ns agents\nzone_id: *ns\n",
			wantField: "anchors and aliases",
		},
		{name: "a merge key", doc: minimal + "<<: {namespace: first}\n", wantField: "merge keys"},
		{name: "two merge keys", doc: minimal + "<<: {namespace: a}\n<<: {zone_id: z}\n", wantField: "merge keys"},
		{
			name:  "a trailing end marker",
			doc:   minimal + "...\n",
			check: func(v *zonesetup.Values) bool { return v.Namespace == zonesetup.DefaultNamespace },
		},
		{name: "a display-name email", doc: minimal + "users: [{email: A <a@x.io>}]\n", wantField: "users[0].email"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := zonesetup.ParseValues(strings.NewReader(tc.doc))
			if tc.wantField != "" {
				if !errors.Is(err, zonesetup.ErrValues) || !strings.Contains(err.Error(), tc.wantField) {
					t.Fatalf("error = %v, want ErrValues naming %s", err, tc.wantField)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !tc.check(got) {
				t.Errorf("values = %+v", got)
			}
		})
	}
}

// TestURLUserinfo also proves the refusal never echoes the credential.
func TestURLUserinfo(t *testing.T) {
	t.Parallel()
	_, err := zonesetup.ParseValues(strings.NewReader(withAPI("https://sa:hunter2@api.keycard.ai")))
	if !errors.Is(err, zonesetup.ErrValues) || !strings.Contains(err.Error(), "must not carry userinfo") {
		t.Fatalf("error = %v, want ErrValues refusing userinfo in management_api_url", err)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error %q leaks the password", err)
	}
}

// TestParseErrorsKeepFileTextOut proves no parse error repeats a value from
// the file, even one that no check reaches.
func TestParseErrorsKeepFileTextOut(t *testing.T) {
	t.Parallel()
	const url = "https://sa:hunter2@api.keycard.ai" //nolint:gosec // A fake credential no error may repeat.
	for name, doc := range map[string]string{
		"a value with the wrong tag": strings.Replace(minimal, "https://api.keycard.ai", "!!int "+url, 1),
		"a null key":                 minimal + "null: " + url + "\n",
		"an unknown key":             minimal + url + ": 1\n",
		"a value of the wrong type":  minimal + "users: [" + url + "]\n",
	} {
		_, err := zonesetup.ParseValues(strings.NewReader(doc))
		if !errors.Is(err, zonesetup.ErrValues) {
			t.Errorf("%s: error = %v, want ErrValues", name, err)
			continue
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("%s: error %q leaks the password", name, err)
		}
	}
}

func TestSecretFileKeepsBytes(t *testing.T) {
	t.Parallel()
	values, err := zonesetup.ParseValues(strings.NewReader(
		strings.Replace(minimal, "client_id: sa-1\n", "client_id: sa-1\n  client_secret_file: /secret\n", 1),
	))
	if err != nil {
		t.Fatal(err)
	}
	for file, want := range map[string]string{"  s3cret  \n": "  s3cret  ", "s3cret\r\n": "s3cret"} {
		read := func(string) ([]byte, error) { return []byte(file), nil }
		got, loadErr := zonesetup.LoadSecrets(values, func(string) (string, bool) { return "", false }, read)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		if got.ManagementClientSecret != want {
			t.Errorf("secret from %q = %q, want %q", file, got.ManagementClientSecret, want)
		}
	}
	empty := func(string) ([]byte, error) { return []byte("\n"), nil }
	_, err = zonesetup.LoadSecrets(values, func(string) (string, bool) { return "", false }, empty)
	if !errors.Is(err, zonesetup.ErrMissingSecret) || !strings.Contains(err.Error(), "is empty") {
		t.Fatalf("error = %v, want ErrMissingSecret for an empty file", err)
	}
}

func TestLoadSecrets(t *testing.T) {
	t.Parallel()
	values, err := zonesetup.ParseValues(strings.NewReader(minimal + `
resources:
  - identifier: https://openrouter.ai/api/v1
    secret_env: OPENROUTER_API_KEY
`))
	if err != nil {
		t.Fatal(err)
	}
	noFile := func(string) ([]byte, error) { return nil, os.ErrNotExist }
	env := map[string]string{zonesetup.DefaultManagementSecretEnv: "sa-secret", openRouterKey: "or-key"}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }

	got, err := zonesetup.LoadSecrets(values, lookup, noFile)
	if err != nil {
		t.Fatal(err)
	}
	want := &zonesetup.Secrets{
		ManagementClientSecret: "sa-secret",
		Resource:               map[string]string{openRouterKey: "or-key"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("secrets = %+v, want %+v", got, want)
	}

	delete(env, openRouterKey)
	_, err = zonesetup.LoadSecrets(values, lookup, noFile)
	if !errors.Is(err, zonesetup.ErrMissingSecret) || !strings.Contains(err.Error(), openRouterKey) {
		t.Fatalf("error = %v, want ErrMissingSecret naming OPENROUTER_API_KEY", err)
	}
}
