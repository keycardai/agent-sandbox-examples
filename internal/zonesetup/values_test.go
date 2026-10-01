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
management_service_account:
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
		ManagementServiceAccount: zonesetup.ManagementServiceAccountValues{
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

// The refusals of a value that does not fit its field.
const (
	missingItem = "a list item is missing at line"
	wrongType   = "a value has the wrong type at line"
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
			name:  "a namespace YAML 1.1 reads as a boolean",
			doc:   minimal + "namespace: no\n",
			check: func(v *zonesetup.Values) bool { return v.Namespace == "no" },
		},
		{
			name:  "a null namespace defaults to agents",
			doc:   minimal + "namespace: null\n",
			check: func(v *zonesetup.Values) bool { return v.Namespace == zonesetup.DefaultNamespace },
		},
		{
			name:  "an AKS issuer keeps its trailing slash",
			doc:   strings.Replace(minimal, "https://oidc.eks.us-west-2.amazonaws.com/id/EXAMPLE", aksIssuer, 1),
			check: func(v *zonesetup.Values) bool { return v.IssuerURL == aksIssuer },
		},
		{
			name:  "an end marker with no document after it",
			doc:   minimal + "...\n",
			check: func(v *zonesetup.Values) bool { return v.ManagementAPIURL == "https://api.keycard.ai" },
		},
		{
			name: "both zone fields",
			doc:  minimal + "zone_id: zone-1\n",
			check: func(v *zonesetup.Values) bool {
				return v.ZoneURL == "https://acme.keycard.cloud" && v.ZoneID == "zone-1"
			},
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
			name:      "a query on the issuer url",
			doc:       strings.Replace(minimal, "id/EXAMPLE", "id/EXAMPLE?x=1", 1),
			wantField: "issuer_url must not have a query or a fragment",
		},
		{
			name:      "an empty query on the issuer url",
			doc:       strings.Replace(minimal, "id/EXAMPLE", "id/EXAMPLE?", 1),
			wantField: "issuer_url must not have a query or a fragment",
		},
		{
			name:      "a fragment on the zone url",
			doc:       strings.Replace(minimal, "acme.keycard.cloud", "acme.keycard.cloud/#a", 1),
			wantField: "zone_url must not have a query or a fragment",
		},
		{
			name:      "an empty fragment on the zone url",
			doc:       strings.Replace(minimal, "acme.keycard.cloud", "acme.keycard.cloud/#", 1),
			wantField: "zone_url must not have a query or a fragment",
		},
		{
			name:      "a plain http zone url, even beside a zone id",
			doc:       strings.Replace(minimal, "https://acme", "http://acme", 1) + "zone_id: z\n",
			wantField: "zone_url",
		},
		{name: "a bad namespace", doc: minimal + "namespace: Agents_1\n", wantField: "namespace"},
		{name: "a case-variant key", doc: minimal + "MANAGEMENT_API_URL: x\n", wantField: "MANAGEMENT_API_URL"},
		{name: "a port", doc: withAPI("https://api.keycard.ai:8443"), wantField: "management_api_url must not set a port"},
		{name: "a port with no host", doc: withAPI("https://:443"), wantField: "management_api_url"},
		{
			name:      "an empty management port",
			doc:       withAPI(`"https://api.keycard.ai:"`),
			wantField: "management_api_url must not set a port",
		},
		{name: "a loopback http url", doc: withAPI("http://127.0.0.1"), wantField: "management_api_url must use https"},
		{
			name: "an empty issuer port",
			doc: strings.Replace(
				minimal,
				"https://oidc.eks.us-west-2.amazonaws.com/id/EXAMPLE",
				`"https://i.example:"`,
				1,
			),
			wantField: "issuer_url must not set a port",
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
		{name: "a repeated key", doc: minimal + "namespace: a\nnamespace: b\n", wantField: "a key is repeated at line"},
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
		{name: "an anchor", doc: minimal + "namespace: &ns agents\n", wantField: "anchors and aliases"},
		{
			name:      "a mapping tagged as a string",
			doc:       minimal + "resources: [!!str {identifier: r, secret_env: K, prefix: true}]\n",
			wantField: wrongType,
		},
		{
			name:      "a list tagged as a mapping",
			doc:       minimal + "resources: !!map [{identifier: r, secret_env: K, prefix: true}]\n",
			wantField: wrongType,
		},
		{name: "a merge key", doc: minimal + "<<: {namespace: first}\n", wantField: "merge keys"},
		{
			name:      "a binary-tagged key",
			doc:       minimal + "resources: [{identifier: r, secret_env: K, !!binary cHJlZml4: \"yes\"}]\n",
			wantField: "a key must be a plain string",
		},
		{name: "a number key", doc: minimal + "1: x\n", wantField: "a key must be a plain string"},
		{name: "two merge keys", doc: minimal + "<<: {namespace: a}\n<<: {zone_id: z}\n", wantField: "merge keys"},
		{
			name:      "a value of the wrong type",
			doc:       minimal + "resources: [{identifier: r, secret_env: K, prefix: wrong}]\n",
			wantField: "a value has the wrong type at line",
		},
		{name: "a number as a string", doc: minimal + "namespace: 123\n", wantField: wrongType},
		{name: "a boolean as a string", doc: minimal + "namespace: true\n", wantField: wrongType},
		{
			name:      "a quoted string as a boolean",
			doc:       minimal + "resources: [{identifier: r, secret_env: K, prefix: \"yes\"}]\n",
			wantField: wrongType,
		},
		{
			name:      "a number as the client_id",
			doc:       strings.Replace(minimal, "client_id: sa-1\n", "client_id: 12345\n", 1),
			wantField: wrongType,
		},
		{name: "a binary string", doc: minimal + "namespace: !!binary YWdlbnRz\n", wantField: wrongType},
		{name: "a null user", doc: minimal + "users: [null]\n", wantField: missingItem},
		{
			name:      "a null resource after a resource",
			doc:       minimal + "resources: [{identifier: r, secret_env: K}, null]\n",
			wantField: missingItem,
		},
		{
			name: "a null user in JSON",
			doc: `{"management_api_url": "https://api.keycard.ai", "zone_id": "z", ` +
				`"management_service_account": {"client_id": "sa-1"}, ` +
				`"issuer_url": "https://oidc.example.com", "users": [null]}`,
			wantField: missingItem,
		},
		{
			name:      "a secret_env that is not a variable name",
			doc:       minimal + "resources: [{identifier: r, secret_env: 'a b'}]\n",
			wantField: "resources[0].secret_env is not an environment variable name",
		},
		{
			name:      "a client_secret_env that is not a variable name",
			doc:       strings.Replace(minimal, "client_id: sa-1\n", "client_id: sa-1\n  client_secret_env: a-b\n", 1),
			wantField: "client_secret_env is not an environment variable name",
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
		"a URL as an email":      minimal + "users: [{email: '" + url + "'}]\n",
		"a URL as the namespace": minimal + "namespace: " + url + "\n",
		"a URL identifier listed twice": minimal + "resources: [{identifier: '" + url + "', secret_env: K}, " +
			"{identifier: '" + url + "', secret_env: K}]\n",
		"a backtick in a mistagged value": strings.Replace(
			minimal, "https://api.keycard.ai", "!!int 'x`"+url+"'", 1,
		),
		"a line number inside a key": minimal + "'https://sa:line 8675309@api.keycard.ai': 1\n",
		"a line number inside a mistagged value": strings.Replace(
			minimal, "https://api.keycard.ai", "!!int 'https://sa:line 8675309@api.keycard.ai'", 1,
		),
		"a null key":                 minimal + "null: " + url + "\n",
		"a value of the wrong type":  minimal + "users: [" + url + "]\n",
		"a value with the wrong tag": strings.Replace(minimal, "https://api.keycard.ai", "!!int "+url, 1),
		"an unknown key":             minimal + url + ": 1\n",
	} {
		_, err := zonesetup.ParseValues(strings.NewReader(doc))
		if !errors.Is(err, zonesetup.ErrValues) {
			t.Errorf("%s: error = %v, want ErrValues", name, err)
			continue
		}
		if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "8675309") {
			t.Errorf("%s: error %q leaks the password", name, err)
		}
	}
}

