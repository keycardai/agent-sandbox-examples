package state

import "github.com/keycardai/agent-sandbox-examples/internal/sandbox"

// Trouble is something that keeps a sandbox that runs from working.
type Trouble int

// The troubles of a sandbox that runs.
const (
	// TroubleMirror is the mirror down: Keycard cannot verify the agent.
	TroubleMirror Trouble = iota
	// TroubleProxy is the egress proxy down: requests go nowhere.
	TroubleProxy
	// TroubleProxyLog is the proxy's log that cannot be read.
	TroubleProxyLog
	// TroubleChecks is checks that failed.
	TroubleChecks
)

// Troubles lists what keeps the sandbox from working, in the order a
// request needs it.
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
