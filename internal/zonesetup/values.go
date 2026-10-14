// Package zonesetup reads the values file zone-setup configures a zone from
// and resolves that zone through the management API. It finds or creates the
// cluster's provider, the egress proxy Application, the resources in it, each
// user's token credential, impersonation policy and access policy, and the
// policy set that holds those policies.
package zonesetup

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/mail"
	"net/url"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"

	yamlv3 "go.yaml.in/yaml/v3"
)

// The error classes the package returns. Every error wraps one of them.
var (
	// ErrAPI is a management API request that failed.
	ErrAPI = errors.New("management API")
	// ErrAuth is a service account the token endpoint did not accept.
	ErrAuth = errors.New("service account token")
	// ErrRefused is a client ID and secret the token endpoint refused with
	// HTTP 400 or 401. An error that wraps ErrRefused also wraps ErrAuth.
	ErrRefused = errors.New("refused")
	// ErrApplicationAmbiguous is an application identifier more than one
	// application in the zone has.
	ErrApplicationAmbiguous = errors.New("application ambiguous")
	// ErrCredentialWildcard is a token credential on the Application for the
	// cluster's provider whose subject ends in "*". The zone matches it against
	// every subject with that prefix, so it binds more than one
	// ServiceAccount.
	ErrCredentialWildcard = errors.New("token credential with a wildcard subject")
	// ErrMissingSecret is a credential the values file names but the
	// environment or filesystem does not hold.
	ErrMissingSecret = errors.New("missing secret")
	// ErrPolicyAmbiguous is a policy name more than one unarchived policy in
	// the zone has.
	ErrPolicyAmbiguous = errors.New("policy ambiguous")
	// ErrPolicySetActive is a zone whose active policy set is not the sandbox
	// set.
	ErrPolicySetActive = errors.New("another policy set is active")
	// ErrPolicySetAmbiguous is a policy set name more than one unarchived
	// policy set in the zone has.
	ErrPolicySetAmbiguous = errors.New("policy set ambiguous")
	// ErrProviderAmbiguous is an issuer_url, a credential_provider or the
	// keycard-vault type that more than one provider in the zone has.
	ErrProviderAmbiguous = errors.New("provider ambiguous")
	// ErrProviderMismatch is a provider whose identifier is issuer_url but whose
	// issuer is not. The identifier is unique in the zone, so zone-setup can
	// neither use that provider nor create its own beside it.
	ErrProviderMismatch = errors.New("provider issuer mismatch")
	// ErrProviderNotFound is a credential provider a resource needs but the
	// zone does not have.
	ErrProviderNotFound = errors.New("provider not found")
	// ErrResourceAmbiguous is a resource identifier more than one resource
	// in the zone has.
	ErrResourceAmbiguous = errors.New("resource ambiguous")
	// ErrValues is a values file that cannot be used as written.
	ErrValues = errors.New("values file")
	// ErrUserAmbiguous is an email more than one user in the zone has.
	ErrUserAmbiguous = errors.New("user ambiguous")
	// ErrUserNotFound is an email no user in the zone has. The management API
	// cannot create a user.
	ErrUserNotFound = errors.New("user not found")
	// ErrZoneAmbiguous is a zone_url more than one zone issues from.
	ErrZoneAmbiguous = errors.New("zone ambiguous")
	// ErrZoneMismatch is a zone_id and a zone_url that name different zones.
	ErrZoneMismatch = errors.New("zone_id and zone_url disagree")
	// ErrZoneNotFound is a zone_id or zone_url that names no zone the service
	// account can see.
	ErrZoneNotFound = errors.New("zone not found")
)

const (
	// DefaultManagementSecretEnv is the environment variable that holds the
	// service account secret when the values file names no other source.
	DefaultManagementSecretEnv = "KEYCARD_MANAGEMENT_CLIENT_SECRET"
	// DefaultNamespace is the default sandbox namespace used when not configured in the values file.
	DefaultNamespace = "agents"
	// DefaultPolicySetName names the zone-wide policy set for every enrolled
	// user's impersonation and access policies when the values file names none.
	DefaultPolicySetName = "Kubernetes Sandbox"
	// maxNamespaceLength is the length limit of a DNS-1123 label.
	maxNamespaceLength = 63
)