// errReadURL is a read error that repeats a credential.
var errReadURL = errors.New("reading https://sa:hunter2@api.keycard.ai")

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errReadURL }

func TestReadErrorKeepsTextOut(t *testing.T) {
	t.Parallel()
	_, err := zonesetup.ParseValues(failingReader{})
	if !errors.Is(err, zonesetup.ErrValues) || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error = %v, want ErrValues without the reader's text", err)
	}
}

// TestLoadSecretsKeepsFileTextOut proves no missing-secret error repeats an
// identifier, a path or a read error, which can hold a credential.
func TestLoadSecretsKeepsFileTextOut(t *testing.T) {
	t.Parallel()
	const url = "https://sa:hunter2@api.keycard.ai" //nolint:gosec // A fake credential no error may repeat.
	values, err := zonesetup.ParseValues(strings.NewReader(minimal +
		"resources: [{identifier: '" + url + "', secret_env: K}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	env := func(k string) (string, bool) { return "set", k == zonesetup.DefaultManagementSecretEnv }
	_, err = zonesetup.LoadSecrets(values, env, nil)
	if !errors.Is(err, zonesetup.ErrMissingSecret) || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("missing resource secret: error = %v, want ErrMissingSecret without the identifier", err)
	}

	values.ManagementServiceAccount = zonesetup.ManagementServiceAccountValues{
		ClientID:         "sa-1",
		ClientSecretFile: "/" + url,
	}
	for name, read := range map[string]func(string) ([]byte, error){
		"a read error":  func(string) ([]byte, error) { return nil, errReadURL },
		"an empty file": func(string) ([]byte, error) { return nil, nil },
	} {
		_, err = zonesetup.LoadSecrets(values, env, read)
		if !errors.Is(err, zonesetup.ErrMissingSecret) || strings.Contains(err.Error(), "hunter2") {
			t.Errorf("%s: error = %v, want ErrMissingSecret without the path", name, err)
		}
	}
}

func TestSecretFileTrimsWhitespace(t *testing.T) {
	t.Parallel()
	values, err := zonesetup.ParseValues(strings.NewReader(
		strings.Replace(minimal, "client_id: sa-1\n", "client_id: sa-1\n  client_secret_file: /secret\n", 1),
	))
	if err != nil {
		t.Fatal(err)
	}
	const want = "s3cret"
	for _, file := range []string{"\t" + want + "\n\n", "  " + want + "  \n", want + "\r\n"} {
		read := func(string) ([]byte, error) { return []byte(file), nil }
		got, loadErr := zonesetup.LoadSecrets(values, func(string) (string, bool) { return "", false }, read)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		if got.ManagementClientSecret != want {
			t.Errorf("secret from %q = %q, want %q", file, got.ManagementClientSecret, want)
		}
	}
	for _, file := range []string{"\n", " \t\r\n"} {
		empty := func(string) ([]byte, error) { return []byte(file), nil }
		_, err = zonesetup.LoadSecrets(values, func(string) (string, bool) { return "", false }, empty)
		if !errors.Is(err, zonesetup.ErrMissingSecret) || !strings.Contains(err.Error(), "is empty") {
			t.Errorf("file %q: error = %v, want ErrMissingSecret for an empty file", file, err)
		}
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
