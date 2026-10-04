#!/usr/bin/env python3
"""Aggregate per-job request logs from the Funnel load test.

usage: aggregate.py DIR [--json OUT]

DIR holds one subdirectory per job, each with requests.jsonl, meta.json and
status.json. Prints a Markdown summary on stdout.
"""

import json
import math
import statistics
import sys
from datetime import datetime, timezone
from pathlib import Path


def ts(s):
    return datetime.fromisoformat(s.replace("Z", "+00:00")).timestamp()


def pct(values, p):
    if not values:
        return None
    v = sorted(values)
    return v[max(0, math.ceil(p / 100 * len(v)) - 1)]


def load(root):
    jobs = []
    for d in sorted(Path(root).iterdir()):
        if not d.is_dir():
            continue
        reqs_file = d / "requests.jsonl"
        reqs = [json.loads(l) for l in reqs_file.read_text().splitlines() if l.strip()] if reqs_file.exists() else []
        for r in reqs:
            r["t"] = ts(r["start"])
            r["job"] = d.name
            # gauger's 10 s http.Client timeout surfaces as "request canceled".
            if not r["status"] and r.get("err_kind") == "other" and r["latency_ms"] >= 9990:
                r["err_kind"] = "timeout"
        meta = json.loads((d / "meta.json").read_text()) if (d / "meta.json").exists() else {}
        status = json.loads((d / "status.json").read_text()) if (d / "status.json").exists() else {}
        jobs.append({"name": d.name, "reqs": reqs, "meta": meta, "status": status})
    return jobs


def summarize(jobs):
    reqs = [r for j in jobs for r in j["reqs"]]
    active = [j for j in jobs if j["reqs"]]
    meta = next((j["meta"] for j in jobs if j["meta"]), {})

    # Window in which every job was sending: latest first request to the
    # earliest last metrics request.
    firsts = [min(r["t"] for r in j["reqs"]) for j in active]
    lasts = [max(r["t"] for r in j["reqs"] if r["path"] == "/v1/metrics") for j in active
             if any(r["path"] == "/v1/metrics" for r in j["reqs"])]
    lo, hi = (max(firsts), min(lasts)) if firsts and lasts else (0, 0)
    window = max(hi - lo, 0)
    inwin = [r for r in reqs if lo <= r["t"] <= hi]

    answered = [r for r in reqs if r["status"]]
    errors = [r for r in reqs if not r["status"]]
    metrics_ok = [r for r in reqs if r["path"] == "/v1/metrics" and 200 <= r["status"] < 300]
    err_kinds = {}
    for r in errors:
        err_kinds[r["err_kind"]] = err_kinds.get(r["err_kind"], 0) + 1
    new_conns = [r for r in reqs if r["got_conn"] and not r["reused"]]
    flush = meta.get("flush_every", "")
    sample = meta.get("sample_every", "")
    batch_bytes = [r["bytes"] for r in reqs if r["path"] == "/v1/metrics"]
    return {
        "label": meta.get("label", ""),
        "date": datetime.fromtimestamp(min(r["t"] for r in reqs), timezone.utc).strftime("%Y-%m-%d %H:%M UTC") if reqs else "",
        "jobs": len(active),
        "jobs_done_sent": sum(1 for j in jobs if j["status"].get("done_sent")),
        "jobs_late_s_max": max((j["meta"].get("late_s", 0) for j in jobs), default=0),
        "flags": f"-flush-every {flush} -sample-every {sample}",
        "window_s": round(window, 1),
        "req_s": round(len(inwin) / window, 2) if window else None,
        "kb_s": round(sum(r["bytes"] for r in inwin) / 1000 / window, 1) if window else None,
        "kb_s_with_headers": round(sum(r["bytes"] + r["header_bytes"] for r in inwin) / 1000 / window, 1) if window else None,
        "requests": len(reqs),
        "requests_by_path": {p: sum(1 for r in reqs if r["path"] == p) for p in sorted({r["path"] for r in reqs})},
        "bytes_sent": sum(r["bytes"] for r in reqs),
        "header_bytes_sent": sum(r["header_bytes"] for r in reqs),
        "p50_ms": pct([r["latency_ms"] for r in answered], 50),
        "p99_ms": pct([r["latency_ms"] for r in answered], 99),
        "max_ms": max((r["latency_ms"] for r in answered), default=None),
        "metrics_2xx_p50_ms": pct([r["latency_ms"] for r in metrics_ok], 50),
        "metrics_2xx_p99_ms": pct([r["latency_ms"] for r in metrics_ok], 99),
        "status_2xx": sum(1 for r in answered if 200 <= r["status"] < 300),
        "status_429": sum(1 for r in answered if r["status"] == 429),
        "status_4xx_other": sum(1 for r in answered if 400 <= r["status"] < 500 and r["status"] != 429),
        "status_5xx": sum(1 for r in answered if r["status"] >= 500),
        "conn_errors": len(errors),
        "conn_errors_by_kind": err_kinds,
        "new_connections": len(new_conns),
        "reused_pct": round(100 * sum(1 for r in reqs if r["reused"]) / len(reqs), 1) if reqs else None,
        "tls_handshake_p50_ms": pct([r["tls_ms"] for r in new_conns if r.get("tls_ms")], 50),
        "protos": sorted({r["proto"] for r in answered if r.get("proto")}),
        "batch_bytes_median": statistics.median(batch_bytes) if batch_bytes else None,
        "batch_bytes_max": max(batch_bytes, default=None),
        "over_2s": sum(1 for r in answered if r["latency_ms"] > 2000),
        "write_p50_p99_ms": [pct([r["wrote_ms"] for r in answered if r.get("wrote_ms")], p) for p in (50, 99)],
        "wait_p50_p99_ms": [pct([r["ttfb_ms"] - r["wrote_ms"] for r in answered if r.get("wrote_ms") and r.get("ttfb_ms")], p) for p in (50, 99)],
        "by_remote": by_remote(answered),
        "unsent_batches": sum(j["status"].get("unsent_batches", 0) for j in jobs),
        "error_samples": [r["err"] for r in errors][:5],
    }


