#!/usr/bin/env python3
"""Black-box protocol tests for the mcp-snooze proxy.

Run: go build -o mcp-snooze . && python3 tests/test_proxy.py
     (MCP_SNOOZE_BIN overrides the binary path.)

A fake stdio MCP server appends its pid to spawn.log on every start and logs each
method it receives to recv.log; the tests drive the proxy from the client side and
assert only on protocol output and process facts.
"""
import json
import os
import queue
import subprocess
import sys
import tempfile
import threading
import time

HERE = os.path.dirname(os.path.abspath(__file__))
BIN = os.environ.get("MCP_SNOOZE_BIN", os.path.join(HERE, "..", "mcp-snooze"))
IDLE = 1.0

FAKE = r'''
import json, os, sys, time
d = os.environ["FAKE_DIR"]
with open(os.path.join(d, "spawn.log"), "a") as f:
    f.write(f"{os.getpid()}\n")
def send(o):
    sys.stdout.write(json.dumps(o) + "\n"); sys.stdout.flush()
for line in sys.stdin:
    m = json.loads(line)
    with open(os.path.join(d, "recv.log"), "a") as f:
        f.write(f"{os.getpid()} {m.get('method', 'RESPONSE')}\n")
    if "id" not in m or "method" not in m:
        continue
    meth, i = m["method"], m["id"]
    if meth == "initialize":
        if os.environ.get("FAKE_HANG_INIT"):
            continue
        send({"jsonrpc": "2.0", "id": i, "result": {
            "protocolVersion": m["params"]["protocolVersion"],
            "capabilities": {"tools": {}},
            "serverInfo": {"name": "fake", "version": "9.9.9"}}})
    elif meth == "tools/list":
        time.sleep(float(os.environ.get("FAKE_SLOW_LIST", "0")))
        if os.environ.get("FAKE_FAIL_LIST"):
            send({"jsonrpc": "2.0", "id": i, "error": {"code": -32000, "message": "not ready"}})
            continue
        tools = [{"name": "echo", "inputSchema": {"type": "object"}},
                 {"name": "slow", "inputSchema": {"type": "object"}}]
        if os.environ.get("FAKE_EXTRA_TOOL"):
            tools.append({"name": "extra", "inputSchema": {"type": "object"}})
        send({"jsonrpc": "2.0", "id": i, "result": {"tools": tools}})
    elif meth == "tools/call":
        if m["params"]["name"] == "hang":
            continue
        if m["params"]["name"] == "slow":
            time.sleep(float(m["params"]["arguments"]["s"]))
        send({"jsonrpc": "2.0", "id": i, "result": {"content": [
            {"type": "text", "text": f"pid={os.getpid()} args={json.dumps(m['params'].get('arguments', {}))}"}]}})
    else:
        send({"jsonrpc": "2.0", "id": i, "error": {"code": -32601, "message": "nope"}})
'''


class Client:
    def __init__(self, work, cache, cwd=None, env=None, args=(), server=None):
        self.work = work
        self.notes = []
        env = dict(os.environ, FAKE_DIR=work, **(env or {}))
        self.p = subprocess.Popen(
            [BIN, "--idle", str(IDLE), "--cache-dir", cache, *args, "--",
             *(server or [sys.executable, os.path.join(work, "fake.py")])],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            text=True, bufsize=1, env=env, cwd=cwd or work)
        self.q = queue.Queue()
        threading.Thread(target=self._pump, daemon=True).start()

    def _pump(self):
        for line in self.p.stdout:
            if line.strip():
                self.q.put(json.loads(line))
        self.q.put(None)

    def send(self, o):
        self.p.stdin.write(json.dumps(o) + "\n")
        self.p.stdin.flush()

    def call(self, i, method, params=None, timeout=15):
        self.send({"jsonrpc": "2.0", "id": i, "method": method, "params": params or {}})
        end = time.time() + timeout
        while time.time() < end:
            try:
                m = self.q.get(timeout=end - time.time())
            except queue.Empty:
                break
            if m is None:
                raise AssertionError(f"shim closed stdout while waiting for {method}")
            if m.get("id") == i and "method" not in m:
                return m
            if "method" in m and "id" not in m:
                self.notes.append(m["method"])
        raise AssertionError(f"no response to {method} id={i!r} in {timeout}s")

    def handshake(self):
        r = self.call(0, "initialize", {"protocolVersion": "2025-06-18", "capabilities": {},
                                        "clientInfo": {"name": "t", "version": "1"}})
        self.send({"jsonrpc": "2.0", "method": "notifications/initialized"})
        return r

    def close(self):
        try:
            self.p.stdin.close()
        except Exception:
            pass
        try:
            self.p.wait(5)
        except subprocess.TimeoutExpired:
            self.p.kill()
            raise AssertionError("shim did not exit within 5s of stdin EOF")


def spawns(work):
    f = os.path.join(work, "spawn.log")
    return [int(x) for x in open(f).read().split()] if os.path.exists(f) else []


def alive(pid):
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    # 已退出但未回收的僵尸也算死
    st = subprocess.run(["ps", "-o", "stat=", "-p", str(pid)], capture_output=True, text=True).stdout
    return bool(st.strip()) and not st.strip().startswith("Z")


