package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/zalando/go-keyring"
	yaml "go.yaml.in/yaml/v3"
)

// Keychain keeps secrets between runs, by name.
type Keychain interface {
	// Get returns ErrNoSecret for a name it does not keep.
	Get(name string) (string, error)
	Set(name, secret string) error
	Delete(name string) error
	// Where names where the secrets are, in a sentence.
	Where() string
}

var (
	// ErrNoSecret means the keychain does not keep the secret.
	ErrNoSecret = errors.New("no such secret")
	// ErrKeychain means the keychain refused to read, save or delete a
	// secret.
	ErrKeychain = errors.New("the keychain refused")
)

// Store keeps what setup asked: the config and the secrets. It keeps them
// for the session, and saves them to the config file and the keychain when
// the operator agrees. Copies of a Local share it.
type Store struct {
	mu       sync.Mutex
	config   *Config
	path     string
	saved    bool
	typed    map[string]string
	keychain Keychain
}

// NewStore returns a store that saves to the config file at path, or saves
// nothing for NoConfig. config is what the file holds, if anything, or the
// settings of the session for NoConfig. keychain keeps the secrets.
func NewStore(path string, config *Config, keychain Keychain) *Store {
	saved := config != nil && path != NoConfig && path != ""
	return &Store{config: config, path: path, saved: saved, typed: map[string]string{}, keychain: keychain}
}

// Config returns the config, or nil before setup.
func (s *Store) Config() *Config {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.config
}

// Path returns the config file the store saves to, or NoConfig.
func (s *Store) Path() string {
	if s == nil {
		return NoConfig
	}
	return s.path
}

// Saved reports whether the config file holds the config.
func (s *Store) Saved() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saved
}

// CanSave reports whether the store has a file to save to.
func (s *Store) CanSave() bool {
	return s != nil && s.path != NoConfig && s.path != "" && s.keychain != nil
}

// KeychainWhere names where saved secrets go, in a sentence.
func (s *Store) KeychainWhere() string {
	if s == nil || s.keychain == nil {
		return "nowhere"
	}
	return s.keychain.Where()
}

