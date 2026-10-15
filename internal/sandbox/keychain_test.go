package sandbox_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
)

// errDenied is the error a locked keychain gives.
var errDenied = errors.New("user interaction is not allowed")

// TestKeychainRefuses returns the keychain's refusal, such as a locked
// keychain or an operator who denies access. keyring's mock is global, so
// the keychain tests do not run in parallel.
//
//nolint:paralleltest // keyring's mock is global.
func TestKeychainRefuses(t *testing.T) {
	keyring.MockInitWithError(errDenied)
	t.Cleanup(keyring.MockInit)
	keychain := sandbox.NewOSKeychain(filepath.Join(t.TempDir(), "keycard", "sandbox.yaml"))
	err := keychain.Set("github-token", "ghp")
	checkErr(t, err, []error{sandbox.ErrKeychain, errDenied}, "KEYCARD_SANDBOX_SECRET_GITHUB_TOKEN")
	_, err = keychain.Get("github-token")
	checkErr(t, err, []error{sandbox.ErrKeychain, errDenied}, "reading github-token")
	err = keychain.Delete("github-token")
	checkErr(t, err, []error{sandbox.ErrKeychain, errDenied}, "deleting github-token")
}

// TestKeychainCheck finds a working keychain by reading a secret it does
// not keep, and returns the keychain's error for one that does not work.
//
//nolint:paralleltest // keyring's mock is global.
func TestKeychainCheck(t *testing.T) {
	keyring.MockInit()
	keychain := sandbox.NewOSKeychain(filepath.Join(t.TempDir(), "keycard", "sandbox.yaml"))
	if err := keychain.Check(); err != nil {
		t.Errorf("Check = %v, want nil for a working keychain", err)
	}
	keyring.MockInitWithError(errDenied)
	t.Cleanup(keyring.MockInit)
	checkErr(t, keychain.Check(), []error{errDenied}, "does not work")
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
	other := sandbox.NewOSKeychain(filepath.Join(t.TempDir(), "other.yaml"))
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
