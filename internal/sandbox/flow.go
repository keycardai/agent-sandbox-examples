package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Defaults for a localdev sandbox.
const (
	DefaultAPIURL    = "https://api.localdev.keycard.sh"
	DefaultCluster   = "keycard-sandbox-local"
	DefaultIssuer    = "https://sandbox-oidc.localdev.keycard.sh"
	DefaultNamespace = "agents"
	// sandboxPolicySet is the policy set kubernetes-sandbox-user writes.
	sandboxPolicySet = "Kubernetes Sandbox"
)

// App runs the sandbox flows against one state file.
type App struct {
	KC        *Keycard
	State     *State
	StatePath string
	UI        *UI
}

func (a *App) save() error { return a.State.Save(a.StatePath) }

func (a *App) sandboxKube() Kube { return Kube{Context: ClusterContext(a.State.Cluster)} }

// SetupInput is what setup needs. Exactly one of ZoneID or CreateZone is set.
type SetupInput struct {
	ZoneID         string
	CreateZone     string
	APIKeys        []APIKey
	Cluster        string
	Issuer         string
	Namespace      string
	EgressProxyDir string
}

// APIKey is the key an API server package is installed with.
type APIKey struct {
	Slug string
	Name string
	// Label is the package's name for the key, such as Personal API Key.
	Label string
	Key   string
}

// Setup prepares the zone and the sandbox cluster. Every step checks first,
// so running it again changes only what is missing.
func (a *App) Setup(ctx context.Context, in SetupInput) error {
	s := a.State
	s.Cluster, s.Issuer, s.Namespace = in.Cluster, in.Issuer, in.Namespace
	a.UI.Section("Setting up")

	if err := a.UI.Step(
		ctx,
		"Zone",
		func(ctx context.Context) (string, error) { return a.useZone(ctx, in) },
	); err != nil {
		return err
	}
	for _, key := range in.APIKeys {
		title := key.Name + " installed"
		if err := a.UI.Step(ctx, title, func(ctx context.Context) (string, error) {
			out, err := a.KC.Install(ctx, s.ZoneID, key.Slug, map[string]any{"api_key": key.Key})
			if err != nil {
				return "", err
			}
			apiURL, _ := out["api_url"].(string)
			if apiURL == "" {
				return "", fmt.Errorf("the %s install returned no api_url", key.Slug)
			}
			s.SetResource(Resource{Slug: key.Slug, Name: key.Name, APIURL: apiURL})
			return apiURL, a.save()
		}); err != nil {
			return err
		}
	}
	if len(s.Resources) == 0 {
		return errors.New("no API server is installed; pick at least one")
	}
	if err := a.UI.Step(ctx, "Current policy set recorded for `sandbox down`", func(ctx context.Context) (string, error) {
		return a.recordPolicySet(ctx)
	}); err != nil {
		return err
	}
	if err := a.UI.Step(ctx, "Egress images", func(ctx context.Context) (string, error) {
		missing := MissingImages(ctx)
		if len(missing) == 0 {
			return "already built", nil
		}
		return "built " + strings.Join(missing, ", "), BuildImages(ctx, in.EgressProxyDir)
	}); err != nil {
		return err
	}
	if err := a.UI.Step(
		ctx,
		"Sandbox cluster "+s.Cluster,
		func(ctx context.Context) (string, error) { return a.ensureCluster(ctx) },
	); err != nil {
		return err
	}
	if err := a.UI.Step(ctx, "Images in the cluster", func(ctx context.Context) (string, error) {
		if ImportedImages(ctx, s.Cluster) {
			return "already imported", nil
		}
		return "imported", ImportImages(ctx, s.Cluster)
	}); err != nil {
		return err
	}
	localdev := Kube{Context: LocaldevContext}
	if err := a.UI.Step(ctx, "Issuer published", func(ctx context.Context) (string, error) {
		return s.Issuer, PublishIssuer(ctx, a.sandboxKube(), localdev, s.Issuer)
	}); err != nil {
		return err
	}
	if err := a.UI.Step(ctx, "Issuer verified the way the zone checks it", func(ctx context.Context) (string, error) {
		return "", VerifyIssuer(ctx, a.sandboxKube(), s.Issuer)
	}); err != nil {
		return err
	}
	if err := a.UI.Step(ctx, "Sandbox application installed", func(ctx context.Context) (string, error) {
		return a.installApplication(ctx)
	}); err != nil {
		return err
	}
	return a.UI.Step(ctx, "Namespace "+s.Namespace+" ready, zone CA loaded", func(ctx context.Context) (string, error) {
		ca, err := LocaldevCA()
		if err != nil {
			return "", err
		}
		return "", a.sandboxKube().Apply(ctx, NamespaceObjects(s.Namespace, ca))
	})
}

