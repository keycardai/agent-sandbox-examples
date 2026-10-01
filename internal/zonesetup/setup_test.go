//nolint:goconst,varnamelen // The double repeats API field names and ids so each expectation reads on its own.
package zonesetup_test

import (
	"bytes"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/keycardai/agent-sandbox-poc/internal/zonesetup"
)

// testValues is the values file every run case starts from. It is built
// directly because the double serves plain http on a loopback port, which
// ParseValues refuses.
func testValues(f *zoneAPI) *zonesetup.Values {
	return &zonesetup.Values{
		ManagementAPIURL:         f.srv.URL,
		ZoneURL:                  testZoneURL,
		ManagementServiceAccount: zonesetup.ManagementServiceAccountValues{ClientID: testClientID},
		IssuerURL:                testIssuer,
		Namespace:                zonesetup.DefaultNamespace,
		Resources: []zonesetup.ResourceValues{
			{Identifier: githubURL, Name: "GitHub API", Prefix: true, CredentialProvider: githubProvider},
			{
				Identifier: openRouterURL, Name: openRouterURL,
				Prefix: true, SecretEnv: openRouterKey,
			},
		},
	}
}

func testSecrets() *zonesetup.Secrets {
	return &zonesetup.Secrets{
		ManagementClientSecret: testClientSecret,
		Resource:               map[string]string{openRouterKey: testResourceSecret},
	}
}

// runCase is one scenario: a seeded zone, one run, and the writes, outputs,
// log lines and error it must produce.
type runCase struct {
	name   string
	seed   func(f *zoneAPI)
	values func(v *zonesetup.Values)
	dryRun bool

	wantWrites []string
	wantErr    string
	wantOut    *zonesetup.Outputs
	wantLog    []string
	check      func(t *testing.T, f *zoneAPI)
}

func freshWrites() []string {
	return []string{
		"POST /zones/zone-1/providers",
		"POST /zones/zone-1/applications",
		"POST /zones/zone-1/resources",
		"POST /zones/zone-1/resources",
		"POST /zones/zone-1/secrets",
		"PUT /zones/zone-1/applications/app-1/dependencies/res-1",
		"PUT /zones/zone-1/applications/app-1/dependencies/res-2",
	}
}

func completeOutputs() *zonesetup.Outputs {
	return &zonesetup.Outputs{
		K8sSandbox: zonesetup.K8sSandboxOutputs{
			ProviderID: "prov-1", ApplicationID: "app-1",
			ApplicationIdentifier: "urn:keycard:sandbox-proxy:agents", Namespace: zonesetup.DefaultNamespace,
		},
	}
}

// runCases seeds, runs and checks each case in parallel.
func runCases(t *testing.T, cases []runCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newZoneAPI(t)
			if tc.seed != nil {
				tc.seed(f)
			}
			values := testValues(f)
			if tc.values != nil {
				tc.values(values)
			}
			var log bytes.Buffer
			setup := &zonesetup.Setup{
				Values:  values,
				Secrets: testSecrets(),
				DryRun:  tc.dryRun,
				Log:     &log,
			}
			out, err := setup.Run(t.Context())
			checkRunError(t, err, tc.wantErr, log.String())
			if got := f.writes(); !reflect.DeepEqual(got, tc.wantWrites) {
				t.Errorf("writes:\n got %s\nwant %s\nlog:\n%s",
					strings.Join(got, "\n     "), strings.Join(tc.wantWrites, "\n     "), log.String())
			}
			if tc.wantOut != nil && !reflect.DeepEqual(out, tc.wantOut) {
				t.Errorf("outputs = %+v\nwant %+v", out, tc.wantOut)
			}
			checkLogLines(t, log.String(), tc.wantLog)
			if tc.check != nil {
				tc.check(t, f)
			}
		})
	}
}

