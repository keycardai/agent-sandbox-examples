package state

// Phase is how the sandbox is, as every screen says it.
type Phase int

// The phases of the sandbox.
const (
	// PhaseReading is before the first read of the sandbox came back.
	PhaseReading Phase = iota
	// PhaseBringingUp is a bring-up that runs.
	PhaseBringingUp
	// PhaseChecking is the checks running alone.
	PhaseChecking
	// PhaseStopping is D stopping everything.
	PhaseStopping
	// PhaseStopped is a bring-up the operator stopped at a step.
	PhaseStopped
	// PhaseFailed is a bring-up that failed at a step.
	PhaseFailed
	// PhaseUp is a sandbox that runs.
	PhaseUp
	// PhaseDown is a sandbox that does not run.
	PhaseDown
)

// Phase says how the sandbox is, and for PhaseStopped and PhaseFailed,
// the step the bring-up stopped at.
func (s *Sandbox) Phase() (Phase, *StepRow) {
	switch {
	case s.Run.Job.Is(JobBringUp):
		return PhaseBringingUp, nil
	case s.Run.Job.Is(JobChecks):
		return PhaseChecking, nil
	case s.Run.Job.Is(JobStop):
		return PhaseStopping, nil
	}
	if row := s.Run.FailedStep(); row != nil {
		if row.Stopped() {
			return PhaseStopped, row
		}
		return PhaseFailed, row
	}
	switch {
	case !s.Reads.Done:
		return PhaseReading, nil
	case s.Up():
		return PhaseUp, nil
	}
	return PhaseDown, nil
}
