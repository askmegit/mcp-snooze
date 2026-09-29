#!/usr/bin/env python3
"""Compare the RSS of bare MCP servers with cached mcp-snooze proxies."""

import argparse
import json
import os
import select
import signal
import subprocess
import sys
import tempfile
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
FAKE_SERVER = r'''import json, os, signal, sys
mb, pid_dir = int(sys.argv[1]), sys.argv[2]
memory = bytearray(mb * 1024 * 1024)
for i in range(0, len(memory), 4096): memory[i] = 1
pid_file = os.path.join(pid_dir, f"{os.getpid()}.pid")
open(pid_file, "w").write(str(os.getpid()))
signal.signal(signal.SIGTERM, lambda *_: sys.exit(0))
try:
    for line in sys.stdin:
        try: request = json.loads(line)
        except json.JSONDecodeError: continue
        method = request.get("method")
        if method == "initialize":
            result = {"protocolVersion": request.get("params", {}).get("protocolVersion", "2024-11-05"), "capabilities": {"tools": {}}, "serverInfo": {"name": "bench-fake", "version": "1"}}
        elif method == "tools/list": result = {"tools": [{"name": "echo", "description": "benchmark tool", "inputSchema": {"type": "object", "properties": {}}}]}
        elif method == "tools/call": result = {"content": [{"type": "text", "text": "ok"}], "isError": False}
        else: continue
        print(json.dumps({"jsonrpc": "2.0", "id": request.get("id"), "result": result}), flush=True)
finally:
    try: os.unlink(pid_file)
    except FileNotFoundError: pass
'''

class Session:
    def __init__(self, process, log_file, label):
        self.process = process
        self.log_file = log_file
        self.label = label
        self.buffer = bytearray()


def launch(command, label, log_dir):
    log = open(log_dir / f"{label}.stderr", "w+", encoding="utf-8")
    process = subprocess.Popen(
        command,
        cwd=ROOT,
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=log,
        start_new_session=True,
        bufsize=0,
    )
    return Session(process, log, label)


def send_notification(session, method):
    message = {"jsonrpc": "2.0", "method": method}
    session.process.stdin.write((json.dumps(message) + "\n").encode())


def request(session, method, request_id, params=None):
    message = {"jsonrpc": "2.0", "id": request_id, "method": method}
    if params is not None:
        message["params"] = params
    session.process.stdin.write((json.dumps(message) + "\n").encode())
    deadline = time.monotonic() + 20
    while time.monotonic() < deadline:
        newline = session.buffer.find(b"\n")
        if newline >= 0:
            line = bytes(session.buffer[:newline])
            del session.buffer[: newline + 1]
            response = json.loads(line)
            if response.get("id") == request_id:
                if "error" in response:
                    raise RuntimeError(f"{session.label} {method} failed: {response['error']}")
                return response
            continue
        remaining = deadline - time.monotonic()
        ready, _, _ = select.select([session.process.stdout], [], [], remaining)
        if not ready:
            break
        chunk = os.read(session.process.stdout.fileno(), 4096)
        if not chunk:
            break
        session.buffer.extend(chunk)
    raise RuntimeError(f"{session.label} timed out waiting for {method}; {read_log(session)}")


def read_log(session):
    session.log_file.flush()
    session.log_file.seek(0)
    return session.log_file.read().strip()


def handshake(session):
    request(session, "initialize", f"{session.label}-init", {
        "protocolVersion": "2024-11-05",
        "capabilities": {},
        "clientInfo": {"name": "snooze-bench", "version": "1"},
    })
    send_notification(session, "notifications/initialized")
    request(session, "tools/list", f"{session.label}-list")
def process_snapshot():
    processes, children = {}, {}
    for line in subprocess.check_output(["ps", "-Ao", "pid=,ppid=,rss="], text=True).splitlines():
        if len(fields := line.split()) == 3:
            pid, parent, rss = map(int, fields)
            processes[pid] = rss
            children.setdefault(parent, []).append(pid)
    return processes, children

def measure(sessions, pid_dir):
    processes, children = process_snapshot()
    tree, pending = set(), [s.process.pid for s in sessions]
    while pending:
        pid = pending.pop()
        if pid in processes and pid not in tree:
            tree.add(pid)
            pending.extend(children.get(pid, ()))
    fake = {int(p.stem) for p in pid_dir.glob("*.pid") if p.stem.isdigit()}
    return len(tree), len(fake & processes.keys()), sum(processes[p] for p in tree) / 1024, fake

def remember_fake_pids(pid_dir, known):
    known.update(int(p.stem) for p in pid_dir.glob("*.pid") if p.stem.isdigit())

def close_session(session):
    if session.process.stdin and not session.process.stdin.closed: session.process.stdin.close()
    try: session.process.wait(timeout=2)
    except subprocess.TimeoutExpired: pass

