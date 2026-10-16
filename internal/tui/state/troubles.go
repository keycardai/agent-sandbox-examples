package state

import "github.com/keycardai/agent-sandbox-examples/internal/sandbox"

// Trouble is something that keeps a sandbox that runs from working as it
// should, though the agent's pod may run.
type Trouble int

// The troubles of a sandbox that runs.
const (
	// TroubleMirror is the mirror down, so that Keycard cannot verify the
	// agent with the cluster's keys.
	TroubleMirror Trouble = iota
	// TroubleProxy is the egress proxy's container that stopped, so that the
	// agent's requests go nowhere.
	TroubleProxy
	// TroubleProxyLog is the proxy's log that cannot be read, so that what
	// Keycard did is not known.
	TroubleProxyLog
	// TroubleChecks is checks that failed.
	TroubleChecks
)

// Troubles lists what keeps the sandbox from working as it should, in the
// order a request needs it: Keycard can verify the agent, the proxy runs
// and its log reads, and the checks pass.
func (s *Sandbox) Troubles() []Trouble {
	var troubles []Trouble
	if s.Run.StepOf(sandbox.StepMirror) != nil && !s.Reads.Status.Mirror.Running {
		troubles = append(troubles, TroubleMirror)
	}
	if proxy, ok := s.Container(ProxyContainer); ok && Crashed(proxy.Container) {
		troubles = append(troubles, TroubleProxy)
	}
	if s.Agents.Traffic.Err() != nil {
		troubles = append(troubles, TroubleProxyLog)
	}
	if checks := s.Run.StepOf(sandbox.StepChecks); checks != nil && checks.State == StepFailed && !checks.Stopped() {
		troubles = append(troubles, TroubleChecks)
	}
	return troubles
}