func (a *App) useZone(ctx context.Context, in SetupInput) (string, error) {
	var zone Zone
	var err error
	if in.CreateZone != "" {
		zone, err = a.KC.CreateZone(ctx, in.CreateZone)
	} else {
		zone, err = a.KC.GetZone(ctx, in.ZoneID)
	}
	if err != nil {
		return "", err
	}
	if zone.ID != a.State.ZoneID {
		// A different zone starts a fresh record of what it holds.
		a.State.Resources, a.State.Users, a.State.PreviousSet = nil, map[string]*User{}, nil
		a.State.ApplicationID, a.State.ProviderID = "", ""
	}
	a.State.ZoneID, a.State.ZoneName, a.State.ZoneURL = zone.ID, zone.Name, zone.URL()
	if a.State.ZoneURL == "" {
		return "", fmt.Errorf("zone %s has no URL", zone.ID)
	}
	return zone.Name + " (" + zone.ID + ")", a.save()
}

// recordPolicySet remembers the zone's active policy set before enrollment
// replaces it, unless it is already the sandbox set.
func (a *App) recordPolicySet(ctx context.Context) (string, error) {
	if a.State.PreviousSet != nil {
		return "already recorded", nil
	}
	binding, err := a.KC.ActiveBinding(ctx, a.State.ZoneID)
	if err != nil {
		return "", err
	}
	if binding == nil {
		return "the zone has none", nil
	}
	var set struct {
		Name string `json:"name"`
	}
	if _, err := a.KC.do(
		ctx,
		http.MethodGet,
		"/zones/"+a.State.ZoneID+"/policy-sets/"+binding.PolicySetID,
		nil,
		&set,
	); err != nil {
		return "", err
	}
	if set.Name == sandboxPolicySet {
		// An earlier run already replaced the zone's policies. The platform's
		// default set is what a zone starts with, so it is the one to restore.
		fallback, err := a.defaultPolicySet(ctx)
		if err != nil || fallback == nil {
			return "the sandbox set is already active", err
		}
		a.State.PreviousSet = fallback
		return "default-zone-policies (the sandbox set was already active)", a.save()
	}
	a.State.PreviousSet = binding
	return set.Name, a.save()
}

// defaultPolicySet finds the latest version of the platform's
// default-zone-policies set.
func (a *App) defaultPolicySet(ctx context.Context) (*Binding, error) {
	var page struct {
		Items []struct {
			ID              string `json:"id"`
			Name            string `json:"name"`
			LatestVersionID string `json:"latest_version_id"`
		} `json:"items"`
	}
	if _, err := a.KC.do(
		ctx,
		http.MethodGet,
		"/zones/"+a.State.ZoneID+"/policy-sets?query%5Bname%5D=default-zone-policies",
		nil,
		&page,
	); err != nil {
		return nil, err
	}
	for _, s := range page.Items {
		if s.Name == "default-zone-policies" && s.LatestVersionID != "" {
			return &Binding{PolicySetID: s.ID, PolicySetVersionID: s.LatestVersionID}, nil
		}
	}
	return nil, nil
}

