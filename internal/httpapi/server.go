package httpapi

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ashleyconnor/streamterm/internal/run"
)

type Server struct {
	mgr    *run.Manager
	static fs.FS
	mux    *http.ServeMux
}

func New(mgr *run.Manager, static fs.FS) *Server {
	s := &Server{mgr: mgr, static: static, mux: http.NewServeMux()}

	s.mux.Handle("GET /", http.FileServer(http.FS(static)))
	s.mux.HandleFunc("GET /api/scripts", s.handleScripts)
	s.mux.HandleFunc("POST /api/runs", s.handleStartRun)
	s.mux.HandleFunc("GET /api/runs/{id}/events", s.handleEvents)
	s.mux.HandleFunc("GET /api/runs/{id}/log.jsonl", s.handleLog)
	s.mux.HandleFunc("POST /api/runs/{id}/stop", s.handleStop)

	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) handleScripts(w http.ResponseWriter, r *http.Request) {
	scripts, err := s.mgr.Scripts()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "Cannot read the script directory.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"scripts": scripts})
}

func (s *Server) handleStartRun(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Script string `json:"script"`
		// A pointer so an omitted field keeps the live window rather than
		// silently reading as false.
		Live *bool `json:"live"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		httpError(w, http.StatusBadRequest, "Request body must be JSON with a script field.")
		return
	}

	live := true
	if body.Live != nil {
		live = *body.Live
	}

	rn, err := s.mgr.Start(body.Script, live)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":     rn.ID,
		"script": rn.Script,
		"live":   live,
	})
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	rn, ok := s.mgr.Get(r.PathValue("id"))
	if !ok {
		httpError(w, http.StatusNotFound, "No such run.")
		return
	}
	rn.Stop()
	w.WriteHeader(http.StatusAccepted)
}

// handleLog serves the complete durable envelope history as JSON Lines.
//
// This is the counterpart to the live stream, and the reason the envelope
// format is worth having: the exact same records that were pushed over SSE can
// be replayed from a flat file. In a real system this file would be written to
// object storage when the run ends and served from a CDN, and the streaming
// endpoint would be retired for that run.
func (s *Server) handleLog(w http.ResponseWriter, r *http.Request) {
	rn, ok := s.mgr.Get(r.PathValue("id"))
	if !ok {
		httpError(w, http.StatusNotFound, "No such run.")
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	enc := json.NewEncoder(w)
	// The payload is rendered HTML delivered as JSON, never interpolated into
	// an HTML document by the server. Go escapes <, > and & by default, which
	// here inflates every span tag for no benefit.
	enc.SetEscapeHTML(false)
	for _, e := range rn.Log() {
		if err := enc.Encode(e); err != nil {
			return
		}
	}
}

// handleEvents streams envelopes over Server-Sent Events.
//
// SSE is the right shape for this problem. The channel is one-directional,
// which matches read-only exactly — there is no inbound message handler to
// forget to secure. And the resume protocol is free: the browser remembers the
// last id it saw and replays it in Last-Event-ID on reconnect, so the cursor
// lives in the client without any application code.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	rn, ok := s.mgr.Get(r.PathValue("id"))
	if !ok {
		httpError(w, http.StatusNotFound, "No such run.")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		httpError(w, http.StatusInternalServerError, "Streaming is not supported by this server.")
		return
	}

	// The cursor: Last-Event-ID on an automatic browser reconnect, ?from= when
	// the client reconnects deliberately. Both name the last sequence number
	// successfully applied, so the server resumes at cursor+1.
	after := parseCursor(r.Header.Get("Last-Event-ID"))
	if after == 0 {
		after = parseCursor(r.URL.Query().Get("from"))
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	// Defeats proxy buffering, which otherwise holds the stream until some
	// buffer fills and makes a live log look frozen.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// Tell the browser how long to wait before reconnecting.
	_, _ = w.Write([]byte("retry: 1500\n\n"))
	flusher.Flush()

	backlog, ch, live := rn.Subscribe(after, 512)

	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false) // see handleLog
	send := func(e run.Envelope) bool {
		// Only durable kinds carry an id. A window is state rather than an
		// increment, so advancing the client's cursor past one would let a
		// reconnect skip finalized lines that came after it.
		if e.Kind != "window" {
			if _, err := w.Write([]byte("id: " + strconv.FormatInt(e.Seq, 10) + "\n")); err != nil {
				return false
			}
		}
		if _, err := w.Write([]byte("data: ")); err != nil {
			return false
		}
		if err := enc.Encode(e); err != nil { // Encode appends the newline
			return false
		}
		if _, err := w.Write([]byte("\n")); err != nil {
			return false
		}
		return true
	}

	for _, e := range backlog {
		if !send(e) {
			if ch != nil {
				rn.Unsubscribe(ch)
			}
			return
		}
	}
	flusher.Flush()

	if !live {
		// Run already finished; the backlog was the whole story.
		_, _ = w.Write([]byte("event: eof\ndata: {}\n\n"))
		flusher.Flush()
		return
	}
	defer rn.Unsubscribe(ch)

	ctx := r.Context()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case e, open := <-ch:
			if !open {
				// Either the run ended or this subscriber fell too far behind
				// to keep up. Both are recoverable by reconnecting, which the
				// client will do off the back of the eof event.
				_, _ = w.Write([]byte("event: eof\ndata: {}\n\n"))
				flusher.Flush()
				return
			}
			if !send(e) {
				return
			}
			flusher.Flush()

		case <-heartbeat.C:
			// A comment frame. Keeps idle intermediaries from reaping a
			// connection during a long silent step.
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func parseCursor(s string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
