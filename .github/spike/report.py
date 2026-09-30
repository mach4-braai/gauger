#!/usr/bin/env python3
"""Match Runner.Worker's child processes to the job's steps and print a Markdown report.

Usage: report.py TRACE.jsonl JOB.json SHORT_LIVED_COUNT
"""

import json
import sys
from datetime import datetime

SLACK = 1.0


def parse_time(value):
    return datetime.fromisoformat(value.replace("Z", "+00:00")).timestamp() if value else None


def load(path):
    processes = {}
    with open(path) as trace:
        for line in trace:
            event = json.loads(line)
            pid = event["pid"]
            if event["ev"] == "new":
                processes[pid] = {**event, "parents": [event["ppid"]], "gone": None}
            elif pid in processes and event["ev"] == "reparent":
                processes[pid]["parents"].append(event["ppid"])
            elif pid in processes and event["ev"] == "gone":
                processes[pid]["gone"] = event["seen"]
    return processes


def descends(processes, pid, ancestor):
    seen = set()
    while pid in processes and pid not in seen:
        seen.add(pid)
        parent = processes[pid]["parents"][0]
        if parent == ancestor:
            return True
        pid = parent
    return False


def step_at(steps, moment):
    for step in steps:
        start = parse_time(step.get("started_at"))
        end = parse_time(step.get("completed_at")) or float("inf")
        if start is not None and start - SLACK <= moment <= end + SLACK:
            return step
    return None


def fmt(moment):
    return datetime.fromtimestamp(moment).strftime("%H:%M:%S.%f")[:-3] if moment else "-"


def main():
    processes = load(sys.argv[1])
    with open(sys.argv[2]) as job_file:
        steps = json.load(job_file)["steps"]
    short_lived = int(sys.argv[3])

    workers = [pid for pid, info in processes.items() if info["comm"] == "Runner.Worker"]
    if not workers:
        print("No `Runner.Worker` process was seen.")
        return
    worker = workers[0]

    print("### Runner.Worker children matched to steps\n")
    print("| pid | started | exited | command | step by time window |")
    print("|---|---|---|---|---|")
    for pid, info in sorted(processes.items(), key=lambda item: item[1]["start"]):
        if info["parents"][0] != worker:
            continue
        step = step_at(steps, info["start"])
        label = f"{step['number']}. {step['name']}" if step else "none"
        print(f"| {pid} | {fmt(info['start'])} | {fmt(info['gone'])} | `{info['cmd'][:80]}` | {label} |")

    print("\n### Steps from the REST API\n")
    for step in steps:
        print(f"- {step['number']}. {step['name']}: {step.get('started_at')} to {step.get('completed_at')}")

    descendants = [pid for pid in processes if descends(processes, pid, worker)]
    seen_true = sum(1 for pid in descendants if processes[pid]["comm"] == "true")
    print("\n### Coverage\n")
    print(f"- Processes seen under Runner.Worker: {len(descendants)}")
    print(f"- Sub-second `/bin/true` runs seen at 100 ms polling: {seen_true} of {short_lived}")

    for marker, label in (
        ("gauger-spike-background", "Background process"),
        ("gauger-spike-container", "Container process"),
    ):
        matches = [pid for pid, info in processes.items() if marker in info["cmd"]]
        for pid in matches:
            info = processes[pid]
            chain = " -> ".join(str(parent) for parent in info["parents"])
            parent_names = ", ".join(processes.get(parent, {}).get("comm", "?") for parent in info["parents"])
            start_step = step_at(steps, info["start"])
            end_step = step_at(steps, info["gone"]) if info["gone"] else None
            print(
                f"- {label} {pid} `{info['cmd'][:60]}`: parents {chain} ({parent_names}), "
                f"under Runner.Worker = {descends(processes, pid, worker)}, "
                f"started in {start_step['name'] if start_step else 'none'}, "
                f"exited in {end_step['name'] if end_step else 'unknown'}"
            )


if __name__ == "__main__":
    main()
