// Package zonesetup reads the values file zone-setup configures a zone from
// and resolves that zone through the management API.
package zonesetup

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/mail"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	yamlv2 "go.yaml.in/yaml/v2"
	kjson "sigs.k8s.io/json"
	"sigs.k8s.io/yaml"
)

// The error classes the package returns. Every error wraps one of them.
var (
	// ErrValues is a values file that cannot be used as written.
	ErrValues = errors.New("values file")
	// ErrMissingSecret is a credential the values file names but the
	// environment or filesystem does not hold.
	ErrMissingSecret = errors.New("missing secret")
	// ErrAuth is a service account the token endpoint did not accept.
	ErrAuth = errors.New("service account token")
	// ErrAPI is a management API request that failed.
	ErrAPI = errors.New("management API")
	// ErrZoneNotFound is a zone_id or zone_url that names no zone the service
	// account can see.
	ErrZoneNotFound = errors.New("zone not found")
	// ErrZoneAmbiguous is a zone_url more than one zone issues from.
	ErrZoneAmbiguous = errors.New("zone ambiguous")
	// ErrZoneMismatch is a zone_id and a zone_url that name different zones.
	ErrZoneMismatch = errors.New("zone_id and zone_url disagree")
)

const (
	// DefaultManagementSecretEnv is the environment variable that holds the
	// service account secret when the values file names no other source.
	DefaultManagementSecretEnv = "KEYCARD_MANAGEMENT_CLIENT_SECRET"
	// DefaultNamespace is the namespace the sandboxes run in when the values
	// file names none.
	DefaultNamespace = "agents"
	// maxNamespaceLength is the length limit of a DNS-1123 label.
	maxNamespaceLength = 63
)

var namespaceRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// Values is the values file. It holds no credential. Every secret is named by
// the environment variable or file that holds it.
type Values struct {
	// ManagementAPIURL is the management API, a separate host from the zone.
	ManagementAPIURL string `json:"management_api_url"`
	// ZoneURL is the zone's issuer URL.
	ZoneURL string `json:"zone_url"`
	// ZoneID names the zone directly.
	ZoneID string `json:"zone_id"`
	// ServiceAccount is the organization service account zone-setup
	// authenticates with. It is NEVER the sidecar's identity.
	ServiceAccount ServiceAccountValues `json:"service_account"`
	// IssuerURL is the cluster's OIDC issuer, kept byte for byte.
	IssuerURL string `json:"issuer_url"`
	// Namespace is the sandbox namespace. It defaults to DefaultNamespace.
	Namespace string `json:"namespace"`
	// Users are the sandbox users, one per email.
	Users []UserValues `json:"users"`
	// Resources are the destinations the proxy mints credentials for.
	Resources []ResourceValues `json:"resources"`
}

// ServiceAccountValues is the organization service account.
type ServiceAccountValues struct {
	ClientID string `json:"client_id"`
	// ClientSecretEnv names the environment variable holding the secret. It
	// defaults to DefaultManagementSecretEnv when no file is named.
	ClientSecretEnv string `json:"client_secret_env"`
	// ClientSecretFile names a file holding the secret. When set, it is the
	// only source read.
	ClientSecretFile string `json:"client_secret_file"`
}

// UserValues is one sandbox user.
type UserValues struct {
	Email string `json:"email"`
}

// ResourceValues is one destination. Exactly one of CredentialProvider and
// SecretEnv is set.
type ResourceValues struct {
	// Identifier is what the proxy sends as the exchange's `resource`
	// parameter, verbatim.
	Identifier string `json:"identifier"`
	// Name is the display name. It defaults to Identifier.
	Name string `json:"name"`
	// Prefix makes the identifier match every URL it is a prefix of.
	Prefix bool `json:"prefix"`
	// CredentialProvider is the identifier of an existing provider in the zone.
	CredentialProvider string `json:"credential_provider"`
	// SecretEnv names the environment variable holding a static token for
	// this resource.
	SecretEnv string `json:"secret_env"`
}

