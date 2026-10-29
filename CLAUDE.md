# streamterm — working notes

Read `README.md` first for the architecture. This file covers only what the
code and the README don't already say: decisions whose rationale isn't obvious
from reading the source, the traps, and what's still open.

## Verify before and after changes

```sh
make test                       # unit tests, no server needed
make run &                      # serves on :8080
python3 tools/e2e.py            # disconnects mid-run and asserts recovery
```

`tools/e2e.py` is the real safety net. It asserts the properties the design
rests on: finalized lines contiguous with no duplicates across a reconnect,
carriage-return rewrites collapsing to one record in its final state,
`log.jsonl` matching what the stream delivered, a late subscriber receiving the
complete log, and every subscription announcing the viewport. If a change breaks
the streaming model, this is what catches it. Run it after touching anything in
`internal/run` or `internal/httpapi`.

Assertions about finalized lines alone are not enough, and historically were
not: the live region is separate client-side state, and a bug that froze it
mid-run left every line-based check passing. When you add a property here,
ask which of the two regions it actually covers.

The server serves `web/` through `go:embed`, so **editing CSS or JS does
nothing until you rebuild**. Check what is actually listening before trusting a
result — a stale `streamterm` holding `:8080` will make a fixed binary look
broken, because the new one exits on `bind: address already in use`:

```sh
lsof -nP -iTCP:8080 -sTCP:LISTEN
```

## Invariants — breaking these breaks the model

**Finalized lines are immutable.** Once `ScrollOutFunc` fires, that line is
committed with a permanent number. Nothing may revise or reorder it. The
sequence of `line` envelopes must be gap-free and strictly increasing.

**Window envelopes carry no SSE `id`.** A window is state, not an increment.
Giving one an id would let the client's cursor advance past it, so a reconnect
would skip finalized lines published afterwards. This is subtle and easy to
"fix" wrongly — see `send()` in `internal/httpapi/server.go`.

**Every subscription announces the viewport, empty or not.** The corollary of
the rule above: because windows are never replayed from the durable log, the
snapshot `hub.subscribe` appends is the *only* thing that can correct a
client's live region. A client that dropped with rows on screen is still
rendering them, so an empty viewport must still be sent as `len: 0` — that
envelope is what tells it to clear. Guarding the append with
`if len(h.window) > 0` reads like an obvious optimisation and is the bug it
used to have: resume after the run finished and the pre-drop window stayed
stranded on screen, with correct finalized lines scrolling above it.
`tools/e2e.py` now asserts this for both resume paths.

**Append-only runs publish no window envelopes.** `Config.Live=false` gives the
renderer a one-row screen, so nothing is ever rewritable, and `execute()` skips
both the per-flush window and the final `len: 0` one. The only window such a
client ever sees is the len-0 snapshot `hub.subscribe` adds, which is correct —
the viewport really is empty. Two things to preserve: `cfg.Rows` still feeds
`LINES` in `buildEnv`, so shrinking the *renderer* must not shrink what the
process is told (see `windowRows` in `execute()`); and `meta` carries `live` so
the client can retire its live region up front rather than inferring it from
never receiving a window.

**Ordering inside the run loop is load-bearing.** `rend.write()` may fire
`ScrollOutFunc` and publish `line` envelopes; the `window` envelope must be
published *after* that call returns, so it always describes the state after
those lines left. Reversing it shows lines twice. See `execute()` in
`internal/run/manager.go`.

**`maxLines == window height`** in `newRenderer`. That equality is what makes
"scrolled out of the buffer" and "scrolled off the screen" the same event. If
they diverge, lines either finalize while still rewritable or never finalize.

**Slow subscribers get dropped, not waited on.** `hub.publish` closes a
subscriber's channel when its buffer is full rather than blocking. That's
deliberate: one stalled reader must not stall the pipeline, and disconnection
is recoverable because the client resumes from its cursor. Don't "fix" this by
making the send blocking.

## Constraints

