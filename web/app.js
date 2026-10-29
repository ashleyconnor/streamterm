/* streamterm client
 * ---------------------------------------------------------------------------
 * The whole client is a reducer over envelopes. Two rules cover everything:
 *
 *   line   -> append. Never revised, so it can be committed to the DOM once
 *             and left alone.
 *   window -> patch. The mutable tail of the emulated screen, addressed by row
 *             index. `len` gives the height; rows not mentioned are unchanged.
 *
 * Because `line` is append-only and carries a sequence number, resuming after
 * a dropped connection is just "send me everything after N". No diffing, no
 * reconciliation, no reading the whole log again.
 */

const $ = (id) => document.getElementById(id);

const el = {
  script: $('script'), run: $('run'), stop: $('stop'), liveOpt: $('live-window'),
  drop: $('drop'), resume: $('resume'), follow: $('follow'),
  well: $('well'), empty: $('empty'), finalized: $('finalized'),
  live: $('live'), seam: $('seam'), notices: $('notices'),
  wire: $('wire'), state: $('state'), dl: $('dl'),
  mScript: $('m-script'), mBytes: $('m-bytes'), mScreen: $('m-screen'),
  mEnv: $('m-env'), mConn: $('m-conn'), mLines: $('m-lines'),
};

const MAX_DOM_LINES = 4000;  // see note in trimDom()
const MAX_WIRE_ROWS = 120;

let es = null;          // active EventSource
let runId = null;
let cursor = 0;         // last durable sequence number applied
let envelopes = 0;
let finalLines = 0;
let finished = false;

/* ------------------------------------------------------------------ setup - */

async function loadScripts() {
  try {
    const res = await fetch('/api/scripts');
    const { scripts } = await res.json();
    if (!scripts || !scripts.length) {
      el.script.innerHTML = '<option value="">no scripts found</option>';
      el.run.disabled = true;
      return;
    }
    el.script.innerHTML = scripts
      .map((s) => `<option value="${s}">${s}</option>`)
      .join('');
  } catch {
    el.script.innerHTML = '<option value="">server unreachable</option>';
    el.run.disabled = true;
  }
}

/* -------------------------------------------------------------- run control */

async function startRun() {
  closeStream();
  resetView();

  const res = await fetch('/api/runs', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      script: el.script.value,
      live: el.liveOpt.checked,
    }),
  });

  if (!res.ok) {
    const { error } = await res.json().catch(() => ({}));
    addNotice(error || 'The run could not be started.');
    return;
  }

  ({ id: runId } = await res.json());
  cursor = 0;
  finished = false;

  el.dl.href = `/api/runs/${runId}/log.jsonl`;
  el.dl.hidden = false;
  el.stop.disabled = false;
  el.drop.disabled = false;
  el.run.disabled = true;

  openStream();
}

/* Reconnects from the client's own cursor. The browser does this by itself on
 * a network drop (it replays Last-Event-ID); ?from= is the explicit version,
 * used here so the Drop/Resume buttons can demonstrate the mechanism. */
function openStream() {
  const url = `/api/runs/${runId}/events` + (cursor ? `?from=${cursor}` : '');
  es = new EventSource(url);

  es.onopen = () => setConn('open');
  es.onmessage = (ev) => apply(JSON.parse(ev.data));

  es.addEventListener('eof', () => {
    closeStream();

    if (finished) {
      // The run is over and the durable log has been fully delivered. Closing
      // here stops EventSource from politely reconnecting forever.
      setConn('closed');
      el.drop.disabled = true;
      el.resume.disabled = true;
      return;
    }

    // The run is still going, so the server dropped us rather than ending the
    // stream — almost always because this client fell behind its send buffer.
    // That is a deliberate server-side choice: one slow reader must not stall
    // the pipeline for everyone. Recovery is simply to reconnect from the
    // cursor, which is the same path the Resume button takes.
    setConn(`dropped at ${cursor}, reconnecting`);
    setTimeout(() => { if (!finished) openStream(); }, 400);
  });

  es.onerror = () => {
    // Transport-level failure. EventSource retries on its own using the retry
    // interval the server sent, so this only reports state.
    if (es && es.readyState === EventSource.CONNECTING) setConn('retrying');
  };
}

function closeStream() {
  if (es) { es.close(); es = null; }
}

function stopRun() {
  if (runId) fetch(`/api/runs/${runId}/stop`, { method: 'POST' });
}

/* --------------------------------------------------------- envelope reducer */

function apply(e) {
  envelopes += 1;
  el.mEnv.textContent = envelopes.toLocaleString();

  // Only durable kinds advance the cursor. A window has no id on the wire, so
  // treating it as progress would let a resume skip the finalized lines that
  // were published after it.
  if (e.kind !== 'window' && e.seq) cursor = e.seq;

  if (typeof e.bytes === 'number' && e.bytes > 0) {
    el.mBytes.textContent = formatBytes(e.bytes);
  }

  switch (e.kind) {
    case 'meta':   onMeta(e);   break;
    case 'line':   onLine(e);   break;
    case 'window': onWindow(e); break;
    case 'status': onStatus(e); break;
    case 'notice': addNotice(e.text); break;
  }

  logToWire(e);
  if (el.follow.checked) el.well.scrollTop = el.well.scrollHeight;
}

function onMeta(e) {
  el.empty.hidden = true;
  el.mScript.textContent = e.script || '—';
  // An append-only run never sends a window, so there is no mutable depth to
  // report — saying "24 rows" would imply a live region that does not exist.
  el.mScreen.textContent = e.live === false ? 'append-only' : `${e.cols}×${e.rows}`;
}

