package run

import "testing"

func windowOf(t *testing.T, envs []Envelope) (Envelope, bool) {
	t.Helper()
	for _, e := range envs {
		if e.Kind == "window" {
			return e, true
		}
	}
	return Envelope{}, false
}

func publishWindow(h *hub, rows ...string) {
	patch := make([]WindowRow, len(rows))
	for i, html := range rows {
		patch[i] = WindowRow{I: i, HTML: html}
	}
	n := len(rows)
	h.publish(Envelope{Kind: "window", Patch: patch, Len: &n})
}

// A subscriber joining mid-run gets the viewport materialised in full, however
// sparse the patches that built it were.
func TestSubscribeMaterialisesWindow(t *testing.T) {
	h := newHub()
	publishWindow(h, "one", "two", "three")
	h.publish(Envelope{Kind: "line", Line: 1, HTML: "gone"})
	publishWindow(h, "one", "TWO", "three") // sparse in principle, full here

	backlog, ch, live := h.subscribe(0, 8)
	if !live {
		t.Fatal("hub should still be live")
	}
	defer h.unsubscribe(ch)

	w, ok := windowOf(t, backlog)
	if !ok {
		t.Fatal("no window envelope in the backlog")
	}
	if w.Len == nil || *w.Len != 3 {
		t.Fatalf("want a 3-row window, got %v", w.Len)
	}
	if len(w.Patch) != 3 {
		t.Fatalf("want every row materialised, got %d", len(w.Patch))
	}
	if w.Patch[1].HTML != "TWO" {
		t.Errorf("materialised window is stale: %q", w.Patch[1].HTML)
	}
}

// The regression this file exists for.
//
// A client that disconnects while rows are on screen is still holding those
// rows. If the run drains its viewport while that client is away, the empty
// window is the only thing that can tell it to clear them — so an empty
// viewport must still produce an envelope. Suppressing it as "nothing to say"
// leaves the pre-drop window stranded on screen for good.
func TestSubscribeSendsEmptyWindowSoResumersCanClear(t *testing.T) {
	h := newHub()
	publishWindow(h, "still on screen")

	// The run drains the screen: those rows finalize and the viewport empties.
	h.publish(Envelope{Kind: "line", Line: 1, HTML: "still on screen"})
	publishWindow(h) // len 0

	backlog, ch, _ := h.subscribe(0, 8)
	if ch != nil {
		defer h.unsubscribe(ch)
	}

	w, ok := windowOf(t, backlog)
	if !ok {
		t.Fatal("an empty viewport must still be announced, or a resuming " +
			"client keeps rendering the window it had before it dropped")
	}
	if w.Len == nil || *w.Len != 0 {
		t.Fatalf("want len 0 so the client truncates, got %v", w.Len)
	}
}

// Same story once the run is over: the backlog is the client's last word from
// the server, so it has to carry the final viewport state too.
func TestSubscribeAfterCloseStillAnnouncesWindow(t *testing.T) {
	h := newHub()
	publishWindow(h, "a", "b")
	h.publish(Envelope{Kind: "line", Line: 1, HTML: "a"})
	publishWindow(h)
	h.publish(Envelope{Kind: "status", State: "succeeded"})
	h.close()

	backlog, _, live := h.subscribe(2, 8)
	if live {
		t.Fatal("a closed hub must not report itself live")
	}
	w, ok := windowOf(t, backlog)
	if !ok || w.Len == nil || *w.Len != 0 {
		t.Fatal("a client resuming after the run ended is never told to clear " +
			"its live region")
	}
}

// Window envelopes must not advance the durable history, or a resume would skip
// the finalized lines published after them.
func TestWindowsAreNotDurable(t *testing.T) {
	h := newHub()
	h.publish(Envelope{Kind: "line", Line: 1, HTML: "one"})
	publishWindow(h, "live row")
	h.publish(Envelope{Kind: "line", Line: 2, HTML: "two"})

	for _, e := range h.snapshot() {
		if e.Kind == "window" {
			t.Fatal("a window leaked into the durable log")
		}
	}

	// Resuming from the first line must still deliver the second.
	backlog, ch, _ := h.subscribe(1, 8)
	if ch != nil {
		defer h.unsubscribe(ch)
	}
	var lines int
	for _, e := range backlog {
		if e.Kind == "line" {
			lines++
			if e.Line != 2 {
				t.Errorf("resumed with the wrong line: %d", e.Line)
			}
		}
	}
	if lines != 1 {
		t.Fatalf("want exactly the one missed line, got %d", lines)
	}
}

// A subscriber that stops reading must be dropped, not waited on.
func TestSlowSubscriberIsDropped(t *testing.T) {
	h := newHub()
	_, ch, live := h.subscribe(0, 1)
	if !live {
		t.Fatal("hub should be live")
	}

	// Fill the buffer and then overrun it.
	for i := 0; i < 5; i++ {
		h.publish(Envelope{Kind: "line", Line: i + 1, HTML: "x"})
	}

	drained := 0
	for range ch { // a closed channel ends the range
		drained++
	}
	if drained > 2 {
		t.Errorf("producer buffered %d envelopes for a stalled reader", drained)
	}

	h.mu.Lock()
	subs := len(h.subs)
	h.mu.Unlock()
	if subs != 0 {
		t.Errorf("dropped subscriber was left registered (%d subs)", subs)
	}
}