// Secrets holds every credential value the values file names.
type Secrets struct {
	ManagementClientSecret string
	// Resource maps a SecretEnv name to its value.
	Resource map[string]string
}

// ParseValues reads and validates a values file, YAML or JSON, and applies
// defaults. The file holds exactly one document. Unknown keys, duplicate keys and keys that differ from a field
// name only in case are errors. Every error wraps ErrValues.
func ParseValues(reader io.Reader) (*Values, error) {
	raw, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrValues, err)
	}
	if strings.TrimSpace(string(raw)) == "" {
		return nil, fmt.Errorf("%w: empty", ErrValues)
	}
	if err = singleDocument(raw); err != nil {
		return nil, err
	}
	data, err := yaml.YAMLToJSONStrict(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrValues, err)
	}
	var values Values
	strict, err := kjson.UnmarshalStrict(data, &values, kjson.DisallowUnknownFields)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrValues, err)
	}
	if len(strict) > 0 {
		return nil, fmt.Errorf("%w: %w", ErrValues, errors.Join(strict...))
	}
	steps := []func() error{values.checkConnection, values.checkSandbox, values.checkUsers, values.checkResources}
	for _, step := range steps {
		if err := step(); err != nil {
			return nil, err
		}
	}
	return &values, nil
}

// singleDocument refuses a stream that holds more than one YAML document.
// Every "---" marker starts a document, even an empty one. A trailing comment
// or "..." marker starts none.
func singleDocument(raw []byte) error {
	decoder := yamlv2.NewDecoder(bytes.NewReader(raw))
	var doc any
	if err := decoder.Decode(&doc); err != nil {
		return fmt.Errorf("%w: %w", ErrValues, err)
	}
	err := decoder.Decode(&doc)
	switch {
	case err == nil:
		return fmt.Errorf("%w: holds more than one document", ErrValues)
	case !errors.Is(err, io.EOF):
		return fmt.Errorf("%w: content after the first document: %w", ErrValues, err)
	}
	return nil
}

func (v *Values) checkConnection() error {
	if err := checkURL("management_api_url", v.ManagementAPIURL); err != nil {
		return err
	}
	if v.ZoneURL == "" && v.ZoneID == "" {
		return fmt.Errorf("%w: one of zone_url or zone_id is required", ErrValues)
	}
	if v.ZoneURL != "" {
		if err := checkURL("zone_url", v.ZoneURL); err != nil {
			return err
		}
	}
	if v.ServiceAccount.ClientID == "" {
		return fmt.Errorf("%w: service_account.client_id is required", ErrValues)
	}
	if v.ServiceAccount.ClientSecretEnv == "" && v.ServiceAccount.ClientSecretFile == "" {
		v.ServiceAccount.ClientSecretEnv = DefaultManagementSecretEnv
	}
	return nil
}

func (v *Values) checkSandbox() error {
	if err := checkURL("issuer_url", v.IssuerURL); err != nil {
		return err
	}
	if v.Namespace == "" {
		v.Namespace = DefaultNamespace
	}
	if !namespaceRE.MatchString(v.Namespace) || len(v.Namespace) > maxNamespaceLength {
		return fmt.Errorf("%w: namespace %q is not a DNS-1123 label", ErrValues, v.Namespace)
	}
	return nil
}

func (v *Values) checkUsers() error {
	seen := map[string]bool{}
	for i, user := range v.Users {
		if parsed, err := mail.ParseAddress(user.Email); err != nil || parsed.Address != user.Email {
			return fmt.Errorf("%w: users[%d].email %q is not a bare email address", ErrValues, i, user.Email)
		}
		if seen[user.Email] {
			return fmt.Errorf("%w: users[%d].email %q is listed twice", ErrValues, i, user.Email)
		}
		seen[user.Email] = true
	}
	return nil
}

