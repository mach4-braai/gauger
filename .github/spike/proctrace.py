#!/usr/bin/env python3
"""Poll /proc every 100 ms and write process births, reparents and exits as JSON lines."""

import json
import os
import signal
import sys
import time

INTERVAL = 0.1
TICKS = os.sysconf("SC_CLK_TCK")


def boot_time():
    with open("/proc/stat") as stat:
        for line in stat:
            if line.startswith("btime "):
                return int(line.split()[1])
    raise RuntimeError("no btime in /proc/stat")


def snapshot(btime):
    processes = {}
    for entry in os.listdir("/proc"):
        if not entry.isdigit():
            continue
        try:
            with open(f"/proc/{entry}/stat") as stat:
                raw = stat.read()
            with open(f"/proc/{entry}/cmdline", "rb") as cmdline:
                cmd = cmdline.read().replace(b"\0", b" ").decode(errors="replace").strip()
        except OSError:
            continue
        comm = raw[raw.index("(") + 1 : raw.rindex(")")]
        fields = raw[raw.rindex(")") + 2 :].split()
        processes[int(entry)] = {
            "ppid": int(fields[1]),
            "start": btime + int(fields[19]) / TICKS,
            "comm": comm,
            "cmd": cmd[:200],
        }
    return processes


def main():
    out = open(sys.argv[1], "a", buffering=1)
    running = True

    def stop(*_):
        nonlocal running
        running = False

    signal.signal(signal.SIGTERM, stop)
    btime = boot_time()
    known = {}
    while running:
        now = time.time()
        current = snapshot(btime)
        for pid, info in current.items():
            previous = known.get(pid)
            if previous is None or previous["start"] != info["start"]:
                out.write(json.dumps({"ev": "new", "pid": pid, "seen": now, **info}) + "\n")
            elif previous["ppid"] != info["ppid"]:
                out.write(json.dumps({"ev": "reparent", "pid": pid, "ppid": info["ppid"], "seen": now}) + "\n")
        for pid in known.keys() - current.keys():
            out.write(json.dumps({"ev": "gone", "pid": pid, "seen": now}) + "\n")
        known = current
        time.sleep(max(0.0, INTERVAL - (time.time() - now)))


if __name__ == "__main__":
    main()
