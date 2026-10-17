package state

import (
	"slices"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
)

// The containers of the sandbox's pod, and the state of one that finished.
const (
	AgentContainer = "agent"
	ProxyContainer = "egress-proxy"
	// NetsetupContainer is the init container that sets the firewall rules.
	NetsetupContainer = "egress-netsetup"
	StateTerminated   = "terminated"
)

// ContainerRow is one container of a pod.
type ContainerRow struct {
	Pod       string
	Container sandbox.ContainerStatus
}

// PodState is how an agent's pod is, from the last read of the pods.
type PodState int

// The states of an agent's pod.
const (
	// PodNone is an agent without a pod: it was not brought up.
	PodNone PodState = iota
	// PodComingUp is a pod that an action makes or restarts now.
	PodComingUp
	// PodUnread is a pod the last read could not list.
	PodUnread
	// PodMissing is a pod the last read did not find.
	PodMissing
	// PodStarting is a pod whose agent does not run yet.
	PodStarting
	// PodRunning is a pod whose agent runs.
	PodRunning
	// PodFirewallFailed is a pod whose firewall failed, so no agent runs.
	PodFirewallFailed
	// PodAgentFailed is a pod whose agent stopped or is stuck.
	PodAgentFailed
)

// PodState says how agent's pod is, with the container that says so. Every
// screen reads it here, so that they agree.
func (s *Sandbox) PodState(agent sandbox.Agent) (PodState, ContainerRow) {
	if row, failed := s.firewallFailed(agent.Pod); failed {
		return PodFirewallFailed, row
	}
	row, found := s.agentRow(agent.Pod)
	if !found {
		return s.noPod(agent), ContainerRow{}
	}
	switch {
	case row.Container.Ready:
		return PodRunning, row
	case Crashed(row.Container):
		return PodAgentFailed, row
	}
	return PodStarting, row
}

// Crashed reports whether a container stopped or is stuck.
func Crashed(ctr sandbox.ContainerStatus) bool { return ctr.State == StateTerminated || stuck(ctr) }

// stuck reports whether a container does not come up by itself.
func stuck(ctr sandbox.ContainerStatus) bool {
	return slices.Contains(
		[]string{"CrashLoopBackOff", "ErrImagePull", "ImagePullBackOff", "CreateContainerConfigError"}, ctr.Reason,
	)
}

// firewallFailed returns pod's firewall container when it did not set the
// rules. A failed proxy is TroubleProxy, since the agent runs without it.
func (s *Sandbox) firewallFailed(pod string) (ContainerRow, bool) {
	for _, row := range s.Agents.Pods {
		ctr := row.Container
		if row.Pod == pod && ctr.Name == NetsetupContainer &&
			(ctr.State == StateTerminated && ctr.ExitCode != 0 || stuck(ctr)) {
			return row, true
		}
	}
	return ContainerRow{}, false
}

// noPod says why the last read found no agent container for agent.
func (s *Sandbox) noPod(agent sandbox.Agent) PodState {
	switch {
	case agent.Email != "" && s.Run.Job.Agent == agent.Email:
		return PodComingUp
	case agent.Pod == "":
		return PodNone
	case s.Agents.PodsErr != nil:
		return PodUnread
	}
	return PodMissing
}

// agentRow finds the agent's container of pod in the last read.
func (s *Sandbox) agentRow(pod string) (ContainerRow, bool) {
	index := slices.IndexFunc(s.Agents.Pods, func(row ContainerRow) bool {
		return pod != "" && row.Pod == pod && row.Container.Name == AgentContainer
	})
	if index < 0 {
		return ContainerRow{}, false
	}
	return s.Agents.Pods[index], true
}