var (
	envNameRE   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	namespaceRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	plainKeyRE  = regexp.MustCompile(`^[A-Za-z0-9_.\[\]-]*$`)
	repeatedRE  = regexp.MustCompile(
		`(?s)^line \d+: (?:mapping key .* already defined at line \d+|field .* already set in (?:map|type) \S+)$`,
	)
	typeLineRE  = regexp.MustCompile(`^line (\d+): `)
	unknownRE   = regexp.MustCompile(`(?s)^line \d+: field (.*) not found in type \S+$`)
	wrongTypeRE = regexp.MustCompile(`^line \d+: cannot unmarshal `)
	yamlLineRE  = regexp.MustCompile(`^yaml: line (\d+):`)
)

// Values is the values file. It holds no credential. Every secret is named by
// the environment variable or file that holds it. Marshal leaves out the empty
// optional keys.
type Values struct {
	// ManagementAPIURL is the management API, a separate host from the zone.
	ManagementAPIURL string `yaml:"management_api_url"`
	// ZoneURL is the zone's issuer URL.
	ZoneURL string `yaml:"zone_url,omitempty"`
	// ZoneID names the zone directly.
	ZoneID string `yaml:"zone_id,omitempty"`
	// ManagementServiceAccount is the organization service account zone-setup
	// authenticates with. It is NEVER the sidecar's identity.
	ManagementServiceAccount ManagementServiceAccountValues `yaml:"management_service_account"`
	// IssuerURL is the cluster's OIDC issuer, kept byte for byte.
	IssuerURL string `yaml:"issuer_url,omitempty"`
	// Namespace is the sandbox namespace. It defaults to DefaultNamespace.
	Namespace string `yaml:"namespace,omitempty"`
	// PolicySet names the zone-wide policy set the per-user policies go in.
	// It defaults to DefaultPolicySetName.
	PolicySet string `yaml:"policy_set,omitempty"`
	// Users are the sandbox users, one per email.
	Users []UserValues `yaml:"users"`
	// Resources are the destinations the proxy mints credentials for.
	Resources []ResourceValues `yaml:"resources,omitempty"`
}

// ManagementServiceAccountValues is the organization service account.
type ManagementServiceAccountValues struct {
	ClientID string `yaml:"client_id"`
	// ClientSecretEnv names the environment variable holding the secret. It
	// defaults to DefaultManagementSecretEnv when no file is named.
	ClientSecretEnv string `yaml:"client_secret_env,omitempty"`
	// ClientSecretFile names a file holding the secret. When set, it is the
	// only source read.
	ClientSecretFile string `yaml:"client_secret_file,omitempty"`
}

// UserValues is one sandbox user.
type UserValues struct {
	Email string `yaml:"email"`
}

// ResourceValues is one destination. Exactly one of CredentialProvider,
// SecretEnv and SecretFile is set.
type ResourceValues struct {
	// Identifier must equal the canonical url the proxy sends as the
	// exchange's `resource` parameter. The zone matches it exactly.
	Identifier string `yaml:"identifier"`
	// Name is the display name. It defaults to Identifier.
	Name string `yaml:"name,omitempty"`
	// Prefix makes the identifier match every URL it is a prefix of.
	Prefix bool `yaml:"prefix"`
	// CredentialProvider is the identifier of an existing provider in the zone.
	CredentialProvider string `yaml:"credential_provider,omitempty"`
	// SecretEnv names the environment variable holding a static token for
	// this resource.
	SecretEnv string `yaml:"secret_env,omitempty"`
	// SecretFile names a file holding a static token for this resource.
	// LoadSecrets trims the whitespace around the token.
	SecretFile string `yaml:"secret_file,omitempty"`
}

