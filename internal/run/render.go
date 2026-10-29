package run

import (
	"strings"

	terminal "github.com/buildkite/terminal-to-html/v3"
)

// renderer turns a stream of raw ANSI bytes into two distinct outputs:
//
//	finalized lines — HTML that will never change again, emitted once
//	the live window — the mutable bottom of the screen, re-sent on change
//
// The split comes from how terminal-to-html bounds its buffer. Screen keeps at
// most maxLines of content; when a line is pushed past the top it is gone from
// the buffer forever, and ScrollOutFunc hands us its final HTML. By setting
// maxLines equal to the window height, "scrolled out of the buffer" and
// "scrolled off the visible screen" become the same event.
//
// This matters because the two halves have genuinely different semantics.
// Anything still on screen can be rewritten by a later escape sequence — a \r
// progress bar overwrites its own line dozens of times, cursor-up sequences
// repaint earlier rows. Those bytes are not safe to append to an immutable
// log. Once a line scrolls off, nothing can address it any more, so it is safe
// to assign a permanent line number and never touch it again.
type renderer struct {
	screen *terminal.Screen

	lineNo     int
	onFinal    func(lineNo int, html string)
	lastWindow []string
}

// newRenderer builds a screen whose scrollback is exactly one window tall.
func newRenderer(cols, rows int, onFinal func(lineNo int, html string)) (*renderer, error) {
	r := &renderer{onFinal: onFinal}

	// WithMaxSize caps both the buffer and the window. Passing the same row
	// count for both means every line that leaves the viewport also leaves the
	// buffer, and therefore fires ScrollOutFunc immediately.
	s, err := terminal.NewScreen(terminal.WithMaxSize(cols, rows))
	if err != nil {
		return nil, err
	}
	if err := s.SetSize(cols, rows); err != nil {
		return nil, err
	}
	s.ScrollOutFunc = func(html string) {
		r.lineNo++
		r.onFinal(r.lineNo, html)
	}

	r.screen = s
	return r, nil
}

// write feeds a batch of raw bytes through the ANSI parser. Parser state
// (current SGR style, cursor position, open hyperlinks) lives in the Screen
// and persists across calls, so a chunk boundary can fall anywhere — even in
// the middle of an escape sequence — without corrupting the output. This is
// the property that makes streaming rendering possible at all; a naive
// "convert each chunk independently" approach loses the style that was opened
// in a previous chunk.
func (r *renderer) write(p []byte) {
	_, _ = r.screen.Write(p)
}

// windowDelta returns only the rows that changed since the last call, plus the
// window's current height.
//
// A progress bar is the motivating case: it rewrites one row of a 24-row
// window many times a second. Sending the whole viewport each time would put
// 24 rows on the wire to convey one row of change. Diffing by index costs one
// string comparison per row and typically reduces a window event to a single
// entry.
//
// The window stays *state* rather than becoming an increment, because `len`
// plus the changed rows is enough to reconstruct it — the hub keeps a
// materialised copy so a client joining late still receives the whole
// viewport in one envelope.
func (r *renderer) windowDelta() (patch []WindowRow, length int, changed bool) {
	html := r.screen.AsHTML()

	var lines []string
	if html != "" {
		lines = strings.Split(html, "\n")
	}

	if equalStrings(lines, r.lastWindow) {
		return nil, 0, false
	}

	for i, l := range lines {
		if i >= len(r.lastWindow) || r.lastWindow[i] != l {
			patch = append(patch, WindowRow{I: i, HTML: l})
		}
	}

	r.lastWindow = append(r.lastWindow[:0:0], lines...)
	return patch, len(lines), true
}

// finish drains the viewport. Everything still on screen when the process
// exits can no longer be rewritten, so it is promoted to finalized lines and
// the window is emptied. After this the run's durable log is complete on its
// own — a client that only ever reads the log, never the live stream, sees the
// identical output.
func (r *renderer) finish() {
	html := r.screen.AsHTML()
	if html == "" {
		r.lastWindow = nil
		return
	}

	lines := strings.Split(html, "\n")
	// Trailing blank lines are an artifact of the final newline, not content.
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	for _, l := range lines {
		r.lineNo++
		r.onFinal(r.lineNo, l)
	}
	r.lastWindow = nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
