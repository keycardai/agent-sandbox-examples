package tui

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
)

const (
	// minCheck is the least time a machine check shows its spinner.
	minCheck = 350 * time.Millisecond
	// checkTimeout bounds one machine check.
	checkTimeout = 10 * time.Second
)

// checkDoneMsg is one machine check's result: check index of run.
type checkDoneMsg struct {
	run, index int
	check      sandbox.Check
}

// startPreflight runs the machine checks the bring-up needs.
func (m *model) startPreflight() tea.Cmd { return m.preflight(m.state.PreflightItems()) }

// preflight stops the last run and runs the checks in items one at a time.
func (m *model) preflight(items []sandbox.PreflightItem) tea.Cmd {
	m.stopPreflight()
	m.preflightCtx, m.preflightStop = context.WithCancel(m.ctx)
	m.state.Setup.StartPreflight(items)
	return m.nextCheck()
}

func (m *model) stopPreflight() {
	if m.preflightStop != nil {
		m.preflightStop()
	}
}

// onCheck keeps a check's result, records it, and starts the next check.
// A result of an earlier run is dropped.
func (m *model) onCheck(msg checkDoneMsg) tea.Cmd {
	if !m.state.Setup.CheckDone(msg.run, msg.index, msg.check) {
		return nil
	}
	mark := map[sandbox.CheckState]string{sandbox.CheckOK: "✓", sandbox.CheckNote: "•", sandbox.CheckFailed: "✕"}
	m.state.Record(mark[msg.check.State], msg.check.Name+": "+msg.check.Detail)
	return m.nextCheck()
}

// nextCheck runs the first check without a result, for at least minCheck.
func (m *model) nextCheck() tea.Cmd {
	run := m.state.Setup.Preflight
	index := len(run.Results)
	if index >= len(run.Items) {
		return nil
	}
	item, base := run.Items[index], m.preflightCtx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(base, checkTimeout)
		defer cancel()
		started := time.Now()
		check := item.Run(ctx)
		if wait := minCheck - time.Since(started); wait > 0 {
			time.Sleep(wait)
		}
		return checkDoneMsg{run: run.Run, index: index, check: check}
	}
}
