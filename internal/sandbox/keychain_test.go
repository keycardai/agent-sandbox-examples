package sandbox_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
)

// TestKeychainFallback keeps the secrets in a file readable by its owner
// alone on a platform with no keychain, and says so. keyring's mock is
// global, so the keychain tests do not run in parallel.
//
//nolint:paralleltest // keyring's mock is global.
func TestKeychainFallback(t *testing.T) {
	keyring.MockInitWithError(keyring.ErrUnsupportedPlatform)
	t.Cleanup(keyring.MockInit)
	keychain := sandbox.NewOSKeychain(filepath.Join(t.TempDir(), "keycard", "sandbox.yaml"))
	if err := keychain.Set("github-token", "ghp"); err != nil {
		t.Fatal(err)
	}
	if secret, err := keychain.Get("github-token"); err != nil || secret != "ghp" {
		t.Errorf("Get = %q, %v; want the secret from the file", secret, err)
	}
	if info, err := os.Stat(keychain.Fallback); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("%s: %v, want mode 0600", keychain.Fallback, err)
	}
	if where := keychain.Where(); !strings.Contains(where, "because this system has no keychain") {
		t.Errorf("Where = %q, want it to say why the file", where)
	}
	if err := keychain.Delete("github-token"); err != nil {
		t.Fatal(err)
	}
	if _, err := keychain.Get("github-token"); !errors.Is(err, sandbox.ErrNoSecret) {
		t.Errorf("Get after Delete = %v, want ErrNoSecret", err)
	}
}

// errDenied is the error a locked keychain gives.
var errDenied = errors.New("user interaction is not allowed")

// TestKeychainRefuses returns the keychain's refusal, such as a locked
// keychain or an operator who denies access, and never writes the secret to
// the fallback file instead.
//
//nolint:paralleltest // keyring's mock is global.
func TestKeychainRefuses(t *testing.T) {
	keyring.MockInitWithError(errDenied)
	t.Cleanup(keyring.MockInit)
	keychain := sandbox.NewOSKeychain(filepath.Join(t.TempDir(), "keycard", "sandbox.yaml"))
	err := keychain.Set("github-token", "ghp")
	checkErr(t, err, []error{sandbox.ErrKeychain, errDenied}, "KEYCARD_SANDBOX_SECRET_GITHUB_TOKEN")
	if _, statErr := os.Stat(keychain.Fallback); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("stat %s: %v, want no fallback file", keychain.Fallback, statErr)
	}
	_, err = keychain.Get("github-token")
	checkErr(t, err, []error{sandbox.ErrKeychain, errDenied}, "reading github-token")
	err = keychain.Delete("github-token")
	checkErr(t, err, []error{sandbox.ErrKeychain, errDenied}, "deleting github-token")
}

// TestKeychainPerFile keeps the secrets of each settings file apart, so
// that saving or forgetting one file's settings leaves another's secrets.
// The default file keeps its secrets as sandbox/<name>.
func TestKeychainPerFile(t *testing.T) {
	const mainSecret = "ghp-main"
	keyring.MockInit()
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", base)
	main := sandbox.NewOSKeychain(filepath.Join(base, "keycard", "sandbox.yaml"))
	otherDir := t.TempDir()
	other := sandbox.NewOSKeychain(filepath.Join(otherDir, "other.yaml"))
	if want := filepath.Join(base, "keycard", "sandbox-secrets.yaml"); main.Fallback != want {
		t.Errorf("the default file's fallback = %s, want %s", main.Fallback, want)
	}
	if want := filepath.Join(otherDir, "other-secrets.yaml"); other.Fallback != want {
		t.Errorf("other.yaml's fallback = %s, want %s", other.Fallback, want)
	}
	for keychain, secret := range map[*sandbox.OSKeychain]string{main: mainSecret, other: "ghp-other"} {
		if err := keychain.Set("github-token", secret); err != nil {
			t.Fatal(err)
		}
	}
	if err := other.Delete("github-token"); err != nil {
		t.Fatal(err)
	}
	if secret, err := main.Get("github-token"); err != nil || secret != mainSecret {
		t.Errorf("the default file's secret = %q, %v; want ghp-main", secret, err)
	}
	if secret, err := keyring.Get("keycard", "sandbox/github-token"); err != nil || secret != mainSecret {
		t.Errorf("sandbox/github-token = %q, %v; want the default file's secret", secret, err)
	}
}
