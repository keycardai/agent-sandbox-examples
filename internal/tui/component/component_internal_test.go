package component

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// TestLog splits writes into lines, keeps a partial line, shows what a
// carriage return leaves, and keeps only the last MaxLines.
func TestLog(t *testing.T) {
	t.Parallel()
	log := &Log{}
	_, _ = log.Write([]byte("one\ntw"))
	_, _ = log.Write([]byte("o\r"))
	_, _ = log.Write([]byte("\nprogress 10%\rprogress 90%\nthree 10%\rthree 90%"))
	lines, _ := log.Snapshot()
	if want := "one|two|progress 90%|three 90%"; strings.Join(lines, "|") != want {
		t.Errorf("lines = %q, want %q", lines, want)
	}
	for i := range MaxLines + 10 {
		log.Add(time.Duration(i).String())
	}
	if log.Len() != MaxLines+1 {
		t.Errorf("Len = %d, want %d with the partial line", log.Len(), MaxLines+1)
	}
}

// TestLogPlain keeps a line's text and drops what would reach the terminal
// as a command: a title, a cursor move, a DCS string, a bell, a backspace.
func TestLogPlain(t *testing.T) {
	t.Parallel()
	log := &Log{}
	log.Add("a\x1b]0;title\x07b\x1b[2Jc\x1bP$qm\x1b\\d\x07\x08\te\x1b[31m")
	_, _ = log.Write([]byte("\x1b[1Apartial\x1b_Ga=T\x1b\\"))
	lines, _ := log.Snapshot()
	if want := "abcd    e|partial"; strings.Join(lines, "|") != want {
		t.Errorf("lines = %q, want %q", lines, want)
	}
}

// TestLogViewFromTop shows a followed log from its end, and a log written
// whole from its first line: when it shows, and again when it is written
// anew after a scroll.
func TestLogViewFromTop(t *testing.T) {
	t.Parallel()
	numbered := func(prefix string) []string {
		lines := make([]string, 10)
		for i := range lines {
			lines[i] = prefix + " " + strconv.Itoa(i)
		}
		return lines
	}
	view := NewLogView(func(_ string, lines []string) []string { return lines })
	view.FromTop("response")
	view.SetSize(20, 3)
	view.Log("agent").Add(numbered("agent")...)
	view.Log("response").Add(numbered("first")...)
	view.Show("agent")
	if got := view.View(); !strings.Contains(got, "agent 9") || strings.Contains(got, "agent 0") {
		t.Errorf("the followed log shows\n%s\nwant its end", got)
	}
	view.Show("response")
	if got := view.View(); !strings.Contains(got, "first 0") {
		t.Errorf("the response shows\n%s\nwant its first line", got)
	}
	view.Keys(tea.KeyPressMsg{Code: tea.KeyDown}, NewKeyMap())
	response := view.Log("response")
	response.Reset()
	response.Add(numbered("second")...)
	view.Refresh(false)
	if got := view.View(); !strings.Contains(got, "second 0") {
		t.Errorf("the new response shows\n%s\nwant its first line", got)
	}
}

// TestTailFile reads a log as it grows, and starts over when it is
// replaced.
func TestTailFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "tunnel.log")
	log := &Log{}
	go TailFile(t.Context(), path, log)
	write := func(text string) {
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	waitFor := func(want string) {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if lines, _ := log.Snapshot(); strings.Join(lines, "|") == want {
				return
			}
			time.Sleep(tailEvery)
		}
		lines, _ := log.Snapshot()
		t.Fatalf("lines = %q, want %q", lines, want)
	}
	write("first\n")
	waitFor("first")
	write("first\nsecond\n")
	waitFor("first|second")
	write("new\n")
	waitFor("new")
}

// TestLogViewSearch shows the lines that hold the search, in any case.
func TestLogViewSearch(t *testing.T) {
	t.Parallel()
	view := NewLogView(func(_ string, lines []string) []string { return lines })
	view.SetSize(30, 3)
	view.Log("traffic").Add("GET api.github.com", "deny example.com", "get pypi.org")
	view.Show("traffic")
	view.Keys(tea.KeyPressMsg{Code: '/', Text: "/"}, NewKeyMap())
	view.SearchKeys(tea.KeyPressMsg{Text: "GET"})
	view.SearchKeys(tea.KeyPressMsg{Code: tea.KeyEnter})
	if got := view.View(); !strings.Contains(got, "GET api.github.com") || !strings.Contains(got, "get pypi.org") ||
		strings.Contains(got, "example.com") {
		t.Errorf("the search for GET shows\n%s\nwant the two GET lines", got)
	}
}

// TestReadLinesPastALongLine cuts a line longer than maxLineBytes, and
// reads on past it to the end, so that the writer never waits.
func TestReadLinesPastALongLine(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 2*maxLineBytes)
	lines := []string{}
	err := ReadLines(strings.NewReader(long+"\nnext\n"), func(line string) { lines = append(lines, line) })
	if err != nil || len(lines) != 2 || len(lines[0]) > maxLineBytes+len("…") || lines[1] != "next" {
		t.Errorf("ReadLines = %v, %d lines; want the long line cut, then next", err, len(lines))
	}
}