// Static reports whether the resource uses a static token stored in the
// zone's vault.
func (r ResourceValues) Static() bool {
	return r.SecretEnv != "" || r.SecretFile != ""
}

// SecretName returns the name of the resource's static token in the zone's
// vault: SecretEnv, or the base name of SecretFile.
func (r ResourceValues) SecretName() string {
	if r.SecretEnv != "" {
		return r.SecretEnv
	}
	return filepath.Base(r.SecretFile)
}

// Secrets holds every credential value the values file names.
type Secrets struct {
	ManagementClientSecret string
	// Resource maps a static resource's identifier to its token.
	Resource map[string]string
}

// ParseValues reads and validates a values file, YAML or JSON, and applies
// defaults. The file holds exactly one document. Unknown keys, duplicate keys and keys that differ from a field
// name only in case are errors. Every error wraps ErrValues.
func ParseValues(reader io.Reader) (*Values, error) {
	return parseValues(reader, "", "")
}

// parseValues parses like ParseValues. When issuerURL is not empty, it sets
// issuer_url to issuerURL. source names where issuerURL came from, for errors.
func parseValues(reader io.Reader, issuerURL, source string) (*Values, error) {
	raw, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot read it", ErrValues)
	}
	if strings.TrimSpace(string(raw)) == "" {
		return nil, fmt.Errorf("%w: empty", ErrValues)
	}
	if err = singleDocument(raw); err != nil {
		return nil, err
	}
	var values Values
	decoder := yamlv3.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err = decoder.Decode(&values); err != nil && !errors.Is(err, io.EOF) {
		return nil, parseError(err)
	}
	if issuerURL != "" {
		if err = checkIssuer(source, issuerURL); err != nil {
			return nil, err
		}
		values.IssuerURL = issuerURL
	}
	steps := []func() error{
		values.checkConnection,
		values.checkManagementServiceAccount,
		values.checkSandbox,
		values.checkUsers,
		values.checkResources,
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return nil, err
		}
	}
	return &values, nil
}

// singleDocument refuses a stream that holds more than one YAML document, and
// a document that uses an anchor, an alias or a merge key. Every "---" marker
// starts a document, even an empty one. A trailing comment or "..." marker
// starts none.
func singleDocument(raw []byte) error {
	decoder := yamlv3.NewDecoder(bytes.NewReader(raw))
	var doc yamlv3.Node
	if err := decoder.Decode(&doc); err != nil && !errors.Is(err, io.EOF) {
		return parseError(err)
	}
	if err := plainStructure(&doc); err != nil {
		return err
	}
	if len(doc.Content) > 0 && !isNull(doc.Content[0]) {
		if err := checkKinds(doc.Content[0], reflect.TypeFor[Values]()); err != nil {
			return err
		}
	}
	var next yamlv3.Node
	err := decoder.Decode(&next)
	switch {
	case err == nil:
		return fmt.Errorf("%w: holds more than one document", ErrValues)
	case !errors.Is(err, io.EOF):
		return fmt.Errorf("%w: content after the first document: %w", ErrValues, parseError(err))
	}
	return nil
}

// plainStructure refuses anchors, aliases and merge keys, which the values
// file has no use for, and which apply keys the duplicate check never sees.
// It refuses a null key, which the decoder skips without an error.
func plainStructure(node *yamlv3.Node) error {
	if node.Kind == yamlv3.AliasNode || node.Anchor != "" {
		return fmt.Errorf("%w: line %d: anchors and aliases are not allowed", ErrValues, node.Line)
	}
	for i, child := range node.Content {
		if node.Kind == yamlv3.MappingNode && i%2 == 0 {
			if err := checkKey(child); err != nil {
				return err
			}
		}
		if err := plainStructure(child); err != nil {
			return err
		}
	}
	return nil
}

