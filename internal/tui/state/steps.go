package state

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/component"
)

// StepState is where one bring-up step is.
type StepState int

// The states of a bring-up step.
const (
	StepPending StepState = iota
	StepRunning
	StepDone
	StepFailed
)

// StepRow is one bring-up step, as the bring-up last reported it.
type StepRow struct {
	Step           sandbox.Step
	State          StepState
	Started, Ended time.Time
	Err            error
	Log            *component.Log
	// Passed and Total count the checks, for the checks step.
	Passed, Total int
}

// Stopped reports whether the operator stopped the bring-up at the step.
func (r *StepRow) Stopped() bool {
	return r.State == StepFailed && errors.Is(r.Err, context.Canceled)
}

// StepSource names a step's log.
func StepSource(id sandbox.StepID) string { return "step " + string(id) }

// ResetSteps lists the steps for the current settings, all pending.
func (s *Sandbox) ResetSteps() {
	s.Run.Steps = nil
	for _, step := range s.session.Steps() {
		s.Run.Steps = append(s.Run.Steps, &StepRow{Step: step, Log: s.logs(StepSource(step.ID))})
	}
	s.Run.Cursor = min(s.Run.Cursor, max(len(s.Run.Steps)-1, 0))
}

// StartSteps sets the steps from index from on pending, with empty logs.
func (r *Run) StartSteps(from int) {
	for _, row := range r.Steps[from:] {
		row.State, row.Err = StepPending, nil
		row.Log.Reset()
	}
	r.Cursor = from
}

// StepStarted marks step i running, and the bring-up on it.
func (s *Sandbox) StepStarted(i int) {
	row := s.Run.Steps[i]
	row.State, row.Started, row.Ended = StepRunning, s.Now(), time.Time{}
	s.Run.Cursor = i
}

// StepEnded marks step i done, or failed with err. Once the sandbox step
// is done, its demo is the one the sandbox runs.
func (s *Sandbox) StepEnded(i int, err error) *StepRow {
	row := s.Run.Steps[i]
	row.Ended, row.Err, row.State = s.Now(), err, StepDone
	if err != nil {
		row.State = StepFailed
	}
	switch {
	case row.Step.ID == sandbox.StepChecks:
		row.Passed, row.Total = countChecks(row.Log)
	case row.Step.ID == sandbox.StepSandbox && err == nil:
		s.Demos.Launched = s.deployedDemo()
	}
	return row
}

// StepOf returns the step id, or nil when the bring-up does not have it.
func (r *Run) StepOf(id sandbox.StepID) *StepRow {
	index := slices.IndexFunc(r.Steps, func(row *StepRow) bool { return row.Step.ID == id })
	if index < 0 {
		return nil
	}
	return r.Steps[index]
}

// FailedStep returns the step a bring-up stopped at, or nil. A failed check
// does not count: the sandbox is up.
func (r *Run) FailedStep() *StepRow {
	for _, row := range r.Steps {
		if row.State == StepFailed && row.Step.ID != sandbox.StepChecks {
			return row
		}
	}
	return nil
}

// ResumeIndex is where a bring-up starts again: at the step that failed,
// or from the beginning. When the tunnel or the mirror that a step before
// the failed one started no longer runs, it starts at that step instead,
// because the later steps need it. A quick tunnel's new URL then makes the
// cluster step offer to recreate the cluster.
func (s *Sandbox) ResumeIndex() int {
	failed := slices.IndexFunc(s.Run.Steps, func(row *StepRow) bool { return row.State == StepFailed })
	if failed < 0 {
		return 0
	}
	for i, row := range s.Run.Steps[:failed] {
		if row.Step.ID == sandbox.StepTunnel && !s.Reads.Status.Tunnel.Running ||
			row.Step.ID == sandbox.StepMirror && !s.Reads.Status.Mirror.Running {
			return i
		}
	}
	return failed
}

// StepsDone counts the steps that are done.
func (r *Run) StepsDone() int {
	done := 0
	for _, row := range r.Steps {
		if row.State == StepDone {
			done++
		}
	}
	return done
}

// countChecks counts the PASS and FAIL lines the checks print.
func countChecks(log *component.Log) (int, int) {
	lines, _ := log.Snapshot()
	passed, total := 0, 0
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "  PASS"):
			passed++
			total++
		case strings.HasPrefix(line, "  FAIL"):
			total++
		}
	}
	return passed, total
}
