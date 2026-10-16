package component

import (
	"charm.land/bubbles/v2/key"
)

// Keys the TUI reads by name, outside the key map.
const (
	KeyEnter = "enter"
	KeyEsc   = "esc"
	KeyCtrlC = "ctrl+c"
	KeyTab   = "tab"
)

// Hint is one key in the footer and what it does.
type Hint struct{ Key, Desc string }

// KeyMap holds every binding. The footer shows the ones that apply to the
// focused pane, and ? shows them all.
type KeyMap struct {
	// Moving around.
	Up, Down, NextPane, PrevPane, Tab1, Tab2, Tab3, Output key.Binding
	// Actions on the sandbox.
	Start, Checks, Agent, Shell, Restart, Send, Events, DNS, Setup, Demo, Activity, Logs, StopAll key.Binding
	// The agents.
	AddAgent, RemoveAgent key.Binding
	// The log pane.
	Follow, Wrap, Top, Bottom, PageUp, PageDown, Search key.Binding
	// The TUI.
	Help, Quit, Back, Mouse key.Binding
}

// NewKeyMap returns the TUI's bindings.
func NewKeyMap() KeyMap {
	bind := func(keys []string, help, desc string) key.Binding {
		return key.NewBinding(key.WithKeys(keys...), key.WithHelp(help, desc))
	}
	return KeyMap{
		Up:       bind([]string{"up", "k"}, "↑/k", "up"),
		Down:     bind([]string{"down", "j"}, "↓/j", "down"),
		NextPane: bind([]string{KeyTab}, KeyTab, "next pane"),
		PrevPane: bind([]string{"shift+tab"}, "shift+tab", "previous pane"),
		Tab1:     bind([]string{"1"}, "1", "home"),
		Tab2:     bind([]string{"2"}, "2", "agents"),
		Tab3:     bind([]string{"3"}, "3", "traffic"),
		Output:   bind([]string{KeyEnter}, "enter", "focus the log"),
		Start:    bind([]string{"u"}, "u", "bring up"),
		Checks:   bind([]string{"c"}, "c", "run checks"),
		Agent:    bind([]string{"a"}, "a", "open the agent"),
		Shell:    bind([]string{"x"}, "x", "shell in agent"),
		Restart:  bind([]string{"R"}, "R", "restart the pod"),
		AddAgent: bind([]string{"n"}, "n", "add a person"),
		// RemoveAgent is a capital, as a key that takes something away.
		RemoveAgent: bind([]string{"X"}, "X", "remove the agent"),
		Send:        bind([]string{"s"}, "s", "send a request"),
		Events:      bind([]string{"o"}, "o", "zone events"),
		DNS:         bind([]string{"d"}, "d", "show DNS"),
		Setup:       bind([]string{"e"}, "e", "settings"),
		Demo:        bind([]string{"p"}, "p", "switch demo"),
		Activity:    bind([]string{"t"}, "t", "activity"),
		Logs:        bind([]string{"l"}, "l", "every log"),
		StopAll:     bind([]string{"D"}, "D", "stop everything"),
		Follow:      bind([]string{"f"}, "f", "follow"),
		Wrap:        bind([]string{"w"}, "w", "wrap"),
		Top:         bind([]string{"g", "home"}, "g", "top"),
		Bottom:      bind([]string{"G", "end"}, "G", "bottom"),
		// PageUp and PageDown name the log pane's own keys, for the help.
		PageUp:   bind([]string{"pgup", "b"}, "pgup/b", "page up"),
		PageDown: bind([]string{"pgdown", "space"}, "pgdn/space", "page down"),
		Search:   bind([]string{"/"}, "/", "search"),
		Help:     bind([]string{"?"}, "?", "help"),
		Quit:     bind([]string{"q", KeyCtrlC}, "q", "quit"),
		Back:     bind([]string{KeyEsc}, "esc", "back"),
		Mouse:    bind([]string{"m"}, "m", "mouse on/off"),
	}
}

// HelpGroup is a titled group of bindings that ? lists.
type HelpGroup struct {
	Title    string
	Bindings []key.Binding
}

// Groups returns every binding, in the groups ? lists.
func (k KeyMap) Groups() []HelpGroup {
	return []HelpGroup{
		{"Tabs", []key.Binding{k.Tab1, k.Tab2, k.Tab3}},
		{"General", []key.Binding{k.Up, k.Down, k.NextPane, k.PrevPane, k.Output, k.Back, k.Help, k.Quit}},
		{"Sandbox", []key.Binding{k.Start, k.Checks, k.Demo, k.Setup, k.Events, k.StopAll}},
		{"Agent", []key.Binding{k.Agent, k.Send, k.Shell, k.Restart, k.AddAgent, k.RemoveAgent}},
		{"Logs", []key.Binding{
			k.Activity, k.Logs, k.DNS, k.Follow, k.Wrap, k.Search, k.Top, k.Bottom, k.PageUp, k.PageDown, k.Mouse,
		}},
	}
}