// checkKinds refuses a value whose kind or tag does not fit the field it
// fills, because the decoder coerces a scalar into a string or bool field. A
// null value leaves its field unset. A key that names no field is left for the
// decoder to report.
func checkKinds(node *yamlv3.Node, typ reflect.Type) error {
	if isNull(node) {
		return nil
	}
	switch {
	case typ.Kind() == reflect.Struct:
		return checkMapping(node, typ)
	case typ.Kind() == reflect.Slice:
		return checkSequence(node, typ.Elem())
	case typ.Kind() == reflect.String:
		return checkScalar(node, "!!str")
	case typ.Kind() == reflect.Bool:
		return checkScalar(node, "!!bool")
	default:
		return wrongType(node)
	}
}

func checkMapping(node *yamlv3.Node, typ reflect.Type) error {
	if node.Kind != yamlv3.MappingNode || node.ShortTag() != "!!map" {
		return wrongType(node)
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		field, ok := fieldByKey(typ, node.Content[i].Value)
		if !ok {
			continue
		}
		if err := checkKinds(node.Content[i+1], field.Type); err != nil {
			return err
		}
	}
	return nil
}

// checkSequence refuses a null item, which the decoder drops without an error.
func checkSequence(node *yamlv3.Node, elem reflect.Type) error {
	if node.Kind != yamlv3.SequenceNode || node.ShortTag() != "!!seq" {
		return wrongType(node)
	}
	for _, item := range node.Content {
		if isNull(item) {
			return fmt.Errorf("%w: a list item is missing at line %d", ErrValues, item.Line)
		}
		if err := checkKinds(item, elem); err != nil {
			return err
		}
	}
	return nil
}

func checkScalar(node *yamlv3.Node, tag string) error {
	if node.Kind != yamlv3.ScalarNode || node.ShortTag() != tag {
		return wrongType(node)
	}
	return nil
}

// fieldByKey finds the field whose yaml tag names key.
func fieldByKey(typ reflect.Type, key string) (reflect.StructField, bool) {
	for field := range typ.Fields() {
		if name, _, _ := strings.Cut(field.Tag.Get("yaml"), ","); name == key {
			return field, true
		}
	}
	return reflect.StructField{}, false
}

func isNull(node *yamlv3.Node) bool {
	return node.Kind == yamlv3.ScalarNode && node.ShortTag() == "!!null"
}

// wrongType names only the line, because the value can hold a credential.
func wrongType(node *yamlv3.Node) error {
	return fmt.Errorf("%w: a value has the wrong type at line %d", ErrValues, node.Line)
}

// checkKey refuses a mapping key that is not a plain string. The decoder
// applies a merge key, skips a null key, and matches a field by a
// binary-tagged key's decoded value.
func checkKey(key *yamlv3.Node) error {
	switch {
	case key.ShortTag() == "!!merge":
		return fmt.Errorf("%w: line %d: merge keys are not allowed", ErrValues, key.Line)
	case isNull(key):
		return fmt.Errorf("%w: line %d: null keys are not allowed", ErrValues, key.Line)
	case key.Kind != yamlv3.ScalarNode || key.ShortTag() != "!!str":
		return fmt.Errorf("%w: line %d: a key must be a plain string", ErrValues, key.Line)
	}
	return nil
}

// parseError describes a parse error in fixed words, with the line it names
// and a key only when the key is a plain name. NEVER repeat the error's text,
// because it can quote the file, which may hold a credential in a URL.
func parseError(err error) error {
	if typeErr, ok := errors.AsType[*yamlv3.TypeError](err); ok && len(typeErr.Errors) > 0 {
		return fmt.Errorf("%w: %s", ErrValues, typeProblem(typeErr.Errors[0]))
	}
	return fmt.Errorf("%w: not valid YAML or JSON%s", ErrValues, atLine(yamlLineRE, err.Error()))
}

// typeProblem names the problem a yaml/v3 TypeError entry reports, in fixed
// words with its line, or by the key alone when the key is a plain name.
func typeProblem(msg string) string {
	where := atLine(typeLineRE, msg)
	if match := unknownRE.FindStringSubmatch(msg); match != nil {
		if plainKeyRE.MatchString(match[1]) {
			return fmt.Sprintf("unknown field %q", match[1])
		}
		return "an unknown key" + where
	}
	switch {
	case repeatedRE.MatchString(msg):
		return "a key is repeated" + where
	case wrongTypeRE.MatchString(msg):
		return "a value has the wrong type" + where
	default:
		return "not valid YAML or JSON" + where
	}
}

