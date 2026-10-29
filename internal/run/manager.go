package run

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Config holds the knobs for a single run.
type Config struct {
	ScriptPath string
	ScriptName string
	WorkDir    string

	// Cols/Rows define the emulated screen. Rows doubles as the depth of the
	// mutable region: a line is finalized once Rows further lines have been
	// printed below it.
	Cols int
	Rows int

	// Live controls whether the run has a mutable region at all.
	//
	// With it off, the renderer's screen is one row tall, so a line is pushed
	// out — and therefore committed — the moment the next one begins, and no
	// window envelopes are published. That is the append-only model a CI log
	// viewer uses: nothing already emitted can be revised.
	//
	// Cols/Rows still describe the environment handed to the process, so
	// COLUMNS/LINES do not change under the script's feet.
	Live bool

	FlushInterval time.Duration
	MaxChunk      int
	MaxBytes      int64
	KillGrace     time.Duration
}

func DefaultConfig() Config {
	return Config{
		Cols:          160,
		Rows:          24,
		Live:          true,
		FlushInterval: 60 * time.Millisecond,
		MaxChunk:      16 * 1024,
		MaxBytes:      32 * 1024 * 1024,
		KillGrace:     3 * time.Second,
	}
}

// Run is one execution of a script.
type Run struct {
	ID      string
	Script  string
	Started time.Time

	hub    *hub
	cancel context.CancelFunc

	mu    sync.Mutex
	state string // running | succeeded | failed | stopped | error
	exit  *int
}

func (r *Run) State() (string, *int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state, r.exit
}

func (r *Run) Stop() { r.cancel() }

// Subscribe resumes from `after` (a sequence number, 0 for the beginning).
// Returns the missed backlog plus a live channel, or a nil channel if the run
// has already finished — in which case the backlog is the complete log.
func (r *Run) Subscribe(after int64, buffer int) ([]Envelope, chan Envelope, bool) {
	return r.hub.subscribe(after, buffer)
}

func (r *Run) Unsubscribe(ch chan Envelope) { r.hub.unsubscribe(ch) }

// Log returns the full durable envelope history. This is the "finished run"
// path: once a run completes, clients read this instead of opening a stream,
// the same way a CI system serves a completed job from object storage rather
// than from its live-log service.
func (r *Run) Log() []Envelope { return r.hub.snapshot() }

// Manager owns the script directory and the set of runs.
type Manager struct {
	scriptDir string
	cfg       Config

	mu      sync.Mutex
	runs    map[string]*Run
	order   []string
	maxRuns int
}

func NewManager(scriptDir string, cfg Config) *Manager {
	return &Manager{
		scriptDir: scriptDir,
		cfg:       cfg,
		runs:      make(map[string]*Run),
		maxRuns:   20,
	}
}