// Forget deletes the config file, and from the keychain the secrets the
// config names and the known ones. It returns how many secrets it deleted.
// The session keeps the settings and the secrets the config names.
func (s *Store) Forget(known ...string) (int, error) {
	if !s.CanSave() {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var named []string
	if s.config != nil {
		named = s.config.secretNames()
	}
	if err := s.keepTyped(named); err != nil {
		return 0, err
	}
	deleted := 0
	for _, name := range slices.Compact(slices.Sorted(slices.Values(slices.Concat(named, known)))) {
		switch err := s.keychain.Delete(name); {
		case err == nil:
			deleted++
		case !errors.Is(err, ErrNoSecret):
			return deleted, fmt.Errorf("deleting %s from %s: %w", name, s.keychain.Where(), err)
		}
	}
	if err := os.Remove(s.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return deleted, fmt.Errorf("deleting %s: %w", s.path, err)
	}
	s.saved = false
	return deleted, nil
}

// keepTyped reads each named secret from the keychain into the session,
// unless the session has it already. The caller holds s.mu.
func (s *Store) keepTyped(names []string) error {
	for _, name := range names {
		if _, typed := s.typed[name]; typed {
			continue
		}
		secret, err := s.keychain.Get(name)
		switch {
		case err == nil:
			s.typed[name] = secret
		case !errors.Is(err, ErrNoSecret):
			return err //nolint:wrapcheck // The keychains name the secret and the keychain.
		}
	}
	return nil
}

func (s *Store) setConfig(config *Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.config = config
}

// secret returns the secret name: one typed this session, or else the one
// the keychain keeps. It returns ErrNoSecret for a secret it does not have.
func (s *Store) secret(name string) (string, error) {
	if s == nil || name == "" {
		return "", fmt.Errorf("%w: %q", ErrNoSecret, name)
	}
	s.mu.Lock()
	secret, typed := s.typed[name]
	keychain := s.keychain
	s.mu.Unlock()
	switch {
	case typed:
		return secret, nil
	case keychain == nil:
		return "", fmt.Errorf("%w: %s", ErrNoSecret, name)
	}
	return keychain.Get(name) //nolint:wrapcheck // The keychains name the secret and the keychain.
}

// setSecret keeps a secret typed this session.
func (s *Store) setSecret(name, secret string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.typed[name] = secret
}

// save puts the secrets typed this session in the keychain, then writes
// the config to the config file. When a step fails, it puts back the
// secrets it replaced, so the keychain and the file still agree.
func (s *Store) save() error {
	if !s.CanSave() {
		return fmt.Errorf("%w: there is no config file to save to", ErrSettings)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.config == nil {
		return fmt.Errorf("%w: there are no settings to save", ErrSettings)
	}
	var replaced []savedSecret
	err := s.saveSecrets(&replaced)
	if err == nil {
		err = s.config.save(s.path)
	}
	if err != nil {
		return errors.Join(err, s.restore(replaced))
	}
	s.saved = true
	return nil
}

// savedSecret is a secret save replaced, and what the keychain held before.
type savedSecret struct {
	name, before string
	had          bool
}

// saveSecrets puts each typed secret in the keychain and adds what it held
// before to replaced. The caller holds s.mu.
func (s *Store) saveSecrets(replaced *[]savedSecret) error {
	for _, name := range slices.Sorted(maps.Keys(s.typed)) {
		before, err := s.keychain.Get(name)
		had := err == nil
		if err != nil && !errors.Is(err, ErrNoSecret) {
			return err //nolint:wrapcheck // The keychains name the secret and the keychain.
		}
		if err = s.keychain.Set(name, s.typed[name]); err != nil {
			return err //nolint:wrapcheck // The keychains name the secret and the keychain.
		}
		*replaced = append(*replaced, savedSecret{name: name, before: before, had: had})
	}
	return nil
}

// restore puts back the secrets save replaced. The caller holds s.mu.
func (s *Store) restore(replaced []savedSecret) error {
	var errs []error
	for _, old := range replaced {
		var err error
		if old.had {
			err = s.keychain.Set(old.name, old.before)
		} else if err = s.keychain.Delete(old.name); errors.Is(err, ErrNoSecret) {
			err = nil
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("putting back %s: %w", old.name, err))
		}
	}
	return errors.Join(errs...)
}

// keychainService is the service the OS keychain keeps the secrets under,
// and keychainPrefix starts the name of each.
const keychainService, keychainPrefix = "keycard", "sandbox/"

// OSKeychain is the OS keychain: the macOS Keychain, the Secret Service on
// Linux, or the Windows Credential Manager. It keeps each secret under the
// service keycard, as prefix and the secret's name. On a platform the
// keychain library does not support, it keeps them in the file Fallback,
// readable by its owner alone, and Where says so.
type OSKeychain struct {
	Fallback string
	prefix   string
	mu       sync.Mutex
	inFile   bool
}

// NewOSKeychain returns the keychain of the settings file at configPath.
// Its fallback file sits next to that file: sandbox.yaml falls back to
// sandbox-secrets.yaml. The default settings
// file keeps its secrets as sandbox/<name>. Another file keeps its own as
// sandbox/<id>/<name>, where <id> is the first 8 hex digits of a SHA-256 of
// its path.
func NewOSKeychain(configPath string) *OSKeychain {
	prefix := keychainPrefix
	if path, err := DefaultConfigPath(os.Getenv); err != nil || filepath.Clean(configPath) != path {
		sum := sha256.Sum256([]byte(filepath.Clean(configPath)))
		prefix += hex.EncodeToString(sum[:])[:8] + "/"
	}
	name := strings.TrimSuffix(filepath.Base(configPath), filepath.Ext(configPath)) + "-secrets.yaml"
	return &OSKeychain{Fallback: filepath.Join(filepath.Dir(configPath), name), prefix: prefix}
}

// Get reads a secret from the keychain, or from the fallback file on a
// system with no keychain.
func (k *OSKeychain) Get(name string) (string, error) {
	secret, err := keyring.Get(keychainService, k.prefix+name)
	switch {
	case err == nil:
		return secret, nil
	case errors.Is(err, keyring.ErrNotFound):
		return "", fmt.Errorf("%w: %s", ErrNoSecret, name)
	case !errors.Is(err, keyring.ErrUnsupportedPlatform):
		return "", k.failed("reading", name, err)
	}
	secrets, err := readSecretFile(k.Fallback)
	if err != nil {
		return "", err
	}
	if secret, ok := secrets[name]; ok {
		return secret, nil
	}
	return "", fmt.Errorf("%w: %s", ErrNoSecret, name)
}

// Set puts a secret in the keychain, or in the fallback file on a system
// with no keychain. When the keychain refuses, as it does when it is locked
// or the operator denies access, Set returns the error and writes nothing.
func (k *OSKeychain) Set(name, secret string) error {
	err := keyring.Set(keychainService, k.prefix+name, secret)
	switch {
	case err == nil:
		return nil
	case !errors.Is(err, keyring.ErrUnsupportedPlatform):
		return k.failed("saving", name, err)
	}
	k.mu.Lock()
	k.inFile = true
	k.mu.Unlock()
	secrets, err := readSecretFile(k.Fallback)
	if err != nil {
		return err
	}
	secrets[name] = secret
	return writeSecretFile(k.Fallback, secrets)
}

// Delete removes a secret from the keychain, or from the fallback file on a
// system with no keychain. It returns ErrNoSecret only for a secret neither
// keeps.
func (k *OSKeychain) Delete(name string) error {
	err := keyring.Delete(keychainService, k.prefix+name)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, keyring.ErrNotFound):
		return fmt.Errorf("%w: %s", ErrNoSecret, name)
	case !errors.Is(err, keyring.ErrUnsupportedPlatform):
		return k.failed("deleting", name, err)
	}
	secrets, err := readSecretFile(k.Fallback)
	if err != nil {
		return err
	}
	if _, ok := secrets[name]; !ok {
		return fmt.Errorf("%w: %s", ErrNoSecret, name)
	}
	delete(secrets, name)
	return writeSecretFile(k.Fallback, secrets)
}

