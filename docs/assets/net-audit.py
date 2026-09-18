#!/usr/bin/env python3
"""Summarise a hub HAR: where the bytes went, and what was wasted.

    python3 docs/assets/net-audit.py app.beardrive.ai.har [host]

Save the HAR from DevTools → Network → "Export HAR (with content)" while the
tab is doing the thing you want to measure. The numbers this prints are the
ones docs/network-efficiency-prd.md is measured against, so re-run it after
each stage rather than eyeballing the waterfall.

Columns worth staring at:
  uniq    distinct response bodies. `n` far above `uniq` means the client
          re-downloaded bytes it already held.
  gzip    what the body WOULD cost compressed. Compared against `xfer`, this
          is how you tell whether the response was compressed at all.
"""

from __future__ import annotations

import gzip
import hashlib
import json
import sys
from collections import defaultdict
from urllib.parse import urlparse


def endpoint(url: str) -> str:
    """Collapse per-project and per-path URLs into one comparable key."""
    p = urlparse(url).path
    if "/api/p/" in p:
        p = "/api/p/<id>/" + p.split("/api/p/")[-1].split("/", 1)[-1]
    return p


def main(path: str, host: str | None) -> None:
    har = json.load(open(path))
    entries = har["log"]["entries"]
    if host is None:  # the busiest host is the one under test
        counts: dict[str, int] = defaultdict(int)
        for e in entries:
            counts[urlparse(e["request"]["url"]).netloc] += 1
        host = max(counts, key=counts.get)
    entries = [e for e in entries if urlparse(e["request"]["url"]).netloc == host]
    if not entries:
        sys.exit(f"no entries for host {host}")

    agg: dict[str, dict] = defaultdict(
        lambda: {"n": 0, "xfer": 0, "body": 0, "gzip": 0, "bodies": set(), "status": defaultdict(int)}
    )
    for e in entries:
        key = e["request"]["method"] + " " + endpoint(e["request"]["url"])
        a = agg[key]
        a["n"] += 1
        a["xfer"] += max(e["response"].get("_transferSize") or 0, 0)
        content = e["response"]["content"]
        a["body"] += content.get("size") or 0
        a["status"][e["response"]["status"]] += 1
        text = content.get("text") or ""
        if text:
            digest = hashlib.sha1(text.encode(errors="ignore")).hexdigest()
            if digest not in a["bodies"]:
                # Compress one copy of each distinct body; repeats would only
                # inflate the estimate with bytes that should not be re-sent.
                a["gzip"] += len(gzip.compress(text.encode(errors="ignore"), 6))
            a["bodies"].add(digest)

    rows = sorted(agg.items(), key=lambda kv: -kv[1]["xfer"])
    print(f"host: {host}   window: {entries[0]['startedDateTime']} .. {entries[-1]['startedDateTime']}")
    print(f"{'endpoint':48} {'n':>4} {'xfer MB':>9} {'gzip MB':>9} {'uniq':>5}  statuses")
    for key, v in rows:
        if not v["n"]:
            continue
        print(
            f"{key[:48]:48} {v['n']:>4} {v['xfer'] / 1e6:>9.2f} {v['gzip'] / 1e6:>9.3f} "
            f"{len(v['bodies']):>5}  {dict(v['status'])}"
        )

    total_x = sum(v["xfer"] for v in agg.values())
    total_g = sum(v["gzip"] for v in agg.values())
    print(f"\n{len(entries)} requests, {total_x / 1e6:.2f} MB transferred")
    print(f"distinct bodies would be {total_g / 1e6:.2f} MB gzipped "
          f"({100 * (1 - total_g / total_x):.0f}% of the wire was avoidable)")

    # Anything fetched many times with few distinct bodies is a poll, a
    # too-wide invalidation, or both — which is the whole point of the audit.
    waste = [(k, v) for k, v in rows if v["n"] >= 3 and len(v["bodies"]) * 2 <= v["n"]]
    if waste:
        print("\nrepeated fetches of bytes the client already had:")
        for k, v in waste:
            print(f"  {k[:60]:60} {v['n']} fetches, {len(v['bodies'])} distinct, {v['xfer'] / 1e6:.2f} MB")


if __name__ == "__main__":
    if len(sys.argv) < 2:
        sys.exit(__doc__)
    main(sys.argv[1], sys.argv[2] if len(sys.argv) > 2 else None)
