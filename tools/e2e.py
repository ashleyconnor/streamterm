#!/usr/bin/env python3
"""End-to-end check against a running streamterm server.

    make run &
    python3 tools/e2e.py

Asserts the properties the architecture actually rests on: that finalized lines
are contiguous and never duplicated across a reconnect, that carriage-return
rewrites collapse to a single record, that the durable log and the live stream
tell the identical story, and that every subscription states the viewport so a
resuming client can re-sync the one region it cannot reconstruct itself.
"""
import json
import os
import time
import urllib.request
from html.parser import HTMLParser

BASE = os.environ.get("STREAMTERM", "http://127.0.0.1:8080")

def start(script, live=None):
    body={"script":script}
    if live is not None: body["live"]=live
    req=urllib.request.Request(BASE+"/api/runs", data=json.dumps(body).encode(),
                               headers={"Content-Type":"application/json"}, method="POST")
    return json.load(urllib.request.urlopen(req))["id"]

def read_for(url, seconds, headers=None, until_end=False):
    """Consume SSE and return the parsed envelopes.

    With until_end the read stops as soon as the run reports a terminal status
    or the server sends eof; `seconds` is then only a ceiling. Waiting on the
    run's own signals rather than on a stopwatch keeps this honest when the
    demo scripts change pace — an earlier version hard-coded durations that
    happened to outlast demo.sh, so slowing the script would have silently
    truncated the stream and failed the log-vs-stream comparison.
    """
    req=urllib.request.Request(url, headers=headers or {})
    out=[]; deadline=time.time()+seconds
    r=urllib.request.urlopen(req)
    buf=b""; done=False
    while not done and time.time()<deadline:
        chunk=r.read1(4096)
        if not chunk: break
        buf+=chunk
        while b"\n" in buf:
            line,buf=buf.split(b"\n",1)
            if not line.startswith(b"data: "): continue
            e=json.loads(line[6:])
            if "kind" not in e:              # the eof frame is `data: {}`
                if until_end: done=True
                continue
            out.append(e)
            if until_end and e["kind"]=="status" and e.get("state")!="running":
                done=True
    r.close()
    return out

# --- the page's element ids must be unique -----------------------------------
#
# getElementById returns the first match in document order, so a duplicate id
# does not error — it silently hands one component's element to another. A
# checkbox that borrowed id="live" from the live window's container once
# swallowed every row the renderer appended: the seam appeared, nothing showed
# beneath it, and DOM-querying checks agreed with the bug because they resolved
# the same wrong element.
class _Ids(HTMLParser):
    def __init__(self):
        super().__init__(); self.ids=[]
    def handle_starttag(self, tag, attrs):
        for k,v in attrs:
            if k=="id" and v: self.ids.append(v)

_p=_Ids(); _p.feed(urllib.request.urlopen(BASE+"/").read().decode())
_dupes=sorted({i for i in _p.ids if _p.ids.count(i)>1})
assert not _dupes, f"duplicate element id(s) served by index.html: {_dupes}"
print(f"index.html: {len(_p.ids)} element ids, all unique")

rid=start("demo.sh")
print("run", rid)

# Segment 1: read the first ~3 s, then hang up mid-run.
seg1=read_for(f"{BASE}/api/runs/{rid}/events", 3.0)
cursor=max([e["seq"] for e in seg1 if e["kind"]!="window"], default=0)
lines1=[e for e in seg1 if e["kind"]=="line"]
print(f"segment 1: {len(seg1)} envelopes, {len(lines1)} lines, cursor={cursor}")

time.sleep(2.0)   # stay disconnected while the script keeps running

# Segment 2a: resume via the ?from= query parameter, and stay until the run ends.
seg2=read_for(f"{BASE}/api/runs/{rid}/events?from={cursor}", 90.0, until_end=True)
lines2=[e for e in seg2 if e["kind"]=="line"]
print(f"segment 2: {len(seg2)} envelopes, {len(lines2)} lines")

# Segment 2b: same thing via the Last-Event-ID header the browser sends.
rid2=start("failing.sh")
a=read_for(f"{BASE}/api/runs/{rid2}/events", 1.0)
cur2=max([e["seq"] for e in a if e["kind"]!="window"], default=0)
b=read_for(f"{BASE}/api/runs/{rid2}/events", 60.0, {"Last-Event-ID": str(cur2)}, until_end=True)
hdr_ok = all(e["seq"] > cur2 for e in b if e["kind"] != "window")
print(f"Last-Event-ID header honoured: {hdr_ok}")