// Where names the keychain, or the fallback file once a secret went there.
func (k *OSKeychain) Where() string {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.inFile {
		return HomeRelative(k.Fallback) + ", readable only by you, because this system has no keychain"
	}
	switch runtime.GOOS {
	case "darwin":
		return "the macOS keychain"
	case "windows":
		return "the Windows Credential Manager"
	}
	return "the system keychain"
}

// failed describes a keychain error and what to do about it.
func (k *OSKeychain) failed(doing, name string, err error) error {
	return fmt.Errorf("%w: %s %s in %s: %w. Unlock the keychain and allow access, "+
		"or give the secret as %s and do not save it", ErrKeychain, doing, name, k.Where(), err, secretEnv(name))
}

// readSecretFile reads the fallback file. For a missing file, it returns an
// empty map.
func readSecretFile(path string) (map[string]string, error) {
	secrets := map[string]string{}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the path is next to this program's config file.
	if errors.Is(err, fs.ErrNotExist) {
		return secrets, nil
	}
	if err == nil {
		err = yaml.Unmarshal(raw, &secrets)
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return secrets, nil
}

func writeSecretFile(path string, secrets map[string]string) error {
	raw, err := yaml.Marshal(secrets)
	if err != nil {
		return fmt.Errorf("encoding the secrets: %w", err)
	}
	return writePrivate(path, raw)
}
