package sandbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

var (
	errUserNotFound = errors.New("no user with this email in the zone")
	errNoCredential = errors.New("the enrollment returned no credential")
	errHasAccess    = errors.New("the user already has sandbox policies from an earlier enrollment")
)

// FindUser finds a zone user by email, ignoring case.
func (a *App) FindUser(ctx context.Context, email string) (ZoneUser, error) {
	users, err := a.KC.ListUsers(ctx, a.State.ZoneID)
	if err != nil {
		return ZoneUser{}, err
	}
	i := slices.IndexFunc(users, func(u ZoneUser) bool { return strings.EqualFold(u.Email, email) })
	if i < 0 {
		return ZoneUser{}, fmt.Errorf("%w: %s; they have to sign in to %s once", errUserNotFound, email, a.State.ZoneURL)
	}
	return users[i], nil
}

// AddUser enrolls a user through kubernetes-sandbox-user, which gives their
// sandbox a credential and writes the policies that let it act for them, then
// starts the sandbox.
func (a *App) AddUser(ctx context.Context, email string) (*User, error) {
	if err := a.RequireSetup(); err != nil {
		return nil, err
	}
	a.UI.Section("Adding " + email + " with access")
	user, err := a.lookUp(ctx, email)
	if err != nil {
		return nil, err
	}
	if err := a.UI.Step(ctx, "Enrolled, policies written", func(ctx context.Context) (string, error) {
		return a.enroll(ctx, user)
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
	a.UI.Section("Adding " + email + " without access")
	user, err := a.lookUp(ctx, email)
	if err != nil {
		return nil, err
	}
	if err := a.runSteps(ctx,
		step{"Has no sandbox policies", func(ctx context.Context) (string, error) {
			return "", a.requireNoPolicies(ctx, user)
		}},
		step{"Sandbox credential, no policies", func(ctx context.Context) (string, error) {
			return a.ensureCredential(ctx, user)
		}},
	); err != nil {
		return nil, err
	}
	return user, a.startUserSandbox(ctx, user)
}

// lookUp finds the user in the zone. The CLI never creates users.
func (a *App) lookUp(ctx context.Context, email string) (*User, error) {
	var user *User
	err := a.UI.Step(ctx, "Found in the zone", func(ctx context.Context) (string, error) {
		found, err := a.FindUser(ctx, email)
		if err != nil {
			return "", err
		}
		user = &User{Email: found.Email, ID: found.ID, Identifier: found.Identifier, Pod: PodName(found.ID)}
		return found.ID, nil
	})
	return user, err
}

// enroll installs kubernetes-sandbox-user for the user and records what it
// made.
func (a *App) enroll(ctx context.Context, user *User) (string, error) {
	state := a.State
	out, err := a.KC.Install(ctx, state.ZoneID, "kubernetes-sandbox-user", map[string]any{
		"email": user.Email, "issuer": state.Issuer, "namespace": state.Namespace,
	})
	if err != nil {
		return "", err
	}
	user.Subject, _ = out["subject"].(string)
	user.CredentialID, _ = out["credential_id"].(string)
	if identifier, _ := out["user_identifier"].(string); identifier != "" {
		user.Identifier = identifier
	}
	if user.CredentialID == "" || user.Subject == "" {
		return "", errNoCredential
	}
	user.Access = true
	state.Users[strings.ToLower(user.Email)] = user
	return user.Subject, a.save()
}

// requireNoPolicies refuses a user who already has the policies an enrollment
// writes, because a sandbox for them would get access.
func (a *App) requireNoPolicies(ctx context.Context, user *User) error {
	label := policyLabel(a.State.Namespace)
	for _, kind := range []string{"impersonation", "access"} {
		name := label + " " + kind + ": " + user.Email
		has, err := a.KC.PolicyExists(ctx, a.State.ZoneID, name)
		if err != nil {
			return err
		}
		if has {
			return fmt.Errorf("%w: %q", errHasAccess, name)
		}
	}
	return nil
}

// ensureCredential finds or creates the token credential the user's sandbox
// authenticates with.
func (a *App) ensureCredential(ctx context.Context, user *User) (string, error) {
	state := a.State
	user.Subject = sandboxSubject(state.Namespace, user.ID)
	creds, err := a.KC.TokenCredentials(ctx, state.ZoneID, state.ApplicationID)
	if err != nil {
		return "", err
	}
	i := slices.IndexFunc(creds, func(c TokenCredential) bool {
		return c.ProviderID == state.ProviderID && c.Subject == user.Subject
	})
	if i >= 0 {
		user.CredentialID = creds[i].ID
	} else {
		cred, err := a.KC.CreateTokenCredential(ctx, state.ZoneID, state.ApplicationID, state.ProviderID, user.Subject)
		if err != nil {
			return "", err
		}
		user.CredentialID = cred.ID
	}
	user.Access = false
	state.Users[strings.ToLower(user.Email)] = user
	return user.Subject, a.save()
}

func (a *App) startUserSandbox(ctx context.Context, user *User) error {
	state := a.State
	sandbox := Sandbox{
		Name:            user.Pod,
		Namespace:       state.Namespace,
		ServiceAccount:  user.ID,
		ZoneURL:         state.ZoneURL,
		ClientID:        user.CredentialID,
		ImpersonateUser: user.Identifier,
		Resources:       state.Resources,
		UserID:          user.ID,
	}
	return a.UI.Step(ctx, "Sandbox pod running", func(ctx context.Context) (string, error) {
		return user.Pod, a.startSandbox(ctx, sandbox)
	})
}

// startSandbox applies a sandbox and waits for it. A pod's spec cannot change,
// so an existing pod is replaced.
func (a *App) startSandbox(ctx context.Context, sandbox Sandbox) error {
	const startTimeout = 3 * time.Minute
	gateway, err := HostGateway(ctx, a.State.Cluster)
	if err != nil {
		return err
	}
	sandbox.HostGateway = gateway
	objects, err := sandbox.Objects()
	if err != nil {
		return err
	}
	kube := a.sandboxKube()
	if err := kube.DeletePod(ctx, sandbox.Namespace, sandbox.Name); err != nil {
		return err
	}
	if err := kube.Apply(ctx, objects...); err != nil {
		return err
	}
	return kube.WaitReady(ctx, sandbox.Namespace, sandbox.Name, startTimeout)
}

// policyLabel is the label kubernetes-sandbox-user starts a namespace's
// policy names with: Sandbox in the default namespace, and the namespace added
// in any other.
func policyLabel(namespace string) string {
	if namespace == DefaultNamespace {
		return "Sandbox"
	}
	return "Sandbox (" + namespace + ")"
}

// sandboxSubject is the subject of the tokens a user's sandbox pod presents:
// its ServiceAccount, named after the user's id.
func sandboxSubject(namespace, userID string) string {
	return "system:serviceaccount:" + namespace + ":" + userID
}