func (v *Values) checkResources() error {
	seen := map[string]bool{}
	for i := range v.Resources {
		resource := &v.Resources[i]
		if resource.Identifier == "" {
			return fmt.Errorf("%w: resources[%d].identifier is required", ErrValues, i)
		}
		if seen[resource.Identifier] {
			return fmt.Errorf("%w: resources[%d].identifier %q is listed twice", ErrValues, i, resource.Identifier)
		}
		seen[resource.Identifier] = true
		if resource.Name == "" {
			resource.Name = resource.Identifier
		}
		if (resource.CredentialProvider == "") == (resource.SecretEnv == "") {
			return fmt.Errorf("%w: resources[%d]: set exactly one of credential_provider and secret_env", ErrValues, i)
		}
	}
	return nil
}

// checkURL requires an absolute https URL with a host, no userinfo and either
// no port or a valid one. Only a loopback host may use http. NEVER put the value in an error, because
// a URL can carry a credential.
func checkURL(key, value string) error {
	if value == "" {
		return fmt.Errorf("%w: %s is required", ErrValues, key)
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" {
		return fmt.Errorf("%w: %s is not an absolute URL with a host and a valid port", ErrValues, key)
	}
	if parsed.User != nil {
		return fmt.Errorf("%w: %s must not carry userinfo", ErrValues, key)
	}
	if strings.HasSuffix(parsed.Host, ":") || !validPort(parsed.Port()) {
		return fmt.Errorf("%w: %s port must be 1-65535", ErrValues, key)
	}
	if parsed.Scheme == "https" || parsed.Scheme == "http" && isLoopback(parsed.Hostname()) {
		return nil
	}
	return fmt.Errorf("%w: %s must use https unless its host is loopback", ErrValues, key)
}

// validPort accepts an absent port or a number from 1 to 65535.
func validPort(port string) bool {
	if port == "" {
		return true
	}
	n, err := strconv.Atoi(port)
	return err == nil && n >= 1 && n <= 65535
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// LoadSecrets reads every credential the values file names, and refuses when
// any is missing or empty. A secret file keeps its exact bytes less one
// trailing line ending.
func LoadSecrets(
	values *Values,
	lookupEnv func(string) (string, bool),
	readFile func(string) ([]byte, error),
) (*Secrets, error) {
	management, err := managementSecret(values.ServiceAccount, lookupEnv, readFile)
	if err != nil {
		return nil, err
	}
	resources := map[string]string{}
	var missing []string
	for _, resource := range values.Resources {
		if resource.SecretEnv == "" {
			continue
		}
		value, ok := lookupEnv(resource.SecretEnv)
		if !ok || value == "" {
			missing = append(missing, fmt.Sprintf("%s (for %s)", resource.SecretEnv, resource.Identifier))
			continue
		}
		resources[resource.SecretEnv] = value
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: unset: %s", ErrMissingSecret, strings.Join(missing, ", "))
	}
	return &Secrets{ManagementClientSecret: management, Resource: resources}, nil
}

func managementSecret(
	account ServiceAccountValues,
	lookupEnv func(string) (string, bool),
	readFile func(string) ([]byte, error),
) (string, error) {
	if account.ClientSecretFile != "" {
		raw, err := readFile(account.ClientSecretFile)
		if err != nil {
			return "", fmt.Errorf("%w: service_account.client_secret_file: %w", ErrMissingSecret, err)
		}
		secret, found := strings.CutSuffix(string(raw), "\r\n")
		if !found {
			secret = strings.TrimSuffix(secret, "\n")
		}
		if secret == "" {
			return "", fmt.Errorf("%w: service_account.client_secret_file %s is empty",
				ErrMissingSecret, account.ClientSecretFile)
		}
		return secret, nil
	}
	secret, ok := lookupEnv(account.ClientSecretEnv)
	if !ok || secret == "" {
		return "", fmt.Errorf(
			"%w: service_account.client_secret_env %s is unset",
			ErrMissingSecret,
			account.ClientSecretEnv,
		)
	}
	return secret, nil
}