// Scripts lists the runnable scripts.
func (m *Manager) Scripts() ([]string, error) {
	entries, err := os.ReadDir(m.scriptDir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sh") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// Get returns a run by id.
func (m *Manager) Get(id string) (*Run, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[id]
	return r, ok
}

// Start validates the script name and launches it.
//
// The name is resolved against a fixed directory and rejected if it escapes.
// The HTTP layer never gets to name a path or a command — the only thing a
// client can choose is which of the server's own scripts to run. Anything
// looser here turns the whole service into remote code execution.
// live=false runs the script in append-only mode; see Config.Live.
func (m *Manager) Start(name string, live bool) (*Run, error) {
	if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return nil, fmt.Errorf("invalid script name")
	}
	if !strings.HasSuffix(name, ".sh") {
		return nil, fmt.Errorf("invalid script name")
	}

	abs, err := filepath.Abs(filepath.Join(m.scriptDir, name))
	if err != nil {
		return nil, err
	}
	root, err := filepath.Abs(m.scriptDir)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(abs, root+string(os.PathSeparator)) {
		return nil, fmt.Errorf("invalid script name")
	}
	if st, err := os.Stat(abs); err != nil || st.IsDir() {
		return nil, fmt.Errorf("no such script: %s", name)
	}

	cfg := m.cfg
	cfg.ScriptPath = abs
	cfg.ScriptName = name
	cfg.WorkDir = root
	cfg.Live = live

	ctx, cancel := context.WithCancel(context.Background())
	r := &Run{
		ID:      newID(),
		Script:  name,
		Started: time.Now(),
		hub:     newHub(),
		cancel:  cancel,
		state:   "running",
	}

	m.mu.Lock()
	m.runs[r.ID] = r
	m.order = append(m.order, r.ID)
	for len(m.order) > m.maxRuns {
		delete(m.runs, m.order[0])
		m.order = m.order[1:]
	}
	m.mu.Unlock()

	go r.execute(ctx, cfg)
	return r, nil
}

// execute is the pipeline: bytes in, envelopes out.
func (r *Run) execute(ctx context.Context, cfg Config) {
	defer r.hub.close()

	start := time.Now()
	ms := func() int64 { return time.Since(start).Milliseconds() }

	var bytesSeen int64

	// A one-row screen has no room to hold anything back: each line is pushed
	// out as the next begins, so "still rewritable" collapses to nothing. The
	// process is unaffected — it still sees the configured LINES.
	windowRows := cfg.Rows
	if !cfg.Live {
		windowRows = 1
	}

	rend, err := newRenderer(cfg.Cols, windowRows, func(lineNo int, html string) {
		r.hub.publish(Envelope{
			T:     ms(),
			Kind:  "line",
			Line:  lineNo,
			HTML:  html,
			Bytes: bytesSeen,
		})
	})
	if err != nil {
		r.fail("error", nil, fmt.Sprintf("renderer: %v", err), ms())
		return
	}

	live := cfg.Live
	r.hub.publish(Envelope{
		T: ms(), Kind: "meta",
		Script: cfg.ScriptName, Cols: cfg.Cols, Rows: cfg.Rows, Live: &live,
	})
	r.hub.publish(Envelope{T: ms(), Kind: "status", State: "running"})

	chunks, wait, err := spawn(ctx, cfg)
	if err != nil {
		r.fail("error", nil, fmt.Sprintf("spawn: %v", err), ms())
		return
	}

	warnedTruncation := false
	for c := range chunks {
		bytesSeen += int64(len(c.data))

		// Order is load-bearing. write() may fire ScrollOutFunc, which
		// publishes finalized lines; the window published afterwards therefore
		// always describes the state *after* those lines left. A client
		// applying events in sequence order can never show a line twice.
		rend.write(c.data)

		// In append-only mode there is no mutable region to describe, so the
		// window envelope is not merely empty — it is never sent at all.
		if cfg.Live {
			if patch, n, changed := rend.windowDelta(); changed {
				r.hub.publish(Envelope{
					T: ms(), Kind: "window", Patch: patch, Len: &n, Bytes: bytesSeen,
				})
			}
		}

		if c.truncated && !warnedTruncation {
			warnedTruncation = true
			r.hub.publish(Envelope{
				T: ms(), Kind: "notice",
				Text: fmt.Sprintf("Output limit of %d MB reached. The script is still running, but the rest of its output is not being recorded.", cfg.MaxBytes/(1024*1024)),
			})
		}
	}

	// The process is done writing: promote whatever is left on screen.
	rend.finish()

	// Clearing the viewport only means something if there was one. An
	// append-only run publishes no window envelopes at all; the len-0 snapshot
	// that `hub.subscribe` gives every subscriber already says the region is
	// empty, and it has been empty all along.
	if cfg.Live {
		zero := 0
		r.hub.publish(Envelope{T: ms(), Kind: "window", Len: &zero, Bytes: bytesSeen})
	}

	code, werr := wait()

	state := "succeeded"
	switch {
	case werr != nil:
		state = "error"
	case ctx.Err() != nil:
		state = "stopped"
	case code != 0:
		state = "failed"
	}

	r.mu.Lock()
	r.state = state
	r.exit = &code
	r.mu.Unlock()

	if werr != nil {
		r.hub.publish(Envelope{T: ms(), Kind: "notice", Text: werr.Error()})
	}
	r.hub.publish(Envelope{
		T: ms(), Kind: "status", State: state, Exit: &code, Bytes: bytesSeen,
	})
}

func (r *Run) fail(state string, exit *int, msg string, t int64) {
	r.mu.Lock()
	r.state = state
	r.exit = exit
	r.mu.Unlock()
	r.hub.publish(Envelope{T: t, Kind: "notice", Text: msg})
	r.hub.publish(Envelope{T: t, Kind: "status", State: state, Exit: exit})
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
