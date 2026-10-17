package state

import "context"

// JobKind says what work a Job is.
type JobKind int

// The kinds of work that change the sandbox.
const (
	// JobNone is no work.
	JobNone JobKind = iota
	// JobBringUp runs the bring-up's steps from Job.From on.
	JobBringUp
	// JobChecks runs the checks alone, against the sandbox as it is.
	JobChecks
	// JobAction is one call on the session, such as a pod restart.
	JobAction
	// JobStop stops the tunnel and the mirror, and deletes the cluster.
	JobStop
)

// Job is the background work that changes the sandbox. At most one runs,
// and the zero Job means none does. The root model starts and ends it
// through Sandbox, and the pages read it.
type Job struct {
	Kind JobKind
	// Name says what runs, in the footer and the quit dialog, such as
	// "Restarting ada's agent's pod". A bring-up has none.
	Name string
	// From is the first step a bring-up runs.
	From int
	// StopAfter stops everything once the job ends. QuitAfter quits once the
	// job, and the stop after it, end without an error.
	StopAfter, QuitAfter bool
	// cancel stops the job. Only a job that runs steps has one.
	cancel context.CancelFunc
}

// Busy reports whether a job runs.
func (j Job) Busy() bool { return j.Kind != JobNone }

// Is reports whether the job is of kind.
func (j Job) Is(kind JobKind) bool { return j.Kind == kind }

// RunsSteps reports whether the job runs steps: a bring-up, or the checks.
// u and D stop it at the step that runs.
func (j Job) RunsSteps() bool { return j.Kind == JobBringUp || j.Kind == JobChecks }

// WaitText says what runs, for work that waits for it, or returns "".
func (j Job) WaitText() string {
	switch j.Kind {
	case JobBringUp:
		return "A bring-up is running. Wait for it to finish, or press u to stop it."
	case JobChecks:
		return "The checks are running. Wait for them to finish, or press u to stop them."
	case JobAction, JobStop:
		return j.Name + " is running. Wait for it to finish."
	case JobNone:
	}
	return ""
}

// StartJob makes job the one that runs, with cancel to stop it, or nil.
// While another job runs, it refuses.
func (r *Run) StartJob(job Job, cancel context.CancelFunc) bool {
	if r.Job.Busy() {
		return false
	}
	job.cancel = cancel
	r.Job = job
	return true
}

// StopAfterJob makes the job that runs stop everything once it ends, and
// with quit, quit after that.
func (r *Run) StopAfterJob(quit bool) { r.Job.StopAfter, r.Job.QuitAfter = true, quit }

// EndJob ends the job that runs, and returns it. A read that started
// before is no longer current.
func (s *Sandbox) EndJob() Job {
	ended := s.Run.Job
	s.Run.Job = Job{}
	s.Reads.mark++
	return ended
}

// CancelJob stops the job that runs, when it can be stopped.
func (r *Run) CancelJob() {
	if r.Job.cancel != nil {
		r.Job.cancel()
	}
}
