package zonesetup

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestEnsureUserAccessClientSecret runs the client secret mode against the
// fake zone. Every user shares one password credential, and the file keeps
// its secret because the zone returns the secret only on creation.
func TestEnsureUserAccessClientSecret(t *testing.T) {
	t.Parallel()
	const (
		email   = "ada@example.com"
		created = `{"id":"credential-pw","application_id":"application-k","type":"password",` +
			`"identifier":"client-new","password":"secret-new"}`
		listedKept = `{"id":"credential-kept","application_id":"application-k","type":"password",` +
			`"identifier":"client-kept"}`
		// listedToken has the kept client ID but is not a password credential.
		listedToken = `{"id":"credential-token","application_id":"application-k","type":"token",` +
			`"identifier":"client-kept"}`
		keptFile = `{"client_id":"client-kept","client_secret":"secret-kept"}`
		newFile  = `{"client_id":"client-new","client_secret":"secret-new"}`
	)
	values := &Values{
		Namespace: "agents", ApplicationCredential: CredentialClientSecret, Users: []UserValues{{Email: email}},
	}
	access := func(clientID string) []UserAccess {
		return []UserAccess{{
			UserEmail: email, UserID: adaID, ClientID: clientID, Namespace: "agents", UserIdentifier: adaIdentifier,
		}}
	}
	cases := []struct {
		name string
		// file is the client secret file before the run. Empty means none.
		file     string
		fileMode os.FileMode
		listed   string
		dryRun   bool
		// noFileOption runs without Options.ClientSecretFile.
		noFileOption bool
		wantAccess   []UserAccess
		wantWrites   []string
		wantFile     string
		wantLog      string
		wantErr      error
	}{
		{
			name:       "a first run creates the credential and writes the file",
			wantAccess: access("client-new"), wantWrites: []string{bindCall}, wantFile: newFile,
			wantLog: "created client secret client-new and wrote it to FILE\nfound user user-1 for ada@example.com\n",
		},
		{
			name: "a run reuses the credential the file names", file: keptFile, fileMode: 0o600, listed: listedKept,
			wantAccess: access("client-kept"), wantFile: keptFile,
			wantLog: "found client secret client-kept from FILE\nfound user user-1 for ada@example.com\n",
		},
		{
			name: "a credential the application lost is replaced", file: keptFile, fileMode: 0o644,
			wantAccess: access("client-new"), wantWrites: []string{bindCall}, wantFile: newFile,
			wantLog: "application application-k has no credential client-kept\n" +
				"created client secret client-new and wrote it to FILE\nfound user user-1 for ada@example.com\n",
		},
		{
			name: "a token credential with the same identifier does not count", file: keptFile, fileMode: 0o600,
			listed: listedToken, wantAccess: access("client-new"), wantWrites: []string{bindCall}, wantFile: newFile,
			wantLog: "application application-k has no credential client-kept\n" +
				"created client secret client-new and wrote it to FILE\nfound user user-1 for ada@example.com\n",
		},
		{
			name: "a dry run creates nothing", dryRun: true,
			wantAccess: access(""),
			wantLog:    "would create a client secret\nfound user user-1 for ada@example.com\n",
		},
		{
			name: "a run without a file is refused", noFileOption: true, wantErr: ErrClientSecret,
		},
		{
			name: "a file without a secret is refused", file: `{"client_id":"client-kept"}`, fileMode: 0o600,
			wantErr: ErrClientSecret, wantFile: `{"client_id":"client-kept"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeZone{bodies: map[string]string{bindCall: created, listBindingsCall: page(tc.listed)}}
			session := connect(t, fake)
			path := filepath.Join(t.TempDir(), "client-secret.json")
			if tc.file != "" {
				if err := os.WriteFile(path, []byte(tc.file), tc.fileMode); err != nil {
					t.Fatal(err)
				}
			}
			var stderr bytes.Buffer
			opts := &Options{DryRun: tc.dryRun, Log: &stderr, ClientSecretFile: path}
			if tc.noFileOption {
				opts.ClientSecretFile = ""
			}
			got, err := ensureUserAccess(t.Context(), opts, session, applicationK, "", values)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if !slices.Equal(got, tc.wantAccess) {
				t.Errorf("access = %+v, want %+v", got, tc.wantAccess)
			}
			if !slices.Equal(fake.writes, tc.wantWrites) {
				t.Errorf("writes = %q, want %q", fake.writes, tc.wantWrites)
			}
			wantLog := string(bytes.ReplaceAll([]byte(tc.wantLog), []byte("FILE"), []byte(path)))
			if tc.wantErr == nil && stderr.String() != wantLog {
				t.Errorf("stderr = %q, want %q", stderr.String(), wantLog)
			}
			checkClientSecretFile(t, path, tc.wantFile, tc.wantWrites != nil)
		})
	}
}

// checkClientSecretFile checks that the file at path holds want, or does not
// exist when want is empty. A file the run wrote must have mode 0600.
func checkClientSecretFile(t *testing.T, path, want string, written bool) {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the test's own file.
	switch {
	case want == "" && !errors.Is(err, os.ErrNotExist):
		t.Errorf("the client secret file exists (err %v), want none", err)
		return
	case want == "":
		return
	case err != nil:
		t.Fatalf("reading the client secret file: %v", err)
	case string(raw) != want:
		t.Errorf("client secret file = %s, want %s", raw, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if written && info.Mode().Perm() != 0o600 {
		t.Errorf("client secret file mode = %#o, want 0600", info.Mode().Perm())
	}
}