def by_remote(reqs):
    groups = {}
    for r in reqs:
        groups.setdefault(r.get("remote", "").rsplit(":", 1)[0] or "?", []).append(r)
    return {
        ip: {
            "jobs": len({r["job"] for r in rs}),
            "requests": len(rs),
            "p50_ms": pct([r["latency_ms"] for r in rs], 50),
            "p99_ms": pct([r["latency_ms"] for r in rs], 99),
            "over_2s": sum(1 for r in rs if r["latency_ms"] > 2000),
        }
        for ip, rs in sorted(groups.items())
    }


def markdown(s):
    rows = [
        ("Date", s["date"]),
        ("Jobs (sent done)", f'{s["jobs"]} ({s["jobs_done_sent"]}), latest start {s["jobs_late_s_max"]} s late'),
        ("Flags", f'`{s["flags"]}`'),
        ("Overlap window", f'{s["window_s"]} s'),
        ("req/s in window", s["req_s"]),
        ("KB/s in window (bodies / with request headers)", f'{s["kb_s"]} / {s["kb_s_with_headers"]}'),
        ("Requests", f'{s["requests"]} {s["requests_by_path"]}'),
        ("Bytes sent (bodies / headers)", f'{s["bytes_sent"]} / {s["header_bytes_sent"]}'),
        ("Latency p50 / p99 / max ms (all answered)", f'{s["p50_ms"]} / {s["p99_ms"]} / {s["max_ms"]}'),
        ("Latency p50 / p99 ms (/v1/metrics 2xx)", f'{s["metrics_2xx_p50_ms"]} / {s["metrics_2xx_p99_ms"]}'),
        ("2xx / 429 / other 4xx / 5xx", f'{s["status_2xx"]} / {s["status_429"]} / {s["status_4xx_other"]} / {s["status_5xx"]}'),
        ("Connection errors", f'{s["conn_errors"]} {s["conn_errors_by_kind"]}'),
        ("New connections / reused %", f'{s["new_connections"]} / {s["reused_pct"]}'),
        ("TLS handshake p50 ms", s["tls_handshake_p50_ms"]),
        ("Protocol", ", ".join(s["protos"])),
        ("Batch bytes median / max", f'{s["batch_bytes_median"]} / {s["batch_bytes_max"]}'),
        ("Requests over 2 s", s["over_2s"]),
        ("Upload (request written) p50 / p99 ms", " / ".join(map(str, s["write_p50_p99_ms"]))),
        ("Wait (written to first byte) p50 / p99 ms", " / ".join(map(str, s["wait_p50_p99_ms"]))),
        ("Unsent batches", s["unsent_batches"]),
    ]
    out = [f'### {s["label"]}', "", "| | |", "|---|---|"]
    out += [f"| {k} | {v} |" for k, v in rows]
    out += ["", "| Funnel ingress | jobs | requests | p50 ms | p99 ms | over 2 s |", "|---|---|---|---|---|---|"]
    out += [f'| {ip} | {g["jobs"]} | {g["requests"]} | {g["p50_ms"]} | {g["p99_ms"]} | {g["over_2s"]} |' for ip, g in s["by_remote"].items()]
    if s["error_samples"]:
        out += ["", "First errors:", ""] + [f"- `{e}`" for e in s["error_samples"]]
    return "\n".join(out) + "\n"


def main():
    s = summarize(load(sys.argv[1]))
    if "--json" in sys.argv:
        Path(sys.argv[sys.argv.index("--json") + 1]).write_text(json.dumps(s, indent=2))
    print(markdown(s))


if __name__ == "__main__":
    main()
