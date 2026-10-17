package state

import (
	"slices"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
)

// The containers of the sandbox's pod, and the state of one that finished.
const (
	AgentContainer  = "agent"
	ProxyContainer  = "egress-proxy"
	StateTerminated = "terminated"
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
	// PodAgentFailed is a pod whose agent stopped, or does not come up by
	// itself.
	PodAgentFailed
)

// PodState says how agent's pod is, with the agent's container.
func (s *Sandbox) PodState(agent sandbox.Agent) (PodState, ContainerRow) {
	if agent.Pod == "" {
		return PodNone, ContainerRow{}
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

// Crashed reports whether a container stopped, or does not come up by
// itself.
func Crashed(ctr sandbox.ContainerStatus) bool {
	return ctr.State == StateTerminated || slices.Contains(
		[]string{"CrashLoopBackOff", "ErrImagePull", "ImagePullBackOff", "CreateContainerConfigError"}, ctr.Reason,
	)
}