def wait_dead(pid, t):
    end = time.time() + t
    while time.time() < end:
        if not alive(pid):
            return True
        time.sleep(0.1)
    return False


def recv_methods(work, pid):
    f = os.path.join(work, "recv.log")
    return [l.split(" ", 1)[1] for l in open(f).read().splitlines() if l.startswith(f"{pid} ")]


def main():
    work = tempfile.mkdtemp(prefix="mcp-lazy-")
    cache = os.path.join(work, "cache")
    open(os.path.join(work, "fake.py"), "w").write(FAKE)
    fails = []

    def case(name, fn):
        try:
            fn()
            print(f"ok   {name}")
        except AssertionError as e:
            fails.append(name)
            print(f"FAIL {name}: {e}")

    # 1 冷启动：无缓存时拉起一次真服务端填缓存，握手与 tools/list 返回真实内容，空闲后子进程被杀
    def cold():
        c = Client(work, cache)
        try:
            r = c.handshake()
            assert r["result"]["serverInfo"] == {"name": "fake", "version": "9.9.9"}, r
            t = c.call(1, "tools/list")
            assert [x["name"] for x in t["result"]["tools"]] == ["echo", "slow"], t
            s = spawns(work)
            assert len(s) == 1, f"cold start should spawn exactly once, got {s}"
            time.sleep(0.3)
            n = recv_methods(work, s[0]).count("notifications/initialized")
            assert n == 1, f"child got notifications/initialized {n} times"
            assert wait_dead(s[0], IDLE + 4), "child still alive after idle timeout"
        finally:
            c.close()
    case("cold start fills cache, child reaped after idle", cold)

    # 2 热启动：有缓存时 initialize / tools/list / ping 都不拉起子进程
    def warm():
        before = len(spawns(work))
        c = Client(work, cache)
        try:
            r = c.handshake()
            assert r["result"]["serverInfo"]["name"] == "fake", r
            t = c.call("L", "tools/list")
            assert len(t["result"]["tools"]) == 2, t
            p = c.call(7, "ping")
            assert p.get("result") == {}, p
            time.sleep(0.5)
            assert len(spawns(work)) == before, f"warm start spawned: {spawns(work)}"

            # 3 首次 tools/call 才拉起；客户端 id（数字和字符串）原样回来；子进程先收到 initialize
            r = c.call(42, "tools/call", {"name": "echo", "arguments": {"x": 1}})
            assert "result" in r and '"x": 1' in r["result"]["content"][0]["text"], r
            s = spawns(work)
            assert len(s) == before + 1, f"first tools/call should spawn once: {s}"
            got = recv_methods(work, s[-1])
            assert got[:2] == ["initialize", "notifications/initialized"], got
            assert "tools/call" in got, got
            r = c.call("abc", "tools/call", {"name": "echo", "arguments": {}})
            assert r["id"] == "abc" and "result" in r, r
            assert len(spawns(work)) == before + 1, "second call must reuse the live child"

            # 4 空闲被杀后，下次调用透明重拉
            assert wait_dead(s[-1], IDLE + 4), "child not reaped after idle"
            r = c.call(43, "tools/call", {"name": "echo", "arguments": {"y": 2}})
            assert "result" in r, r
            assert len(spawns(work)) == before + 2, f"respawn expected: {spawns(work)}"

            # 5 进行中的调用不算空闲：比 idle 长的调用照样拿到结果
            live = spawns(work)[-1]
            r = c.call(44, "tools/call", {"name": "slow", "arguments": {"s": IDLE * 2.5}})
            assert "result" in r and f"pid={live}" in r["result"]["content"][0]["text"], r
        finally:
            c.close()
        # 6 stdin EOF：shim 退出，子进程一并退出
        assert wait_dead(spawns(work)[-1], 3), "child survived shim exit"
    case("warm start lazy, id passthrough, idle respawn, busy not reaped, EOF cleanup", warm)

    # 7 缓存按工作目录区分（firebase 的工具集随 cwd 变）
    def per_cwd():
        other = os.path.join(work, "other")
        os.makedirs(other, exist_ok=True)
        before = len(spawns(work))
        c = Client(work, cache, cwd=other)
        try:
            c.handshake()
            c.call(1, "tools/list")
        finally:
            c.close()
        assert len(spawns(work)) == before + 1, "new cwd should fill its own cache"
        n = len([f for f in os.listdir(cache) if not f.startswith(".")])
        assert n == 2, f"expected 2 cache entries, got {os.listdir(cache)}"
    case("cache keyed by cwd", per_cwd)

    # 8 取消的请求不能永远占着 in-flight：Node 版 SDK 对取消的请求不回响应
    def cancelled():
        c = Client(work, cache)
        try:
            c.handshake()
            c.send({"jsonrpc": "2.0", "id": 90, "method": "tools/call", "params": {"name": "hang", "arguments": {}}})
            end = time.time() + 5
            while time.time() < end and len(spawns(work)) == 0:
                time.sleep(0.1)
            time.sleep(0.5)
            pid = spawns(work)[-1]
            c.send({"jsonrpc": "2.0", "method": "notifications/cancelled", "params": {"requestId": 90}})
            assert wait_dead(pid, IDLE + 4), "child kept alive by a cancelled request"
        finally:
            c.close()
    case("cancelled request does not block idle reap", cancelled)

    # 9 错误响应不进缓存：服务端没就绪时的 tools/list 错误不能毒化之后的会话
    def no_error_cache():
        cache2 = os.path.join(work, "cache-err")
        c = Client(work, cache2, env={"FAKE_FAIL_LIST": "1"})
        try:
            c.handshake()
            c.call(1, "tools/list")
        finally:
            c.close()
        c = Client(work, cache2)
        try:
            c.handshake()
            t = c.call(1, "tools/list")
            assert "result" in t and len(t["result"]["tools"]) == 2, f"error response was cached: {t}"
        finally:
            c.close()
    case("error responses are not cached", no_error_cache)

    # 10 服务端卡在 initialize：调用在 --start-timeout 内返回错误，EOF 后照常退出
    def hung_init():
        c = Client(work, cache, env={"FAKE_HANG_INIT": "1"}, args=("--start-timeout", "2"))
        try:
            c.handshake()  # 命中缓存，不拉起
            t0 = time.time()
            r = c.call(91, "tools/call", {"name": "echo", "arguments": {}}, timeout=10)
            assert "error" in r, r
            assert time.time() - t0 < 8, "hung child blocked the proxy past --start-timeout"
        finally:
            c.close()
    case("hung initialize times out with error", hung_init)

    # 11 拉起后发现工具列表变了（服务升级）：刷新缓存并通知客户端
    def refresh():
        c = Client(work, cache, env={"FAKE_EXTRA_TOOL": "1"})
        try:
            c.handshake()
            c.call(92, "tools/call", {"name": "echo", "arguments": {}})
            time.sleep(1)
            t = c.call(93, "tools/list")
            names = [x["name"] for x in t["result"]["tools"]]
            assert "extra" in names, f"tools/list not refreshed after spawn: {names}"
            assert "notifications/tools/list_changed" in c.notes, f"no list_changed sent: {c.notes}"
        finally:
            c.close()
        c = Client(work, cache, env={"FAKE_EXTRA_TOOL": "1"})
        try:
            before = len(spawns(work))
            c.handshake()
            names = [x["name"] for x in c.call(1, "tools/list")["result"]["tools"]]
            assert "extra" in names and len(spawns(work)) == before, "refreshed list not persisted to cache"
        finally:
            c.close()
    case("tool list refreshed after spawn", refresh)

    # 12 代理被 SIGTERM：子进程一并退出
    def sigterm():
        c = Client(work, cache)
        try:
            c.handshake()
            c.call(94, "tools/call", {"name": "echo", "arguments": {}})
            pid = spawns(work)[-1]
            c.p.terminate()
            assert wait_dead(pid, 6), "child orphaned after proxy SIGTERM"
        finally:
            c.p.kill()
    case("SIGTERM cleans up child", sigterm)

    # 13 launcher wrappers (npx/uvx/sh) keep the real server as a grandchild: reap the whole tree
    def grandchild():
        c = Client(work, cache, server=["/bin/sh", "-c", f'"{sys.executable}" "{os.path.join(work, "fake.py")}"; true'])
        try:
            c.handshake()
            r = c.call(95, "tools/call", {"name": "echo", "arguments": {}})
            assert "result" in r, r
            pid = spawns(work)[-1]
            assert wait_dead(pid, IDLE + 4), "grandchild server survived idle reap"
        finally:
            c.close()
    case("idle reap kills launcher grandchild", grandchild)

    # 14 a descendant that left the process group (setsid daemon) but still holds stdout must not hang shutdown
    def setsid_descendant():
        fake = os.path.join(work, "fake.py")
        daemon = f'"{sys.executable}" -c "import os,time; os.setsid(); time.sleep(30)" &'
        c = Client(work, cache, server=["/bin/sh", "-c", f'{daemon} exec "{sys.executable}" "{fake}"'])
        c.handshake()
        r = c.call(96, "tools/call", {"name": "echo", "arguments": {}})
        assert "result" in r, r
        t0 = time.time()
        c.close()  # asserts exit within 5s of stdin EOF
        assert time.time() - t0 < 5, "shutdown waited for the setsid descendant"
    case("setsid descendant does not hang shutdown", setsid_descendant)

    # 15 --idle shorter than the cold start must not kill the child mid-initialize
    def idle_shorter_than_cold_start():
        cache3 = os.path.join(work, "cache-slow")
        c = Client(work, cache3, env={"FAKE_SLOW_LIST": "2"}, args=("--idle", "0.3"))
        try:
            r = c.handshake()
            assert "result" in r, f"cold initialize failed: {r}"
        finally:
            c.close()
    case("idle reap does not interrupt cold start", idle_shorter_than_cold_start)

    print(f"\n{'FAILED ' + str(len(fails)) if fails else 'all passed'}")
    return 1 if fails else 0


if __name__ == "__main__":
    sys.exit(main())