// atLine returns " at line N" when lineRE captures N at the start of msg.
func atLine(lineRE *regexp.Regexp, msg string) string {
	if match := lineRE.FindStringSubmatch(msg); match != nil {
		return " at line " + match[1]
	}
	return ""
}

func (v *Values) checkConnection() error {
	if err := checkURL("management_api_url", v.ManagementAPIURL); err != nil {
		return err
	}
	if v.ZoneURL == "" && v.ZoneID == "" {
		return fmt.Errorf("%w: one of zone_url or zone_id is required", ErrValues)
	}
	if v.ZoneURL != "" {
		if err := checkIssuer("zone_url", v.ZoneURL); err != nil {
			return err
		}
	}
	return nil
}

func (v *Values) checkManagementServiceAccount() error {
	if v.ManagementServiceAccount.ClientID == "" {
		return fmt.Errorf("%w: management_service_account.client_id is required", ErrValues)
	}
	if v.ManagementServiceAccount.ClientSecretEnv == "" && v.ManagementServiceAccount.ClientSecretFile == "" {
		v.ManagementServiceAccount.ClientSecretEnv = DefaultManagementSecretEnv
	}
	if v.ManagementServiceAccount.ClientSecretEnv != "" &&
		!envNameRE.MatchString(v.ManagementServiceAccount.ClientSecretEnv) {
		return fmt.Errorf("%w: management_service_account.client_secret_env is not an environment variable name", ErrValues)
	}
	return checkSecretFile("management_service_account.client_secret_file", v.ManagementServiceAccount.ClientSecretFile)
}

// checkSecretFile refuses a relative path. The commands that read a secret
// file run from different directories, so each must find the same file.
func checkSecretFile(key, path string) error {
	if path != "" && !filepath.IsAbs(path) {
		return fmt.Errorf("%w: %s must be an absolute path (got %q)", ErrValues, key, path)
	}
	return nil
}

func (v *Values) checkSandbox() error {
	if err := checkIssuer("issuer_url", v.IssuerURL); err != nil {
		return err
	}
	if v.Namespace == "" {
		v.Namespace = DefaultNamespace
	}
	if !namespaceRE.MatchString(v.Namespace) || len(v.Namespace) > maxNamespaceLength {
		return fmt.Errorf("%w: namespace is not a DNS-1123 label", ErrValues)
	}
	if v.PolicySet == "" {
		v.PolicySet = DefaultPolicySetName
	}
	return nil
}

func (v *Values) checkUsers() error {
	seen := map[string]bool{}
	for i, user := range v.Users {
		if parsed, err := mail.ParseAddress(user.Email); err != nil || parsed.Address != user.Email {
			return fmt.Errorf("%w: users[%d].email is not a bare email address", ErrValues, i)
		}
		if seen[user.Email] {
			return fmt.Errorf("%w: users[%d].email is listed twice", ErrValues, i)
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
			return fmt.Errorf("%w: resources[%d].identifier is listed twice", ErrValues, i)
		}
		seen[resource.Identifier] = true
		if resource.Name == "" {
			resource.Name = resource.Identifier
		}
		if err := checkSource(i, resource); err != nil {
			return err
		}
	}
	return nil
}

// checkSource checks where resources[i] gets its credential: exactly one of
// a provider, an environment variable and an absolute file path.
func checkSource(i int, resource *ResourceValues) error {
	set := 0
	for _, source := range []string{resource.CredentialProvider, resource.SecretEnv, resource.SecretFile} {
		if source != "" {
			set++
		}
	}
	if set != 1 {
		return fmt.Errorf("%w: resources[%d]: set exactly one of credential_provider, secret_env and secret_file",
			ErrValues, i)
	}
	if resource.SecretEnv != "" && !envNameRE.MatchString(resource.SecretEnv) {
		return fmt.Errorf("%w: resources[%d].secret_env is not an environment variable name", ErrValues, i)
	}
	return checkSecretFile(fmt.Sprintf("resources[%d].secret_file", i), resource.SecretFile)
}