func (a *App) ensureCluster(ctx context.Context) (string, error) {
	s := a.State
	exists, err := ClusterExists(ctx, s.Cluster)
	if err != nil {
		return "", err
	}
	if !exists {
		if err := CreateCluster(ctx, s.Cluster, s.Issuer); err != nil {
			return "", err
		}
	}
	// The API server cannot change its issuer while it runs.
	have, err := ClusterIssuer(ctx, a.sandboxKube())
	if err != nil {
		return "", err
	}
	if have != s.Issuer {
		return "", fmt.Errorf(
			"cluster %s mints issuer %s, not %s; run `sandbox down` to recreate it",
			s.Cluster,
			have,
			s.Issuer,
		)
	}
	if exists {
		return "already running", nil
	}
	return "created", nil
}

func (a *App) installApplication(ctx context.Context) (string, error) {
	s := a.State
	resources := make([]any, 0, len(s.Resources))
	for _, url := range s.ResourceURLs() {
		resources = append(resources, url)
	}
	out, err := a.KC.Install(ctx, s.ZoneID, "kubernetes-sandbox", map[string]any{
		"issuer": s.Issuer, "namespace": s.Namespace, "resources": resources,
	})
	if err != nil {
		return "", err
	}
	s.ApplicationID, _ = out["application_id"].(string)
	s.ProviderID, _ = out["provider_id"].(string)
	detail := "urn:keycard:sandbox-proxy:" + s.Namespace
	if skipped := stringList(out["skipped_resources"]); len(skipped) > 0 {
		detail += " · skipped " + strings.Join(skipped, ", ")
	}
	return detail, a.save()
}

