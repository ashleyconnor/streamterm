package run

import (
	"strings"
	"testing"
)

// collect drives a renderer and returns a pointer to the finalized lines.
// A pointer, not a slice: the caller keeps writing after collect returns, and
// each of those writes can append more lines through the callback.
func collect(t *testing.T, cols, rows int, writes ...string) (*[]string, *renderer) {
	t.Helper()

	final := &[]string{}
	r, err := newRenderer(cols, rows, func(_ int, html string) {
		*final = append(*final, html)
	})
	if err != nil {
		t.Fatalf("newRenderer: %v", err)
	}
	for _, w := range writes {
		r.write([]byte(w))
	}
	return final, r
}

// A carriage-return progress bar must collapse to one line. This is the single
// behaviour that separates a screen emulator from a naive ANSI-to-HTML pass,
// and the reason the finalized log is trustworthy: forty rewrites of one row
// become one record, in its final state.
func TestCarriageReturnCollapsesToOneLine(t *testing.T) {
	var b strings.Builder
	for i := 0; i <= 40; i++ {
		b.WriteString("\rprogress ")
		b.WriteString(strings.Repeat("#", i))
	}
	b.WriteString("\ndone\n")

	finalp, r := collect(t, 80, 4, b.String())
	r.finish()

	if len(*finalp) != 2 {
		t.Fatalf("want 2 finalized lines, got %d: %q", len(*finalp), *finalp)
	}
	if got := (*finalp)[0]; !strings.Contains(got, strings.Repeat("#", 40)) {
		t.Errorf("first line is not the final frame of the bar: %q", got)
	}
	if strings.Count((*finalp)[0], "progress") != 1 {
		t.Errorf("intermediate frames leaked into the finalized line: %q", (*finalp)[0])
	}
}

// A style opened in one write and closed in a later one must survive the
// boundary. Chunk boundaries fall wherever the network puts them, so a
// renderer that rebuilt its parser per chunk would drop the colour here.
func TestStyleSurvivesChunkBoundary(t *testing.T) {
	finalp, r := collect(t, 80, 4,
		"\033[31mred begins", // colour opened, chunk ends
		" and continues",     // no escape at all in this chunk
		" then ends\033[0m\n",
	)
	r.finish()

	if len(*finalp) != 1 {
		t.Fatalf("want 1 finalized line, got %d: %q", len(*finalp), *finalp)
	}
	line := (*finalp)[0]
	if !strings.Contains(line, "term-fg31") {
		t.Errorf("colour class lost across the boundary: %q", line)
	}
	for _, want := range []string{"red begins", "and continues", "then ends"} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in %q", want, line)
		}
	}
}

// Lines finalize only once they have been pushed out of the emulated screen,
// which is what makes them safe to treat as immutable.
func TestFinalizationLagsByWindowHeight(t *testing.T) {
	const rows = 4

	finalp, r := collect(t, 80, rows, "a\nb\nc\n")
	if len(*finalp) != 0 {
		t.Fatalf("nothing should finalize while it is still on screen, got %q", *finalp)
	}

	// Push past the window; the earliest lines must now be committed.
	r.write([]byte("d\ne\nf\n"))
	if len(*finalp) == 0 {
		t.Fatal("lines pushed off the screen were never finalized")
	}
	if !strings.Contains((*finalp)[0], "a") {
		t.Errorf("finalized out of order, first line was %q", (*finalp)[0])
	}

	before := len(*finalp)
	r.finish()
	if len(*finalp) <= before {
		t.Error("finish() did not drain the remaining viewport")
	}
}

// The window diff must report only rows that actually changed.
func TestWindowDeltaIsSparse(t *testing.T) {
	_, r := collect(t, 80, 8, "one\ntwo\nthree\n")

	if _, _, changed := r.windowDelta(); !changed {
		t.Fatal("first delta should report a change")
	}
	if _, _, changed := r.windowDelta(); changed {
		t.Error("an unchanged screen should produce no window event")
	}

	// Rewrite only the current row.
	r.write([]byte("\rfour"))
	patch, n, changed := r.windowDelta()
	if !changed {
		t.Fatal("rewriting a row should produce a change")
	}
	if len(patch) != 1 {
		t.Errorf("want a 1-row patch, got %d rows of a %d-row window", len(patch), n)
	}
}