// checkIssuer applies checkURL and refuses a query or a fragment, which an
// OAuth issuer must not have (RFC 8414 section 2).
func checkIssuer(key, value string) error {
	if err := checkURL(key, value); err != nil {
		return err
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.RawQuery != "" || parsed.ForceQuery || strings.Contains(value, "#") {
		return fmt.Errorf("%w: %s must not have a query or a fragment", ErrValues, key)
	}
	return nil
}

// checkURL requires an absolute https URL with a host, no userinfo and no
// port. NEVER put the value in an error, because a URL can carry a
// credential.
func checkURL(key, value string) error {
	if value == "" {
		return fmt.Errorf("%w: %s is required", ErrValues, key)
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" {
		return fmt.Errorf("%w: %s is not an absolute URL with a host", ErrValues, key)
	}
	if parsed.User != nil {
		return fmt.Errorf("%w: %s must not carry userinfo", ErrValues, key)
	}
	if parsed.Scheme != "https" {
		return fmt.Errorf("%w: %s must use https", ErrValues, key)
	}
	if parsed.Port() != "" || strings.HasSuffix(parsed.Host, ":") {
		return fmt.Errorf("%w: %s must not set a port", ErrValues, key)
	}
	return nil
}

// LoadSecrets reads every credential listed in the values file, and refuses
// when any is missing or empty. It removes the whitespace around a secret
// read from a file.
func LoadSecrets(
	values *Values,
	lookupEnv func(string) (string, bool),
	readFile func(string) ([]byte, error),
) (*Secrets, error) {
	management, err := managementSecret(values.ManagementServiceAccount, lookupEnv, readFile)
	if err != nil {
		return nil, err
	}
	resources := map[string]string{}
	var missing []string
	for i, resource := range values.Resources {
		switch {
		case resource.SecretEnv != "":
			value, ok := lookupEnv(resource.SecretEnv)
			if !ok || value == "" {
				missing = append(missing, fmt.Sprintf("%s (for resources[%d])", resource.SecretEnv, i))
				continue
			}
			resources[resource.Identifier] = value
		case resource.SecretFile != "":
			raw, err := readFile(resource.SecretFile)
			value := strings.TrimSpace(string(raw))
			if err != nil || value == "" {
				missing = append(missing, fmt.Sprintf("resources[%d].secret_file", i))
				continue
			}
			resources[resource.Identifier] = value
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: unset: %s", ErrMissingSecret, strings.Join(missing, ", "))
	}
	return &Secrets{ManagementClientSecret: management, Resource: resources}, nil
}

func managementSecret(
	account ManagementServiceAccountValues,
	lookupEnv func(string) (string, bool),
	readFile func(string) ([]byte, error),
) (string, error) {
	if account.ClientSecretFile != "" {
		raw, err := readFile(account.ClientSecretFile)
		if err != nil {
			return "", fmt.Errorf("%w: management_service_account.client_secret_file %s", ErrMissingSecret, readFailure(err))
		}
		secret := strings.TrimSpace(string(raw))
		if secret == "" {
			return "", fmt.Errorf("%w: management_service_account.client_secret_file is empty", ErrMissingSecret)
		}
		return secret, nil
	}
	secret, ok := lookupEnv(account.ClientSecretEnv)
	if !ok || secret == "" {
		return "", fmt.Errorf(
			"%w: management_service_account.client_secret_env %s is unset",
			ErrMissingSecret,
			account.ClientSecretEnv,
		)
	}
	return secret, nil
}

// readFailure describes a failed read in fixed words, because the error names
// the path, which comes from the values file.
func readFailure(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "does not exist"
	case errors.Is(err, fs.ErrPermission):
		return "cannot be read: permission denied"
	default:
		return "cannot be read"
	}
}