**Dependency pin.** `terminal-to-html` v3.17+ requires Go 1.25; v3.16.0 is
pinned to build on Go 1.22. Raising it is fine on a newer toolchain, but you
must re-run `make assets` in the same change: the CSS class names
(`term-fgx196`, `term-bgi4`, …) are an output contract of the library, so the
vendored stylesheet and the module version have to move as a pair.

**`web/vendor/terminal.css` is gitignored** and fetched by
`tools/fetch-assets.sh`. `go:embed all:web` needs the directory to exist, which
is why `web/vendor/.gitkeep` is checked in. Don't delete it.

**`assets.go` must stay at the module root.** `go:embed` only reaches into its
own package directory, so the embed can't live in `cmd/streamterm/`.

## Open work

**The UI has now been rendered in a browser** (desktop 1440×900 and mobile
375×812): idle, running, succeeded, `failed (exit 3)`, `stopped`, and a
drop/resume mid-run that ended with lines 1..439 contiguous in the DOM, no
duplicates and no gaps. Two defects were found and fixed in `web/app.css`, both
worth knowing about because they can be reintroduced by accident:

- A class that sets `display` beats the UA rule for the `hidden` attribute, so
  an element `app.js` hides by toggling `hidden` stays visible. This bit the
  since-removed seam divider. Any new class that sets `display` on an element
  `app.js` hides needs a matching `[hidden] { display: none }` guard.
- The vendored `terminal.css` sets `white-space: pre-wrap` and
  `overflow-wrap: break-word` on `.term-container`. A bounded cols×rows screen
  must not reflow, so both are overridden and the well scrolls in x instead.

The gutter arrangement that fix depends on is easy to break: `.term-container`
is `width: max-content; min-width: 100%` so every row is exactly as wide as the
widest line, and the line number is `position: sticky; left: 0` so it stays
legible while a long line is panned. Sticky is clamped to its own containing
block — give rows individual widths and the gutter goes ragged on pan. The
number's gap from the text is `padding-right`, not margin, so its opaque
background covers it; with a margin, panned text shows through.

**The gutter must total exactly 46px, and `box-sizing` is not inherited into
it.** `#live .wl` indents live rows with `padding-left: 46px`, so the finalized
rows' `::before` has to occupy the same 46px or the two regions step sideways
against each other — the terminal grid visibly breaks at the one boundary the
whole UI is built to show. The trap: the `* { box-sizing:
border-box }` reset does **not** match pseudo-elements (`*` matches elements
only), so `::before` defaults to `content-box` and `width: 46px` plus
`padding-right: 10px` silently measures 56px. The rule therefore sets
`box-sizing: border-box` explicitly. If you touch those numbers, measure it —
compare `Range.getBoundingClientRect().left` of a finalized row against a live
row; they must be equal.

Still unexamined: the coloured `.region` rule scrolls out of view when panned
right, and the wire pane does not re-scroll to the bottom after a viewport
resize. Both are cosmetic.

**No virtualisation.** `web/app.js` caps the DOM at 4,000 finalized lines and
drops the oldest (`trimDom`). Real logs need a windowing list. Note that line
numbers come from `data-n` on each node, not a CSS counter, precisely so
trimming doesn't renumber what remains — keep that property.

**No collapsible sections.** Adding GitHub-style `::group::` or GitLab-style
`section_start:` means splitting incoming bytes on `\n` *before* they reach the
`Screen`, tracking the open group, and attaching it as envelope metadata.
Don't try to detect them in the rendered HTML.

**A signalled run reports `exit -1`.** `cmd.Wait()` has no real exit code for a
signalled process. The client suppresses the number when state is `stopped`; if
you want the actual signal, capture it from `ProcessState.Sys()`.

## Scope

`internal/run/manager.go` resolves the script name against a fixed directory
and rejects anything that escapes it. The HTTP layer never names a path or a
command. Keep it that way — loosening it turns this into remote code execution.

Output is not sanitised. `terminal-to-html` escapes the process bytes it
renders but is explicitly not XSS-hardened. Safe here because the scripts are
yours; if input could ever come from a third party, sanitise before the
envelope.
