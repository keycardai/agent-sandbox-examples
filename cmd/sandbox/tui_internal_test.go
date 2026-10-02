package main

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"github.com/keycardai/agent-sandbox-poc/internal/sandbox"
)

func update(t *testing.T, live *model, msg tea.Msg) tea.Cmd {
	t.Helper()
	_, cmd := live.Update(msg)
	return cmd
}

func TestModelKeepsOnlyRunningStepsLive(t *testing.T) {
	t.Parallel()
	live := newModel(newTheme(), nil, func() {})

	update(t, live, sandbox.Event{Kind: sandbox.EventStepStart, ID: 1, Text: clusterStep})
	if view := live.View().Content; !strings.Contains(view, clusterStep) {
		t.Fatalf("the live area does not show the running step:\n%s", view)
	}
	if cmd := update(t, live, sandbox.Event{Kind: sandbox.EventStepEnd, ID: 1, Text: clusterStep}); cmd == nil {
		t.Fatal("a finished step does not print")
	}
	if strings.Contains(live.View().Content, clusterStep) {
		t.Error("the finished step is still live")
	}
}

// TestModelPrintsInOrder checks that output prints one batch at a time, and
// that the program quits only once the command has finished and everything
// has printed.
func TestModelPrintsInOrder(t *testing.T) {
	t.Parallel()
	live := newModel(newTheme(), nil, func() {})

	update(t, live, sandbox.Event{Kind: sandbox.EventOK, Text: zoneStep})
	update(t, live, sandbox.Event{Kind: sandbox.EventOK, Text: clusterStep})
	update(t, live, finishMsg{})
	if len(live.queue) != 1 || live.done {
		t.Fatalf("queue = %q, done = %v, want the cluster step held until the zone step has printed", live.queue, live.done)
	}
	update(t, live, printedMsg{})
	if len(live.queue) != 0 || live.done {
		t.Fatalf("queue = %q, done = %v, want the cluster step printing", live.queue, live.done)
	}
	update(t, live, printedMsg{})
	if !live.done || live.View().Content != "" {
		t.Error("the program does not quit once everything has printed")
	}
}

func TestModelStopsOnCtrlCThenQuits(t *testing.T) {
	t.Parallel()
	cancels := 0
	live := newModel(newTheme(), nil, func() { cancels++ })
	ctrlC := tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}

	update(t, live, ctrlC)
	if cancels != 1 || live.done || !strings.Contains(live.View().Content, "Stopping") {
		t.Fatalf("cancels = %d, done = %v, want the command stopped and the program still up", cancels, live.done)
	}
	update(t, live, ctrlC)
	if !live.done {
		t.Error("a second ctrl+c does not quit")
	}
}

func TestModelRepliesWithTheFormsOutcome(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		msg  tea.Msg
		want error
	}{
		{name: "answered", msg: formDoneMsg{}},
		{name: "cancelled", msg: formCancelMsg{}, want: huh.ErrUserAborted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			live := newModel(newTheme(), nil, func() {})
			reply := make(chan error, 1)
			var name string
			form := huh.NewForm(huh.NewGroup(huh.NewInput().Title("Name the new zone").Value(&name)))

			update(t, live, askMsg{form: form, reply: reply})
			if !strings.Contains(live.View().Content, "Name the new zone") {
				t.Fatalf("the form is not showing:\n%s", live.View().Content)
			}
			update(t, live, tc.msg)
			if err := <-reply; !errors.Is(err, tc.want) {
				t.Errorf("reply = %v, want %v", err, tc.want)
			}
			if live.form != nil {
				t.Error("the form is still showing")
			}
		})
	}
}
