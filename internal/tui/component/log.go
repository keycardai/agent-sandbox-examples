package component

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/charmbracelet/x/ansi"
)

const (
	// MaxLines bounds each log the TUI keeps. Older lines drop off the top.
	MaxLines = 5000
	// tailEvery is how often a file tail checks for new lines.
	tailEvery = 300 * time.Millisecond
	// maxLineBytes bounds one line read from a stream.
	maxLineBytes = 1 << 20
	// tabSpaces is what a tab becomes, as lipgloss draws one.
	tabSpaces = "    "
)

// Log holds the last MaxLines lines of one log. Writes come from the
// goroutine that reads the log, and reads from the TUI, so it locks.
type Log struct {
	mu sync.Mutex
	// lines is a ring of up to MaxLines lines, the oldest at start.
	lines []string
	start int
	// partial is a line that waits for its newline, cut at maxLineBytes,
	// and cut reports whether it lost bytes there.
	partial string
	cut     bool
	// version counts the writes, so the TUI renders a log again only when
	// it changed.
	version int
}

// Write splits data into lines. A line without its newline waits for the
// next Write. A line keeps its first maxLineBytes and ends in …, as
// ReadLines cuts one.
func (b *Log) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	parts := strings.Split(string(data), "\n")
	for _, part := range parts[:len(parts)-1] {
		b.extendPartial(part)
		b.appendLocked(lineText([]byte(b.partial), b.cut))
		b.partial, b.cut = "", false
	}
	b.extendPartial(parts[len(parts)-1])
	// A partial line shows too, such as a progress bar that has no newline.
	b.version++
	return len(data), nil
}

// Add appends whole lines. The log keeps each line as the terminal shows
// it, without escape sequences or control characters, because a log holds
// what the agent and the tools print.
func (b *Log) Add(lines ...string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.appendLocked(lines...)
}

// Reset empties the log.
func (b *Log) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lines, b.start, b.partial, b.cut = nil, 0, "", false
	b.version++
}

// Snapshot returns the lines, with a partial last line, and the version.
func (b *Log) Snapshot() ([]string, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	lines := make([]string, 0, len(b.lines)+1)
	lines = append(lines, b.lines[b.start:]...)
	lines = append(lines, b.lines[:b.start]...)
	if b.partial != "" {
		lines = append(lines, shown(lineText([]byte(b.partial), b.cut)))
	}
	return lines, b.version
}

// Len returns how many lines the log holds, with a partial last line.
func (b *Log) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.partial != "" {
		return len(b.lines) + 1
	}
	return len(b.lines)
}

// Version returns how many times the log changed.
func (b *Log) Version() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.version
}

// extendPartial adds text to the partial line, up to maxLineBytes. The rest
// of the line is dropped. The caller holds b.mu.
func (b *Log) extendPartial(text string) {
	keep := min(len(text), maxLineBytes-len(b.partial))
	b.partial += text[:keep]
	b.cut = b.cut || keep < len(text)
}

// appendLocked adds lines to the ring. Once it holds MaxLines, each line
// replaces the oldest. The caller holds b.mu.
func (b *Log) appendLocked(lines ...string) {
	if len(lines) == 0 {
		return
	}
	for _, line := range lines {
		if len(b.lines) < MaxLines {
			b.lines = append(b.lines, shown(line))
			continue
		}
		b.lines[b.start] = shown(line)
		b.start = (b.start + 1) % MaxLines
	}
	b.version++
}

// shown is the text a terminal shows of line. A carriage return redraws
// the line, as a progress bar does, so the text after the last one shows.
// A tab becomes spaces. Escape sequences and other control characters go.
func shown(line string) string {
	line = strings.TrimRight(ansi.Strip(line), "\r")
	if i := strings.LastIndexByte(line, '\r'); i >= 0 {
		line = line[i+1:]
	}
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.ReplaceAll(line, "\t", tabSpaces))
}

// TailFile copies the lines of the file at path into log until ctx ends.
// It starts at the beginning, picks up lines as the file grows, and starts
// over when the file is truncated or replaced, as a service's log is when
// the service starts again.
func TailFile(ctx context.Context, path string, log *Log) {
	var offset int64
	var ident os.FileInfo
	for {
		info, err := os.Stat(path)
		switch {
		case err != nil:
		case ident == nil || !os.SameFile(ident, info) || info.Size() < offset:
			log.Reset()
			ident = info
			offset = readFrom(path, 0, log)
		case info.Size() > offset:
			offset = readFrom(path, offset, log)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(tailEvery):
		}
	}
}

// ReadLines calls each with every line of r, until r ends. A line longer
// than maxLineBytes is cut there and ends in …, and the rest of it is read
// and dropped, so that the writer never waits on a long line.
func ReadLines(r io.Reader, each func(line string)) error {
	reader := bufio.NewReader(r)
	var line []byte
	cut := false
	for {
		chunk, err := reader.ReadSlice('\n')
		keep := min(len(chunk), maxLineBytes-len(line))
		line = append(line, chunk[:keep]...)
		cut = cut || len(bytes.TrimRight(chunk[keep:], "\r\n")) > 0
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err == nil || len(line) > 0 {
			each(lineText(line, cut))
		}
		line, cut = line[:0], false
		switch {
		case err == nil:
		case errors.Is(err, io.EOF), errors.Is(err, os.ErrClosed):
			return nil
		default:
			return err //nolint:wrapcheck // The caller names the stream.
		}
	}
}

// lineText is a line without its line ending, and with … when it was cut.
func lineText(line []byte, cut bool) string {
	text := strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r")
	if cut {
		return strings.ToValidUTF8(text, "") + "…"
	}
	return text
}

// readFrom copies the file from offset to its end into log, and returns the
// new offset.
func readFrom(path string, offset int64, log *Log) int64 {
	file, err := os.Open(path) //nolint:gosec // G304: the path is a service log under local/out.
	if err != nil {
		return offset
	}
	defer func() { _ = file.Close() }()
	if _, err = file.Seek(offset, io.SeekStart); err != nil {
		return offset
	}
	n, _ := io.Copy(log, file)
	return offset + n
}
