package run

import (
	"sync"
)

// Envelope is the single JSON object that travels on the wire. Everything the
// browser learns about a run arrives as one of these, in sequence order.
//
// Kinds:
//
//	meta    run started; carries the script name and screen geometry
//	line    a finalized line of output. Append-only, never revised.
//	window  the current live viewport. Replaces the previous window entirely.
//	status  run state change (running / succeeded / failed / stopped)
//	notice  out-of-band message from the server (truncation, slow consumer, ...)
//
// Only meta/line/status/notice are durable and replayed on reconnect. A window
// is ephemeral by definition: the newest one supersedes every earlier one, so
// there is no value in replaying stale viewports.
type Envelope struct {
	Seq  int64  `json:"seq"`
	T    int64  `json:"t"`    // milliseconds since the run started
	Kind string `json:"kind"` // meta | line | window | status | notice

	// kind=line
	Line int    `json:"line,omitempty"` // 1-based logical line number
	HTML string `json:"html,omitempty"`

	// kind=window. Patch is sparse, keyed by row index; Len is the window's
	// height, which also tells the client where to truncate.
	Patch []WindowRow `json:"patch,omitempty"`
	Len   *int        `json:"len,omitempty"`

	// kind=status
	State string `json:"state,omitempty"`
	Exit  *int   `json:"exit,omitempty"`

	// kind=meta
	Script string `json:"script,omitempty"`
	Cols   int    `json:"cols,omitempty"`
	Rows   int    `json:"rows,omitempty"`
	// Live reports whether this run has a mutable region. False means no
	// window envelope will ever be published, so the client can retire its
	// live region up front instead of inferring it from silence.
	Live *bool `json:"live,omitempty"`

	// kind=notice
	Text string `json:"text,omitempty"`

	// Running byte count of raw process output, attached to line/window events
	// so the UI can show throughput without a separate channel.
	Bytes int64 `json:"bytes,omitempty"`
}

// WindowRow is one changed row of the live viewport.
type WindowRow struct {
	I    int    `json:"i"`
	HTML string `json:"html"`
}

// hub fans one producer out to many subscribers, and keeps enough history to
// let a reconnecting client resume exactly where it left off.
type hub struct {
	mu      sync.Mutex
	seq     int64
	durable []Envelope // replayable history, ordered by Seq
	window  []string   // materialised viewport, rebuilt from window patches
	subs    map[chan Envelope]struct{}
	closed  bool
}

func newHub() *hub {
	return &hub{subs: make(map[chan Envelope]struct{})}
}

// publish assigns the next sequence number and broadcasts.
//
// Subscriber channels are buffered. If a subscriber has fallen far enough
// behind that its buffer is full, we drop it rather than blocking the
// producer. That is safe precisely because the transport is resumable: the
// browser reconnects with Last-Event-ID and replays what it missed. Blocking
// the producer to accommodate one slow reader would stall the pipeline for
// everyone, so backpressure is resolved by disconnection, not by waiting.
func (h *hub) publish(e Envelope) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}

	h.seq++
	e.Seq = h.seq

	if e.Kind == "window" {
		h.applyWindow(e)
	} else {
		h.durable = append(h.durable, e)
	}

	for ch := range h.subs {
		select {
		case ch <- e:
		default:
			close(ch)
			delete(h.subs, ch)
		}
	}
}

// subscribe returns everything the client is missing, followed by a live
// channel. Backlog and channel are captured under one lock so no event can
// slip between the replay and the live stream.
func (h *hub) subscribe(after int64, buffer int) (backlog []Envelope, ch chan Envelope, live bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	for _, e := range h.durable {
		if e.Seq > after {
			backlog = append(backlog, e)
		}
	}
	// The viewport is always sent in full, regardless of the client's cursor:
	// it is state, not an increment, and re-applying it is idempotent. Live
	// subscribers receive sparse patches; a joining subscriber receives every
	// row at once, materialised from those same patches.
	//
	// Sent even when the viewport is empty, which is the whole point of it being
	// state. A resuming client may still be holding rows on screen from before
	// it disconnected, and `len: 0` is the only thing that tells it to clear
	// them. Skipping the envelope — as this did while the emptiness looked like
	// "nothing to say" — strands the pre-drop window on screen forever,
	// most visibly when the run finishes while the client is away.
	patch := make([]WindowRow, len(h.window))
	for i, html := range h.window {
		patch[i] = WindowRow{I: i, HTML: html}
	}
	n := len(h.window)
	backlog = append(backlog, Envelope{
		Seq: h.seq, Kind: "window", Patch: patch, Len: &n,
	})

	if h.closed {
		return backlog, nil, false
	}

	ch = make(chan Envelope, buffer)
	h.subs[ch] = struct{}{}
	return backlog, ch, true
}

// applyWindow folds a patch into the materialised viewport. Caller holds mu.
func (h *hub) applyWindow(e Envelope) {
	n := 0
	if e.Len != nil {
		n = *e.Len
	}
	if n < len(h.window) {
		h.window = h.window[:n]
	}
	for len(h.window) < n {
		h.window = append(h.window, "")
	}
	for _, r := range e.Patch {
		if r.I >= 0 && r.I < len(h.window) {
			h.window[r.I] = r.HTML
		}
	}
}

func (h *hub) unsubscribe(ch chan Envelope) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[ch]; ok {
		delete(h.subs, ch)
		close(ch)
	}
}

// close ends the live stream. History remains readable, which is what lets a
// finished run be served from the durable log instead of the stream.
func (h *hub) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for ch := range h.subs {
		close(ch)
		delete(h.subs, ch)
	}
}

func (h *hub) snapshot() []Envelope {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Envelope, len(h.durable))
	copy(out, h.durable)
	return out
}