func stringList(v any) []string {
	items, _ := v.([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// RequireSetup fails when setup has not run.
func (a *App) RequireSetup() error {
	if a.State.ZoneID == "" || a.State.ApplicationID == "" {
		return errors.New("run `sandbox setup` first")
	}
	return nil
}

// FindUser finds a zone user by email, ignoring case.
func (a *App) FindUser(ctx context.Context, email string) (*ZoneUser, error) {
	users, err := a.KC.ListUsers(ctx, a.State.ZoneID)
	if err != nil {
		return nil, err
	}
	for i := range users {
		if strings.EqualFold(users[i].Email, email) {
			return &users[i], nil
		}
	}
	return nil, fmt.Errorf(
		"%w: no user with email %s in zone %s; they have to sign in to %s once",
		ErrNotFound,
		email,
		a.State.ZoneID,
		a.State.ZoneURL,
	)
}

// AddUser enrolls a user through kubernetes-sandbox-user, which gives their
// sandbox a credential and writes the policies that let it act for them, then
// starts the sandbox.
func (a *App) AddUser(ctx context.Context, email string) (*User, error) {
	if err := a.RequireSetup(); err != nil {
		return nil, err
	}
	s := a.State
	a.UI.Section("Adding " + email + " with access")
	user, err := a.lookUp(ctx, email)
	if err != nil {
		return nil, err
	}
	if err := a.UI.Step(ctx, "Enrolled, policies written", func(ctx context.Context) (string, error) {
		out, err := a.KC.Install(ctx, s.ZoneID, "kubernetes-sandbox-user", map[string]any{
			"email": email, "issuer": s.Issuer, "namespace": s.Namespace,
		})
		if err != nil {
			return "", err
		}
		user.Subject, _ = out["subject"].(string)
		user.CredentialID, _ = out["credential_id"].(string)
		if id, _ := out["user_identifier"].(string); id != "" {
			user.Identifier = id
		}
		if user.CredentialID == "" || user.Subject == "" {
			return "", errors.New("the enrollment returned no credential")
		}
		user.Access = true
		s.Users[strings.ToLower(email)] = user
		return user.Subject, a.save()
	}); err != nil {
		return nil, err
	}
	return user, a.startUserSandbox(ctx, user)
}

// AddUserWithoutAccess starts a sandbox for a zone user who has no policies.
// The sandbox gets a credential, so it authenticates as the shared
// application, but nothing lets the application act for this user, so the
// zone refuses every request it makes.
func (a *App) AddUserWithoutAccess(ctx context.Context, email string) (*User, error) {
	if err := a.RequireSetup(); err != nil {
		return nil, err
	}
	s := a.State
	a.UI.Section("Adding " + email + " without access")
	user, err := a.lookUp(ctx, email)
	if err != nil {
		return nil, err
	}
	if err := a.UI.Step(ctx, "Has no sandbox policies", func(ctx context.Context) (string, error) {
		for _, kind := range []string{"impersonation", "access"} {
			has, err := a.KC.PolicyExists(ctx, s.ZoneID, "Sandbox "+kind+": "+user.Email)
			if err != nil {
				return "", err
			}
			if has {
				return "", fmt.Errorf(
					"%s already has access from an earlier enrollment (policy \"Sandbox %s: %s\")",
					user.Email,
					kind,
					user.Email,
				)
			}
		}
		return "", nil
	}); err != nil {
		return nil, err
	}
	if err := a.UI.Step(ctx, "Sandbox credential, no policies", func(ctx context.Context) (string, error) {
		user.Subject = "system:serviceaccount:" + s.Namespace + ":" + user.ID
		creds, err := a.KC.TokenCredentials(ctx, s.ZoneID, s.ApplicationID)
		if err != nil {
			return "", err
		}
		for _, c := range creds {
			if c.ProviderID == s.ProviderID && c.Subject == user.Subject {
				user.CredentialID = c.ID
			}
		}
		if user.CredentialID == "" {
			cred, err := a.KC.CreateTokenCredential(ctx, s.ZoneID, s.ApplicationID, s.ProviderID, user.Subject)
			if err != nil {
				return "", err
			}
			user.CredentialID = cred.ID
		}
		user.Access = false
		s.Users[strings.ToLower(email)] = user
		return user.Subject, a.save()
	}); err != nil {
		return nil, err
	}
	return user, a.startUserSandbox(ctx, user)
}

// lookUp finds the user in the zone. The CLI never creates users.
func (a *App) lookUp(ctx context.Context, email string) (*User, error) {
	var user *User
	err := a.UI.Step(ctx, "Found in the zone", func(ctx context.Context) (string, error) {
		zu, err := a.FindUser(ctx, email)
		if err != nil {
			return "", err
		}
		user = &User{Email: zu.Email, ID: zu.ID, Identifier: zu.Identifier, Pod: PodName(zu.ID)}
		return zu.ID, nil
	})
	return user, err
}

func (a *App) startUserSandbox(ctx context.Context, user *User) error {
	return a.UI.Step(ctx, "Sandbox pod running", func(ctx context.Context) (string, error) {
		return user.Pod, a.startSandbox(ctx, a.sandboxFor(user, user.Pod, user.ID, user.Identifier))
	})
}

func (a *App) sandboxFor(user *User, name, serviceAccount, impersonate string) Sandbox {
	s := a.State
	return Sandbox{
		Name: name, Namespace: s.Namespace, ServiceAccount: serviceAccount,
		ZoneURL: s.ZoneURL, ClientID: user.CredentialID, ImpersonateUser: impersonate,
		Resources: s.Resources, UserID: user.ID,
	}
}

// startSandbox applies a sandbox and waits for it. A pod's spec cannot change,
// so an existing pod is replaced.
func (a *App) startSandbox(ctx context.Context, sb Sandbox) error {
	gateway, err := HostGateway(ctx, a.State.Cluster)
	if err != nil {
		return err
	}
	sb.HostGateway = gateway
	kube := a.sandboxKube()
	if kube.Exists(ctx, sb.Namespace, "pod/"+sb.Name) {
		if _, err := kube.kubectl(
			ctx,
			nil,
			"-n",
			sb.Namespace,
			"delete",
			"pod",
			sb.Name,
			"--wait=true",
			"--timeout=60s",
		); err != nil {
			return err
		}
	}
	if err := kube.Apply(ctx, sb.Objects()); err != nil {
		return err
	}
	return kube.WaitReady(ctx, sb.Namespace, sb.Name, 3*time.Minute)
}

// RunInput is what run needs.
type RunInput struct {
	// Read is the GitHub file the agent reads, as OWNER/REPO/PATH. Only a
	// setup with GitHub needs it.
	Read string
	// Emails limits the run to these users. Empty runs every user.
	Emails []string
}

// Result is what one call by one user's agent got.
type Result struct {
	User     *User
	Resource string
	API      string
	Fetch    Fetch
	// Answer is what the API said, shown the way the call presents it.
	Answer string
	// Reason and Detail are why the proxy refused the request, from its
	// audit record. Both are empty when the proxy let it through.
	Reason, Detail string
	// Matches says the result is what the user's access allows: an answer
	// with access, the zone's refusal without.
	Matches bool
}

// Run has the agent in each user's sandbox call every installed API it has a
// call for, and prints what each call got: the answer for a user with
// access, the zone's refusal for a user without. It fails when any call got
// something other than that.
func (a *App) Run(ctx context.Context, in RunInput) ([]Result, error) {
	users := a.State.SortedUsers()
	if len(in.Emails) > 0 {
		users = users[:0]
		for _, email := range in.Emails {
			u := a.State.Users[strings.ToLower(email)]
			if u == nil {
				return nil, fmt.Errorf("%s has no sandbox yet; run `sandbox add-user --email %s`", email, email)
			}
			users = append(users, u)
		}
	}
	if len(users) == 0 {
		return nil, errors.New("no users yet; run `sandbox add-user`")
	}
	probes, err := probesFor(a.State.Resources, in.Read)
	if err != nil {
		return nil, err
	}
	if len(probes) == 0 {
		return nil, errors.New("the agent has no call for the installed API servers; set up GitHub or Linear")
	}
	var results []Result
	failed := false
	for _, u := range users {
		a.UI.Section("Agent acting for " + u.Email + " (" + accessText(u.Access) + ")")
		for _, p := range probes {
			r, err := a.runProbe(ctx, u, p)
			if err != nil {
				failed = true
			}
			results = append(results, r)
		}
	}
	if failed {
		return results, errors.New("a user's agent did not get what their access allows")
	}
	return results, nil
}

// runProbe makes one call from the user's sandbox, checks it against the
// user's access, and prints the answer or the refusal under the step.
func (a *App) runProbe(ctx context.Context, u *User, p probe) (Result, error) {
	r := Result{User: u, Resource: p.Resource, API: p.API}
	err := a.UI.Step(ctx, p.Title, func(ctx context.Context) (string, error) {
		f, err := a.fetch(ctx, u.Pod, p.Args)
		if err != nil {
			return "", err
		}
		r.Fetch = f
		var answerErr error
		if f.Status == http.StatusOK {
			r.Answer, answerErr = p.Show(f.Body)
		} else {
			r.Reason, r.Detail = a.refusal(ctx, u.Pod)
		}
		// The zone refusing the exchange is the only refusal that shows the
		// user has no access. Anything else is a broken sandbox.
		refused := strings.HasPrefix(r.Reason, "exchange_")
		answered := f.Status == http.StatusOK && answerErr == nil
		r.Matches = (u.Access && answered) || (!u.Access && refused)
		switch {
		case r.Matches && u.Access:
			return "HTTP 200", nil
		case r.Matches:
			return "refused · HTTP " + strconv.Itoa(f.Status) + " " + r.Reason, nil
		case !u.Access && f.Status == http.StatusOK:
			return "", errors.New("expected the zone to refuse the exchange, but the call went through")
		case answerErr != nil:
			return "", answerErr
		case r.Reason != "":
			return "", fmt.Errorf("expected %s, got HTTP %d %s", want(u.Access), f.Status, r.Reason)
		default:
			return "", fmt.Errorf("expected %s, got HTTP %d from the API", want(u.Access), f.Status)
		}
	})
	switch {
	case r.Answer != "":
		a.UI.Quote(r.Answer)
	case r.Detail != "":
		a.UI.Quote(r.Detail)
	case r.Fetch.Status != http.StatusOK && r.Reason == "" && r.Fetch.Body != "":
		a.UI.Quote(r.Fetch.Body)
	}
	return r, err
}

func want(access bool) string {
	if access {
		return "an answer"
	}
	return "the zone to refuse the exchange"
}

func accessText(access bool) string {
	if access {
		return "with access"
	}
	return "no access"
}

// refusal reads why the proxy refused the last request from its audit
// record: the reason, such as exchange_denied, and the zone's error when the
// record has one.
func (a *App) refusal(ctx context.Context, pod string) (reason, detail string) {
	logs, err := a.sandboxKube().kubectl(ctx, nil, "-n", a.State.Namespace, "logs", pod, "-c", "proxy", "--since=2m")
	if err != nil {
		return "", ""
	}
	lines := strings.Split(strings.TrimSpace(logs), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		var rec struct {
			Decision struct {
				Verdict string `json:"verdict"`
				Reason  string `json:"reason"`
				Error   string `json:"error"`
			} `json:"decision"`
		}
		if json.Unmarshal([]byte(lines[i]), &rec) != nil || rec.Decision.Verdict != "deny" {
			continue
		}
		detail = rec.Decision.Error
		if j := strings.Index(detail, "oauth error "); j >= 0 {
			detail = detail[j+len("oauth error "):]
		}
		return rec.Decision.Reason, detail
	}
	return "", ""
}

// Down removes the sandbox cluster and the issuer route, and reactivates the
// policy set that was active before setup.
func (a *App) Down(ctx context.Context) error {
	s := a.State
	a.UI.Section("Tearing down")
	if s.PreviousSet != nil {
		if err := a.UI.Step(ctx, "Zone's previous policy set active again", func(ctx context.Context) (string, error) {
			return s.PreviousSet.PolicySetID, a.KC.Activate(ctx, s.ZoneID, *s.PreviousSet)
		}); err != nil {
			return err
		}
	}
	if err := a.UI.Step(ctx, "Issuer route removed from localdev", func(ctx context.Context) (string, error) {
		return "", UnpublishIssuer(ctx, Kube{Context: LocaldevContext})
	}); err != nil {
		return err
	}
	if err := a.UI.Step(ctx, "Sandbox cluster "+s.Cluster+" deleted", func(ctx context.Context) (string, error) {
		exists, err := ClusterExists(ctx, s.Cluster)
		if err != nil || !exists {
			return "already gone", err
		}
		return "", DeleteCluster(ctx, s.Cluster)
	}); err != nil {
		return err
	}
	s.PreviousSet = nil
	for _, u := range s.Users {
		u.Pod = PodName(u.ID)
	}
	a.UI.Info("Keycard entities stay in the zone; setup reuses them.")
	return a.save()
}

// Status prints the saved state and checks each piece.
func (a *App) Status(ctx context.Context) {
	s := a.State
	if s.ZoneID == "" {
		a.UI.Warn("Nothing set up yet. Run `sandbox` or `sandbox setup`.")
		return
	}
	a.UI.Section("Zone " + s.ZoneName + " (" + s.ZoneID + ")")
	a.UI.Info("URL " + s.ZoneURL)
	for _, r := range s.Resources {
		a.UI.Info(r.Name + " · " + r.APIURL)
	}
	a.UI.Info("Application urn:keycard:sandbox-proxy:" + s.Namespace + " (" + s.ApplicationID + ")")
	a.UI.Section("Cluster " + s.Cluster)
	_ = a.UI.Step(ctx, "Cluster running", func(ctx context.Context) (string, error) {
		exists, err := ClusterExists(ctx, s.Cluster)
		if err == nil && !exists {
			err = errors.New("not found; run `sandbox setup`")
		}
		return "", err
	})
	_ = a.UI.Step(ctx, "Issuer published", func(ctx context.Context) (string, error) {
		return s.Issuer, VerifyIssuer(ctx, a.sandboxKube(), s.Issuer)
	})
	for _, u := range s.SortedUsers() {
		_ = a.UI.Step(ctx, u.Email+" sandbox ready ("+accessText(u.Access)+")", func(ctx context.Context) (string, error) {
			return u.Pod, a.sandboxKube().WaitReady(ctx, s.Namespace, u.Pod, 5*time.Second)
		})
	}
}