// TestRunZone pins the once-per-zone decisions: zone resolution, the
// provider, the application, the resources, their secrets and dependencies.
func TestRunZone(t *testing.T) {
	t.Parallel()
	runCases(t, []runCase{
		{
			name:       "fresh zone creates every entity",
			wantWrites: freshWrites(),
			wantOut:    completeOutputs(),
			check:      checkFreshZone,
		},
		{
			name:       "second run over a complete zone only re-stores the secret",
			seed:       func(f *zoneAPI) { f.seedComplete() },
			wantWrites: []string{"PATCH /zones/zone-1/secrets/sec-1"},
			wantOut:    completeOutputs(),
			check:      checkSecretRestored,
		},
		{
			name: "drift is patched",
			seed: func(f *zoneAPI) {
				f.seedComplete()
				(*f.find(f.providers, "prov-1"))[keyProtocols] = record{
					keyOAuth2: record{keyIssuer: "https://old-issuer.example.com"},
				}
				(*f.find(f.applications, "app-1"))["consent"] = "required"
				(*f.find(f.resources, "res-1"))["prefix"] = false
				f.dependencies["app-1"] = []string{"res-1"}
			},
			wantWrites: []string{
				"PATCH /zones/zone-1/providers/prov-1",
				"PATCH /zones/zone-1/applications/app-1",
				"PATCH /zones/zone-1/resources/res-1",
				"PATCH /zones/zone-1/secrets/sec-1",
				"PUT /zones/zone-1/applications/app-1/dependencies/res-2",
			},
			wantOut: completeOutputs(),
			check:   checkDrift,
		},
		{
			name:   "dry run over a fresh zone writes nothing",
			dryRun: true,
			wantLog: []string{
				"would create provider " + testIssuer,
				"would create application urn:keycard:sandbox-proxy:agents",
				"would create resource https://api.github.com/ (prefix=true, provider=prov-github)",
				"would store the OPENROUTER_API_KEY credential for https://openrouter.ai/api/v1",
				"would make 2 resources dependencies of the application",
			},
		},
		{
			name: "dry run over a drifted zone reports the patches and writes nothing",
			seed: func(f *zoneAPI) {
				f.seedComplete()
				(*f.find(f.resources, "res-1"))["prefix"] = false
			},
			dryRun: true,
			wantLog: []string{
				`would patch resource res-1 (prefix false -> true, provider "prov-github" -> "prov-github")`,
				"would update secret sec-1 from OPENROUTER_API_KEY",
			},
		},
		{
			name:       "a missing credential provider stops the run before the resource is created",
			values:     func(v *zonesetup.Values) { v.Resources[0].CredentialProvider = "gitlab" },
			wantWrites: []string{"POST /zones/zone-1/providers", "POST /zones/zone-1/applications"},
			wantErr: "resource https://api.github.com/: not found: provider with identifier gitlab. " +
				"Create it in the zone first",
		},
		{
			name:    "no zone issues the zone URL",
			values:  func(v *zonesetup.Values) { v.ZoneURL = "https://other.keycard.cloud" },
			wantErr: "zone not found: no zone the service account can list issues https://other.keycard.cloud",
		},
		{
			name:       "a zone id skips the zone listing",
			seed:       func(f *zoneAPI) { f.seedComplete() },
			values:     func(v *zonesetup.Values) { v.ZoneID = testZoneID; v.ZoneURL = "" },
			wantWrites: []string{"PATCH /zones/zone-1/secrets/sec-1"},
			wantOut:    completeOutputs(),
			check:      checkZoneByID,
		},
	})
}

// checkRunError requires no error when wantErr is empty, and an error
// containing wantErr otherwise.
func checkRunError(t *testing.T, err error, wantErr, log string) {
	t.Helper()
	if wantErr == "" && err != nil {
		t.Fatalf("Run: %v\nlog:\n%s", err, log)
	}
	if wantErr != "" && (err == nil || !strings.Contains(err.Error(), wantErr)) {
		t.Fatalf("Run error = %v, want it to contain %q\nlog:\n%s", err, wantErr, log)
	}
}

