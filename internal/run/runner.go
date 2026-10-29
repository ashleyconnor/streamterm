package run

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// chunk is one batch of raw process output handed to the renderer.
type chunk struct {
	data      []byte
	truncated bool
}

// spawn starts the script and returns a channel of batched output plus a
// function that waits for exit.
//
// Deliberately NOT a PTY. Allocating a pseudo-terminal makes isatty() true,
// which unlocks color but also unlocks full-screen cursor addressing, alternate
// screen buffers, and terminal queries the process expects answers to. For a
// log viewer that is a lot of complexity bought for very little. Pipes plus
// forced-color environment variables get the color without the rest, and the
// byte stream stays close to linear — which is exactly what makes the
// finalized-line model in render.go sound.
//
// The tradeoff is real and worth stating: a program that ignores FORCE_COLOR
// and only checks isatty() will emit plain text here. If you need those,
// swap this function for a PTY (creack/pty) — nothing downstream changes,
// because everything downstream already handles arbitrary escape sequences.
func spawn(ctx context.Context, cfg Config) (<-chan chunk, func() (int, error), error) {
	cmd := exec.Command("bash", "--noprofile", "--norc", "-o", "pipefail", cfg.ScriptPath)
	cmd.Dir = cfg.WorkDir
	cmd.Env = buildEnv(cfg)
	cmd.Stdin = nil

	// One pipe, both streams. Assigning the same *os.File to Stdout and Stderr
	// makes os/exec pass a single file descriptor to the child, so ordering is
	// resolved by the kernel at write time rather than by a race between two
	// reader goroutines. Interleaving comes out the way a human sitting at the
	// terminal would have seen it.
	//
	// The cost is that stdout and stderr are no longer distinguishable. If you
	// need to tag them separately, use two pipes and a framing header per chunk
	// (this is what Docker's attach API does) and accept that ordering between
	// the two becomes approximate.
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	cmd.Stdout = pw
	cmd.Stderr = pw

	// Own process group, so a stuck script that has spawned children can be
	// killed as a tree instead of leaking orphans.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return nil, nil, err
	}
	// The child holds its own dup of the write end. Closing the parent's copy
	// is what lets the reader see EOF when the child exits; forgetting this is
	// the classic hang.
	pw.Close()

	out := make(chan chunk, 64)
	go readBatched(pr, out, cfg)

	// Cancellation: SIGTERM the group, then SIGKILL if it does not go quietly.
	var once sync.Once
	stop := func() {
		once.Do(func() {
			pgid, err := syscall.Getpgid(cmd.Process.Pid)
			if err != nil {
				_ = cmd.Process.Kill()
				return
			}
			_ = syscall.Kill(-pgid, syscall.SIGTERM)
			time.AfterFunc(cfg.KillGrace, func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
		})
	}

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			stop()
		case <-done:
		}
	}()

	wait := func() (int, error) {
		err := cmd.Wait()
		close(done)
		pr.Close()

		var ee *exec.ExitError
		switch {
		case err == nil:
			return 0, nil
		case errors.As(err, &ee):
			return ee.ExitCode(), nil
		default:
			return -1, err
		}
	}

	return out, wait, nil
}

// buildEnv constructs a minimal environment that persuades well-behaved tools
// to emit color even though they are writing to a pipe.
//
// The inherited environment is deliberately not passed through. A build agent
// runs untrusted-ish code; leaking the server's environment into it is how
// credentials escape. Everything the script needs is listed explicitly.
func buildEnv(cfg Config) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"LANG=C.UTF-8",

		// Claim a color-capable terminal type. Tools that consult terminfo
		// rather than isatty() key off this.
		"TERM=xterm-256color",

		// The three conventions that between them cover most of the ecosystem.
		// FORCE_COLOR is honoured by Node, npm, pytest, cargo and others;
		// level 3 requests truecolor. CLICOLOR_FORCE is the BSD/coreutils
		// lineage. CI=true flips many tools into a non-interactive mode that
		// still keeps color while dropping cursor-heavy animations.
		"FORCE_COLOR=3",
		"CLICOLOR_FORCE=1",
		"CI=true",

		// Without this, Python buffers 8 KB at a time when stdout is a pipe,
		// and a "live" log arrives in silent lumps. Most runtimes have an
		// equivalent knob; this is the one people forget.
		"PYTHONUNBUFFERED=1",

		// Programs that lay out tables or progress bars ask the terminal for
		// its width, get nothing useful from a pipe, and fall back to 80. Tell
		// them the width the renderer is actually emulating so their line
		// wrapping matches ours.
		"COLUMNS=" + strconv.Itoa(cfg.Cols),
		"LINES=" + strconv.Itoa(cfg.Rows),
	}
	// NO_COLOR is intentionally never forwarded: it is the one variable whose
	// mere presence, at any value, disables color everywhere.
	return env
}

// readBatched reads the pipe as fast as it can and emits accumulated bytes on
// an interval.
//
// Two separate concerns are being solved here. Reading eagerly keeps the pipe
// buffer drained so the child never blocks on write. Emitting on an interval
// keeps a chatty producer from turning into one network frame per printf. A
// script writing 10k short lines produces ~10k tiny writes and, at a 60 ms
// flush, well under a hundred network events.
func readBatched(r io.Reader, out chan<- chunk, cfg Config) {
	defer close(out)

	var (
		mu        sync.Mutex
		pending   []byte
		total     int64
		truncated bool
		eof       bool
	)

	flush := func() {
		mu.Lock()
		if len(pending) == 0 {
			mu.Unlock()
			return
		}
		data := pending
		pending = nil
		trunc := truncated
		truncated = false
		mu.Unlock()

		out <- chunk{data: data, truncated: trunc}
	}

	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				mu.Lock()
				// Hard cap on total output. Past the limit we keep reading (so
				// the child is never blocked on a full pipe) but stop
				// forwarding, and raise the flag exactly once.
				if cfg.MaxBytes > 0 && total >= cfg.MaxBytes {
					if !truncated {
						truncated = true
					}
				} else {
					pending = append(pending, buf[:n]...)
					total += int64(n)
				}
				full := len(pending) >= cfg.MaxChunk
				mu.Unlock()

				// Size-triggered flush keeps latency bounded for a burst that
				// would otherwise sit in the buffer until the next tick.
				if full {
					flush()
				}
			}
			if err != nil {
				mu.Lock()
				eof = true
				mu.Unlock()
				return
			}
		}
	}()

	ticker := time.NewTicker(cfg.FlushInterval)
	defer ticker.Stop()
	for range ticker.C {
		flush()

		mu.Lock()
		finished := eof && len(pending) == 0
		mu.Unlock()
		if finished {
			return
		}
	}
}
