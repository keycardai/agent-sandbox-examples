package sandbox

import (
	"context"
	"errors"
	"time"
)

// Defaults for a localdev sandbox.
const (
	DefaultAPIURL    = "https://api.localdev.keycard.sh"
	DefaultCluster   = "keycard-sandbox-local"
	DefaultIssuer    = "https://sandbox-oidc.localdev.keycard.sh"
	DefaultNamespace = "agents"
)

var (
	errNotSetUp       = errors.New("run `sandbox setup` first")
	errClusterMissing = errors.New("the sandbox cluster does not exist; run `sandbox setup`")
)

// App runs the sandbox flows against one state file.
type App struct {
	KC        *Keycard
	State     *State
	StatePath string
	UI        *UI
}

// step is one reported step of a flow.
type step struct {
	title string
	run   func(context.Context) (string, error)
}

// RequireSetup fails when setup has not run.
func (a *App) RequireSetup() error {
	if a.State.ZoneID == "" || a.State.ApplicationID == "" {
		return errNotSetUp
	}
	return nil
}

// Down removes the sandbox cluster and the issuer route, and reactivates the
// policy set that was active before setup.
func (a *App) Down(ctx context.Context) error {
	state := a.State
	a.UI.Section("Tearing down")
	var steps []step
	if state.PreviousSet != nil {
		steps = append(steps, step{"Zone's previous policy set active again", func(ctx context.Context) (string, error) {
			return state.PreviousSet.PolicySetID, a.KC.Activate(ctx, state.ZoneID, *state.PreviousSet)
		}})
	}
	steps = append(steps,
		step{"Issuer route removed from localdev", func(ctx context.Context) (string, error) {
			return "", UnpublishIssuer(ctx, Kube{Context: LocaldevContext})
		}},
		step{"Sandbox cluster " + state.Cluster + " deleted", func(ctx context.Context) (string, error) {
			exists, err := ClusterExists(ctx, state.Cluster)
			if err != nil || !exists {
				return "already gone", err
			}
			return "", DeleteCluster(ctx, state.Cluster)
		}},
	)
	if err := a.runSteps(ctx, steps...); err != nil {
		return err
	}
	state.PreviousSet = nil
	a.UI.Info("Keycard entities stay in the zone; setup reuses them.")
	return a.save()
}

// Status prints the saved state and checks each piece. A failed check is
// reported and the next one still runs.
func (a *App) Status(ctx context.Context) {
	const readyWait = 5 * time.Second
	state := a.State
	if state.ZoneID == "" {
		a.UI.Warn("Nothing set up yet. Run `sandbox` or `sandbox setup`.")
		return
	}
	a.UI.Section("Zone " + state.ZoneName + " (" + state.ZoneID + ")")
	a.UI.Info("URL " + state.ZoneURL)
	for _, r := range state.Resources {
		a.UI.Info(r.Name + " · " + r.APIURL)
	}
	a.UI.Info("Application " + applicationIdentifier(state.Namespace) + " (" + state.ApplicationID + ")")
	a.UI.Section("Cluster " + state.Cluster)
	_ = a.UI.Step(ctx, "Cluster running", func(ctx context.Context) (string, error) {
		exists, err := ClusterExists(ctx, state.Cluster)
		if err == nil && !exists {
			err = errClusterMissing
		}
		return "", err
	})
	_ = a.UI.Step(ctx, "Issuer published", func(ctx context.Context) (string, error) {
		return state.Issuer, VerifyIssuer(ctx, a.sandboxKube(), state.Issuer)
	})
	for _, user := range state.SortedUsers() {
		title := user.Email + " sandbox ready (" + accessText(user.Access) + ")"
		_ = a.UI.Step(ctx, title, func(ctx context.Context) (string, error) {
			return user.Pod, a.sandboxKube().WaitReady(ctx, state.Namespace, user.Pod, readyWait)
		})
	}
}

// runSteps runs the steps in order and stops at the first that fails.
func (a *App) runSteps(ctx context.Context, steps ...step) error {
	for _, s := range steps {
		if err := a.UI.Step(ctx, s.title, s.run); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) save() error { return a.State.Save(a.StatePath) }

func (a *App) sandboxKube() Kube { return Kube{Context: ClusterContext(a.State.Cluster)} }

// applicationIdentifier is the identifier kubernetes-sandbox gives the shared
// application of a namespace.
func applicationIdentifier(namespace string) string { return "urn:keycard:sandbox-proxy:" + namespace }

func accessText(access bool) string {
	if access {
		return "with access"
	}
	return "no access"
}

// sleep waits for d and reports true, or reports false once ctx is done.
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
