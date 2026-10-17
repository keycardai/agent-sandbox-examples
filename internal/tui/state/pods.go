package state

import (
	"slices"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
)

// The containers of the sandbox's pod, and the state of one that finished.
const (
	AgentContainer = "agent"
	ProxyContainer = "egress-proxy"
	// NetsetupContainer is the pod's init container, which sets the
	// firewall rules.
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
	// PodUnread is a pod the last read could not list.
	PodUnread
	// PodMissing is a pod the last read did not find.
	PodMissing
	// PodStarting is a pod whose agent does not run yet.
	PodStarting
	// PodRunning is a pod whose agent runs.
	PodRunning
	// PodFirewallFailed is a pod whose firewall did not start, so the agent
	// never does.
	PodFirewallFailed
	// PodAgentFailed is a pod whose agent stopped or is stuck.
	PodAgentFailed
)

// PodState says how agent's pod is, with the container that says so: the
// firewall's when it failed, else the agent's.
func (s *Sandbox) PodState(agent sandbox.Agent) (PodState, ContainerRow) {
	if agent.Pod == "" {
		return PodNone, ContainerRow{}
	}
	if row, failed := s.firewallFailed(agent.Pod); failed {
		return PodFirewallFailed, row
	}
	index := slices.IndexFunc(s.Agents.Pods, func(row ContainerRow) bool {
		return row.Pod == agent.Pod && row.Container.Name == AgentContainer
	})
	switch {
	case index < 0 && s.Agents.PodsErr != nil:
		return PodUnread, ContainerRow{}
	case index < 0:
		return PodMissing, ContainerRow{}
	}
	row := s.Agents.Pods[index]
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

// firewallFailed returns the firewall's container of pod when it did not
// set the rules. The egress proxy is an init container too, as a native
// sidecar, but its failure is a trouble of its own: the agent can run
// without it.
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