// checkLogLines requires each line to appear in the log as a whole line.
func checkLogLines(t *testing.T, log string, lines []string) {
	t.Helper()
	for _, line := range lines {
		if !strings.Contains(log, "zone-setup: "+line+"\n") {
			t.Errorf("log lacks %q\nlog:\n%s", line, log)
		}
	}
}

func checkFreshZone(t *testing.T, f *zoneAPI) {
	t.Helper()
	checkFreshZoneIdentities(t, f)
	checkFreshZoneResources(t, f)
}

// checkFreshZoneIdentities pins the provider and application bodies.
func checkFreshZoneIdentities(t *testing.T, f *zoneAPI) {
	t.Helper()
	prov := f.bodyOf("POST /zones/zone-1/providers")
	if prov["identifier"] != testIssuer {
		t.Errorf("provider identifier = %v, want the issuer URL", prov["identifier"])
	}
	if got := dig(prov, keyProtocols, keyOAuth2, keyIssuer); got != testIssuer {
		t.Errorf("provider issuer = %v, want %s", got, testIssuer)
	}
	for _, key := range []string{"client_id", "client_secret"} {
		if _, ok := prov[key]; ok {
			t.Errorf("provider body sets %s; an issuer-only provider has none", key)
		}
	}
	app := f.bodyOf("POST /zones/zone-1/applications")
	if app["identifier"] != "urn:keycard:sandbox-proxy:agents" || app["consent"] != "implicit" ||
		app["name"] != applicationName {
		t.Errorf("application body = %v", app)
	}
}

// checkFreshZoneResources pins the secret and the resources' providers.
func checkFreshZoneResources(t *testing.T, f *zoneAPI) {
	t.Helper()
	sec := f.bodyOf("POST /zones/zone-1/secrets")
	if sec["entity_id"] != "res-2" || sec["name"] != openRouterKey ||
		dig(sec, "data", "token") != testResourceSecret || dig(sec, "data", "type") != "token" {
		t.Errorf("secret body = %v", sec)
	}
	res := *f.find(f.resources, "res-1")
	if res["prefix"] != true || res["credential_provider_id"] != "prov-github" {
		t.Errorf("github resource = %v", res)
	}
	if res := *f.find(f.resources, "res-2"); res["credential_provider_id"] != "prov-vault" {
		t.Errorf("openrouter resource = %v; want the vault provider", res)
	}
}

func checkSecretRestored(t *testing.T, f *zoneAPI) {
	t.Helper()
	if got := (*f.find(f.secrets, "sec-1"))["token"]; got != testResourceSecret {
		t.Errorf("stored secret = %v, want or-key", got)
	}
}

func checkDrift(t *testing.T, f *zoneAPI) {
	t.Helper()
	if got := dig(
		f.bodyOf("PATCH /zones/zone-1/providers/prov-1"),
		keyProtocols,
		keyOAuth2,
		keyIssuer,
	); got != testIssuer {
		t.Errorf("provider patch issuer = %v", got)
	}
	if got := f.bodyOf("PATCH /zones/zone-1/applications/app-1")["consent"]; got != "implicit" {
		t.Errorf("application patch consent = %v", got)
	}
	res := f.bodyOf("PATCH /zones/zone-1/resources/res-1")
	if res["prefix"] != true || res["credential_provider_id"] != "prov-github" {
		t.Errorf("resource patch = %v", res)
	}
}

func checkZoneByID(t *testing.T, f *zoneAPI) {
	t.Helper()
	reads := f.reads()
	if slices.Contains(reads, "/zones") || !slices.Contains(reads, "/zones/zone-1") {
		t.Errorf("reads = %v; want GET /zones/zone-1 and no listing", reads)
	}
}

// dig walks nested string-keyed maps.
func dig(m map[string]any, keys ...string) any {
	var cur any = m
	for _, k := range keys {
		next, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = next[k]
	}
	return cur
}
