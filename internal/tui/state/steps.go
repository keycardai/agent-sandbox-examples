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

// Stopped reports whether the bring-up ended at the step because the
// operator stopped it, not because the step failed.
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

// StartSteps sets the steps from index from on pending, with empty logs,
// for a bring-up that runs them.
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

// StepEnded marks step i done, or failed with err. The checks step counts
// the checks that passed.
func (s *Sandbox) StepEnded(i int, err error) *StepRow {
	row := s.Run.Steps[i]
	row.Ended, row.Err, row.State = s.Now(), err, StepDone
	if err != nil {
		row.State = StepFailed
	}
	if row.Step.ID == sandbox.StepChecks {
		row.Passed, row.Total = countChecks(row.Log)
	}
	return row
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
// or from the beginning. While the tunnel does not run, it starts at the
// tunnel, because every later step needs its URL. A quick tunnel's new URL
// then makes the cluster step offer to recreate the cluster.
func (s *Sandbox) ResumeIndex() int {
	if len(s.Run.Steps) > 0 && s.Run.Steps[0].Step.ID == sandbox.StepTunnel && !s.Reads.Status.Tunnel.Running {
		return 0
	}
	return max(slices.IndexFunc(s.Run.Steps, func(row *StepRow) bool { return row.State == StepFailed }), 0)
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