function onLine(e) {
  const div = document.createElement('div');
  div.className = 'ln';
  div.id = `L${e.line}`;
  div.dataset.n = e.line;
  // Trusted by construction: this HTML is produced by terminal-to-html on the
  // server from the process's own bytes, and the library escapes the text it
  // renders. If any part of the input could come from an untrusted third
  // party, run the output through a sanitiser before this point — the library
  // is explicit that it is not hardened against XSS on its own.
  div.innerHTML = e.html || '&nbsp;';
  el.finalized.appendChild(div);

  finalLines += 1;
  el.mLines.textContent = `${finalLines.toLocaleString()} lines`;
  trimDom();
}

function onWindow(e) {
  const len = e.len || 0;
  el.seam.hidden = len === 0;

  // Grow or shrink the viewport to the height the server reports, then apply
  // the changed rows by index. A progress bar sends exactly one row per frame
  // instead of the whole window.
  const live = el.live;
  while (live.children.length > len) live.removeChild(live.lastChild);
  while (live.children.length < len) {
    const div = document.createElement('div');
    div.className = 'wl';
    div.innerHTML = '&nbsp;';
    live.appendChild(div);
  }

  for (const row of e.patch || []) {
    const node = live.children[row.i];
    // Trusted by construction: this HTML comes from terminal-to-html on the
    // server, which escapes the process bytes it renders. If any of the input
    // could originate from an untrusted third party, sanitise before this
    // point — the library is explicit that it is not XSS-hardened by itself.
    if (node) node.innerHTML = row.html || '&nbsp;';
  }
}

function onStatus(e) {
  // A signalled process has no exit code of its own, so bash reports -1.
  // Showing "stopped (exit -1)" is noise; the state already says everything.
  const showExit = e.exit != null && e.exit >= 0 && e.state !== 'running';
  el.state.textContent = showExit ? `${e.state} (exit ${e.exit})` : e.state;
  el.state.dataset.state = e.state;

  if (e.state !== 'running') {
    finished = true;
    el.run.disabled = false;
    el.stop.disabled = true;
  }
}

function addNotice(text) {
  const p = document.createElement('p');
  p.className = 'notice';
  p.textContent = text;
  el.notices.appendChild(p);
  el.empty.hidden = true;
}

/* ------------------------------------------------------------------- views */

function logToWire(e) {
  const li = document.createElement('li');

  const seq = document.createElement('span');
  seq.className = 'wire-seq';
  seq.textContent = e.kind === 'window' ? '·' : e.seq;

  const kind = document.createElement('span');
  kind.className = 'wire-kind';
  kind.dataset.kind = e.kind;
  kind.textContent = e.kind;

  const body = document.createElement('span');
  body.className = 'wire-body';
  body.textContent = summarise(e);

  li.append(seq, kind, body);
  el.wire.appendChild(li);

  while (el.wire.children.length > MAX_WIRE_ROWS) {
    el.wire.removeChild(el.wire.firstChild);
  }
  el.wire.scrollTop = el.wire.scrollHeight;
}

function summarise(e) {
  switch (e.kind) {
    case 'line':   return `line ${e.line}  ${textOf(e.html)}`;
    case 'window': return `${(e.patch || []).length} of ${e.len || 0} rows`;
    case 'status': return e.exit != null ? `${e.state}  exit ${e.exit}` : e.state;
    case 'meta':   return `${e.script}  ${e.cols}×${e.rows}`;
    case 'notice': return e.text;
    default:       return '';
  }
}

function textOf(html) {
  const d = document.createElement('div');
  d.innerHTML = html || '';
  return (d.textContent || '').slice(0, 90);
}

/* A real viewer virtualises this list — CI logs reach hundreds of megabytes
 * and the DOM gives out long before the data does. Dropping the oldest nodes
 * is the honest small-scale version: it keeps the page responsive and makes
 * the ceiling explicit rather than pretending there isn't one. */
function trimDom() {
  while (el.finalized.children.length > MAX_DOM_LINES) {
    el.finalized.removeChild(el.finalized.firstChild);
  }
}

function resetView() {
  el.finalized.replaceChildren();
  el.live.replaceChildren();
  el.notices.replaceChildren();
  el.wire.replaceChildren();
  el.seam.hidden = true;
  el.empty.hidden = false;
  envelopes = 0; finalLines = 0;
  el.mEnv.textContent = '0';
  el.mBytes.textContent = '0 B';
  el.mLines.textContent = '0 lines';
  el.mScreen.textContent = '—';
  el.mScript.textContent = '—';
  el.state.textContent = 'starting';
  el.state.dataset.state = 'running';
}

function setConn(s) {
  el.mConn.textContent = s;
}

function formatBytes(n) {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / 1024 / 1024).toFixed(2)} MB`;
}

/* ------------------------------------------------------------------ wiring */

el.run.addEventListener('click', startRun);
el.stop.addEventListener('click', stopRun);

el.drop.addEventListener('click', () => {
  closeStream();
  setConn(`dropped at ${cursor}`);
  el.drop.disabled = true;
  el.resume.disabled = false;
  addNotice(`Connection dropped after sequence ${cursor}. The script keeps ` +
            `running on the server. Resume to replay only what was missed.`);
});

el.resume.addEventListener('click', () => {
  el.resume.disabled = true;
  el.drop.disabled = false;
  addNotice(`Resuming from sequence ${cursor}.`);
  openStream();
});

// Turn Follow off as soon as the reader scrolls away from the bottom, and back
// on when they return. Fighting someone for control of the scroll position is
// the fastest way to make a live log unusable.
el.well.addEventListener('scroll', () => {
  const atBottom =
    el.well.scrollHeight - el.well.scrollTop - el.well.clientHeight < 24;
  if (!atBottom && el.follow.checked) el.follow.checked = false;
  if (atBottom && !el.follow.checked) el.follow.checked = true;
});

loadScripts();
