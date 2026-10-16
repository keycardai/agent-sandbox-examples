package state

import (
	"slices"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
)

// Preflight is a run of the machine checks: what it checks, and the results
// so far. Run numbers the runs, so that a late result of an earlier run is
// dropped.
type Preflight struct {
	Items   []sandbox.PreflightItem
	Results []sandbox.Check
	Run     int
}

// Checking reports whether a check of the run has no result yet.
func (p Preflight) Checking() bool { return len(p.Results) < len(p.Items) }

// Failed counts the checks that failed.
func (p Preflight) Failed() int {
	failed := 0
	for _, check := range p.Results {
		if check.State == sandbox.CheckFailed {
			failed++
		}
	}
	return failed
}

// PreflightItems are the machine checks the bring-up needs.
func (s *Sandbox) PreflightItems() []sandbox.PreflightItem { return s.session.PreflightItems() }

// StartPreflight starts a run of the checks in items.
func (s *Sandbox) StartPreflight(items []sandbox.PreflightItem) {
	s.Preflight = Preflight{Items: items, Run: s.Preflight.Run + 1}
}

// CheckDone keeps the result of check index of run, and reports whether it
// kept it. A result of an earlier run, or out of order, is dropped.
func (s *Sandbox) CheckDone(run, index int, check sandbox.Check) bool {
	if run != s.Preflight.Run || index != len(s.Preflight.Results) {
		return false
	}
	s.Preflight.Results = append(s.Preflight.Results, check)
	return true
}

// FixSections lists the sections setup asks to fix the settings' problems,
// or none when a problem is in no one section, so that setup asks every
// section.
func (s *Sandbox) FixSections() []string {
	var sections []string
	for _, problem := range s.Problems {
		section := sandbox.ProblemSection(problem)
		if section == "" {
			return nil
		}
		if !slices.Contains(sections, section) {
			sections = append(sections, section)
		}
	}
	return sections
}
