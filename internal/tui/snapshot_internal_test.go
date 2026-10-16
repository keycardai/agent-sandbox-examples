package tui

import (
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/component"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/page"
)

// update rewrites the snapshots' golden files from the screens drawn now.
//
//nolint:gochecknoglobals // A test flag is a package variable.
var update = flag.Bool("update", false, "rewrite the snapshots' golden files in testdata")

// clockTime matches a time of day, which differs on every run.
var clockTime = regexp.MustCompile(`\b\d{2}:\d{2}:\d{2}\b`)

// TestSnapshots draws each screen, with sample state, at a large size and
// the smallest, and compares it with its golden file in testdata: the text
// without its colors, the spaces at the end of each line, and the times of
// day. -update writes the golden files from what the screens draw now.
// TUI_SNAPSHOTS names a directory to write each screen to with its colors,
// as ANSI text, for a person to look at.
func TestSnapshots(t *testing.T) {
	t.Parallel()
	colors := os.Getenv("TUI_SNAPSHOTS")
	for name, build := range snapshots() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := build(t)
			for _, size := range []struct {
				suffix        string
				width, height int
			}{{"", 150, 42}, {"-small", component.MinWidth, component.MinHeight}} {
				root.Update(tea.WindowSizeMsg{Width: size.width, Height: size.height})
				content := root.View().Content
				checkGolden(t, filepath.Join("testdata", name+size.suffix+".golden"), plainScreen(content))
				if colors != "" {
					//nolint:gosec // G703: the directory is the one the person running the test named.
					if err := os.WriteFile(filepath.Join(colors, name+size.suffix+".ans"), []byte(content), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
		})
	}
}

// plainScreen is a screen as its golden file keeps it: no colors, no spaces
// at the end of a line, and each time of day as hh:mm:ss.
func plainScreen(content string) string {
	lines := strings.Split(ansi.Strip(content), "\n")
	for i, line := range lines {
		lines[i] = clockTime.ReplaceAllString(strings.TrimRight(line, " "), "hh:mm:ss")
	}
	return strings.Join(lines, "\n") + "\n"
}

// checkGolden compares got with the golden file at path, or writes it with
// -update.
func checkGolden(t *testing.T, path, got string) {
	t.Helper()
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // G304: the path is a golden file under testdata.
	if err != nil {
		t.Fatalf("%v. Run go test ./internal/tui -run TestSnapshots -update to write it.", err)
	}
	if got == string(want) {
		return
	}
	gotLines, wantLines := strings.Split(got, "\n"), strings.Split(string(want), "\n")
	for i := range max(len(gotLines), len(wantLines)) {
		var gotLine, wantLine string
		if i < len(gotLines) {
			gotLine = gotLines[i]
		}
		if i < len(wantLines) {
			wantLine = wantLines[i]
		}
		if gotLine != wantLine {
			t.Errorf("%s differs at line %d:\n got %q\nwant %q\nThe screen now:\n%s", path, i+1, gotLine, wantLine, got)
			return
		}
	}
}

// snapshots builds each screen the snapshots draw, by name.
func snapshots() map[string]func(t *testing.T) *model {
	return map[string]func(t *testing.T) *model{
		"bringup": snapshotBringUp,
		"home":    snapshotUp,
	}
}

// writeSettings gives the model settings, as setup keeps them for the
// session.
func writeSettings(t *testing.T, root *model) {
	t.Helper()
	root.shared.Session.Local().Store = sandbox.NewStore(sandbox.NoConfig, &sandbox.Config{}, nil)
}

// snapshotUp is a sandbox that is up, at the snapshots' size, with what
// happened on the way.
func snapshotUp(t *testing.T) *model {
	t.Helper()
	root := upModel(t)
	root.Update(tea.WindowSizeMsg{Width: 150, Height: 42})
	root.shared.Frame = 3
	now := time.Now()
	for i, row := range root.shared.Steps {
		row.State, row.Started, row.Ended = page.StepDone, now.Add(-time.Duration(300-i*25)*time.Second),
			now.Add(-time.Duration(290-i*25)*time.Second)
	}
	for _, event := range [][2]string{
		{"▸", "Bring-up started"},
		{"✓", "Start the tunnel (3s)"},
		{"✓", "Install the charts (41s)"},
		{"✓", "Checks: 6 of 6 passed"},
		{"✓", "The sandbox is up"},
	} {
		root.shared.Record(event[0], event[1])
	}
	return root
}

func snapshotBringUp(t *testing.T) *model {
	t.Helper()
	root := snapshotUp(t)
	root.shared.Job = page.Job{Kind: page.JobBringUp}
	now := time.Now()
	for i, row := range root.shared.Steps {
		switch {
		case i == 6:
			row.State, row.Started, row.Ended = page.StepRunning, now.Add(-41*time.Second), time.Time{}
			row.Log.Add(`Release "keycard-sandbox" has been upgraded. Happy Helming!`, "NAME: keycard-sandbox",
				"STATUS: deployed", "Waiting for deployment agent-sandbox-controller to be ready")
		case i > 6:
			row.State = page.StepPending
		}
	}
	root.shared.StepCursor = 6
	return root
}