# --- assertions ---
seqs=[e["seq"] for e in lines1+lines2]
assert seqs==sorted(seqs), "line sequence numbers out of order"
assert len(set(seqs))==len(seqs), "DUPLICATE line delivered across the reconnect"

nums=[e["line"] for e in lines1+lines2]
assert nums==list(range(1,len(nums)+1)), f"GAP in line numbering: {nums[:5]}..{nums[-5:]}"
print(f"line numbers contiguous 1..{len(nums)}  no duplicates, no gaps")

# The durable log must equal what the stream delivered.
log=[json.loads(l) for l in urllib.request.urlopen(f"{BASE}/api/runs/{rid}/log.jsonl").read().decode().splitlines()]
log_lines=[e for e in log if e["kind"]=="line"]
print(f"log.jsonl: {len(log)} envelopes, {len(log_lines)} lines  (stream delivered {len(nums)})")
assert len(log_lines)==len(nums), "durable log and live stream disagree"

# The progress bar rewrote one line ~40 times. It must appear exactly once.
import re
bars=[e for e in log_lines if "fetching" in re.sub("<[^>]+>","",e["html"])]
print(f"progress-bar line appears {len(bars)} time(s) in the finalized log")
assert len(bars)==1, "carriage-return rewrites leaked into the finalized log"
assert "done" in re.sub("<[^>]+>","",bars[0]["html"]), "progress bar not in its final state"

# A subscriber arriving after the run ends gets the whole thing.
late=read_for(f"{BASE}/api/runs/{rid}/events", 30.0, until_end=True)
late_lines=[e for e in late if e["kind"]=="line"]
print(f"late subscriber received {len(late_lines)} lines from the durable log")
assert len(late_lines)==len(log_lines)

# Every subscription must state the viewport, even when it is empty.
#
# The live region is client-side state that only a window envelope can correct.
# A client that drops with rows on screen and returns after the run has drained
# them will keep rendering those stale rows forever unless the server says
# `len: 0`. Asserting only on finalized lines misses this completely: the lines
# arrive perfectly while the window below them is frozen mid-run.
for label, envs in (("resume mid-run", seg2), ("subscribe after the run ended", late)):
    wins=[e for e in envs if e["kind"]=="window"]
    assert wins, f"{label}: no window envelope, so the live region can never re-sync"
    print(f"{label}: viewport announced ({len(wins)} window envelope(s), "
          f"final len={wins[-1].get('len')})")

assert wins[-1].get("len")==0, \
    "a subscriber joining a finished run was not told to clear its live region"

# --- append-only mode ---------------------------------------------------------
#
# With the live window off there is no mutable region, so the run must produce
# no window envelopes at all — not empty ones — and every line must still be
# contiguous. The carriage-return bar still collapses: a one-row screen resolves
# a rewritten line to its final state before committing it, which is exactly
# what a CI viewer shows.
rid3=start("demo.sh", live=False)
flat=read_for(f"{BASE}/api/runs/{rid3}/events", 90.0, until_end=True)

meta=[e for e in flat if e["kind"]=="meta"]
assert meta and meta[0].get("live") is False, "meta did not advertise append-only mode"

# Every subscription still announces the viewport, so exactly one window is
# expected — the len-0 snapshot from hub.subscribe. What must never appear is a
# window describing rows, which would mean a mutable region exists after all.
windows=[e for e in flat if e["kind"]=="window"]
mutable=[e for e in windows if (e.get("len") or 0)>0 or e.get("patch")]
assert not mutable, f"append-only run exposed a mutable viewport: {mutable[:2]}"
assert len(windows)<=1, \
    f"append-only run published {len(windows)-1} window envelope(s) of its own"

flat_nums=[e["line"] for e in flat if e["kind"]=="line"]
assert flat_nums==list(range(1,len(flat_nums)+1)), \
    f"append-only line numbering is not contiguous: {flat_nums[:5]}..{flat_nums[-5:]}"

flat_bars=[e for e in flat if e["kind"]=="line" and "fetching" in re.sub("<[^>]+>","",e["html"])]
assert len(flat_bars)==1, \
    f"append-only run leaked {len(flat_bars)} progress-bar frames into the log"
print(f"append-only: {len(flat_nums)} lines, no mutable viewport, "
      f"bar collapsed to 1 record")

print("\nALL CHECKS PASSED")
