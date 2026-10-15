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
// screen, and ? shows them all.
type KeyMap struct {
	// Moving around.
	Up, Down key.Binding
	// Actions on the sandbox.
	Start, Checks, Agent, Shell, Restart, Events, Setup, Demo, StopAll key.Binding
	// The log pane.
	Follow, Wrap, Top, Bottom, Search key.Binding
	// The TUI.
	Help, Quit, Back key.Binding
}

// NewKeyMap returns the TUI's bindings.
func NewKeyMap() KeyMap {
	bind := func(keys []string, help, desc string) key.Binding {
		return key.NewBinding(key.WithKeys(keys...), key.WithHelp(help, desc))
	}
	return KeyMap{
		Up:      bind([]string{"up", "k"}, "↑/k", "up"),
		Down:    bind([]string{"down", "j"}, "↓/j", "down"),
		Start:   bind([]string{"u"}, "u", "bring up"),
		Checks:  bind([]string{"c"}, "c", "run checks"),
		Agent:   bind([]string{"a"}, "a", "open the agent"),
		Shell:   bind([]string{"x"}, "x", "shell in agent"),
		Restart: bind([]string{"R"}, "R", "restart agent pod"),
		Events:  bind([]string{"o"}, "o", "zone events"),
		Setup:   bind([]string{"e"}, "e", "settings"),
		Demo:    bind([]string{"p"}, "p", "switch demo"),
		StopAll: bind([]string{"D"}, "D", "stop everything"),
		Follow:  bind([]string{"f"}, "f", "follow"),
		Wrap:    bind([]string{"w"}, "w", "wrap"),
		Top:     bind([]string{"g", "home"}, "g", "top"),
		Bottom:  bind([]string{"G", "end"}, "G", "bottom"),
		Search:  bind([]string{"/"}, "/", "search"),
		Help:    bind([]string{"?"}, "?", "help"),
		Quit:    bind([]string{"q", KeyCtrlC}, "q", "quit"),
		Back:    bind([]string{KeyEsc}, "esc", "back"),
	}
}

// All returns the bindings ? shows, in groups.
func (k KeyMap) All() [][]key.Binding {
	return [][]key.Binding{
		{k.Up, k.Down},
		{k.Start, k.Agent, k.Shell, k.Checks, k.Events, k.Restart, k.Demo, k.Setup, k.StopAll},
		{k.Help, k.Back, k.Quit},
	}
}