def cleanup(sessions, known_fake_pids, pid_dir):
    remember_fake_pids(pid_dir, known_fake_pids)
    for session in sessions:
        if session.process.stdin and not session.process.stdin.closed:
            try: session.process.stdin.close()
            except BrokenPipeError: pass
    groups = {s.process.pid for s in sessions}
    for group in groups:
        try: os.killpg(group, signal.SIGTERM)
        except ProcessLookupError: pass
    deadline = time.monotonic() + 2
    while time.monotonic() < deadline and any(s.process.poll() is None for s in sessions): time.sleep(0.05)
    for group in groups:
        try: os.killpg(group, signal.SIGKILL)
        except ProcessLookupError: pass
    for session in sessions:
        try: session.process.wait(timeout=2)
        except subprocess.TimeoutExpired: pass
        session.log_file.close()
    processes, _ = process_snapshot()
    return sorted({s.process.pid for s in sessions if s.process.poll() is None} | (known_fake_pids & processes.keys()))




def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--bin", default="./mcp-snooze", help="mcp-snooze binary")
    parser.add_argument("--sessions", type=int, default=3)
    parser.add_argument("--mb", type=int, default=50)
    parser.add_argument("--idle", type=float, default=2)
    args = parser.parse_args()
    sessions, known, rows, error, live = [], set(), [], None, []
    with tempfile.TemporaryDirectory(prefix=".snooze-bench-", dir=ROOT) as temporary:
        temp = Path(temporary); pid_dir = temp / "pids"; pid_dir.mkdir(); logs = temp / "logs"; logs.mkdir()
        script = temp / "fake.py"; script.write_text(FAKE_SERVER, encoding="utf-8")
        fake = [sys.executable, str(script), str(args.mb), str(pid_dir)]
        binary = str((ROOT / args.bin).resolve()) if not Path(args.bin).is_absolute() else args.bin
        command = [binary, "--idle", str(args.idle), "--cache-dir", str(temp / "cache"), "--", *fake]
        try:
            bare = [launch(fake, f"bare-{i}", logs) for i in range(args.sessions)]; sessions.extend(bare)
            for s in bare: handshake(s)
            _, bare_count, bare_rss, pids = measure(bare, pid_dir); known.update(pids)
            rows.append(("bare, idle", bare_count, bare_rss))
            if bare_count != args.sessions: raise RuntimeError(f"bare scenario has {bare_count} fake servers; expected {args.sessions}")
            for s in bare: close_session(s)
            warm = launch(command, "warmup", logs); sessions.append(warm); handshake(warm); remember_fake_pids(pid_dir, known); close_session(warm)
            snoozed = [launch(command, f"snoozed-{i}", logs) for i in range(args.sessions)]; sessions.extend(snoozed)
            for s in snoozed: handshake(s)
            _, idle_count, idle_rss, pids = measure(snoozed, pid_dir); known.update(pids)
            rows.append(("snoozed, idle", idle_count, idle_rss))
            if idle_count: raise RuntimeError(f"snoozed idle has {idle_count} fake servers; expected 0")
            response = request(snoozed[0], "tools/call", "snoozed-call", {"name": "echo", "arguments": {"message": "benchmark"}})
            if not response.get("result", {}).get("content"): raise RuntimeError("tools/call returned no content")
            remember_fake_pids(pid_dir, known); _, call_count, call_rss, pids = measure(snoozed, pid_dir); known.update(pids)
            rows.append(("snoozed, after 1 call", call_count, call_rss))
            if call_count != 1: raise RuntimeError(f"after one call has {call_count} fake servers; expected 1")
            time.sleep(args.idle + 2); _, reap_count, reap_rss, pids = measure(snoozed, pid_dir); known.update(pids)
            rows.append(("snoozed, after idle reap", reap_count, reap_rss))
            if reap_count: raise RuntimeError(f"idle reap left {reap_count} fake servers alive; expected 0")
            print("| scenario | processes | RSS MiB |\n|---|---:|---:|")
            for name, count, rss in rows: print(f"| {name} | {count} | {rss:.1f} |")
            saving = (bare_rss - idle_rss) * 100 / bare_rss if bare_rss else 0
            print(f"saving: {saving:.1f}%")
            if idle_rss >= bare_rss * 0.5: raise RuntimeError(f"snoozed idle RSS {idle_rss:.1f} MiB is not below 50% of bare RSS {bare_rss:.1f} MiB")
        except Exception as exc: error = exc
        finally: live = cleanup(sessions, known, pid_dir)
    if error: print(f"benchmark failed: {error}", file=sys.stderr)
    if live: print(f"launched processes still alive after cleanup: {live}", file=sys.stderr)
    return 1 if error or live else 0

if __name__ == "__main__": sys.exit(main())
