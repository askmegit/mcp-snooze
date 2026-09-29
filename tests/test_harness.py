#!/usr/bin/env python3
"""Black-box tests for `mcp-snooze scan / wrap / unwrap` (Claude Code + Codex).

Run: go build -o mcp-snooze . && python3 tests/test_harness.py
     (MCP_SNOOZE_BIN overrides the binary path.)

Every case runs against a throwaway HOME holding a fixture ~/.claude.json and
~/.codex/config.toml, and asserts on the files the binary leaves behind.
"""
import json
import os
import subprocess
import sys
import tempfile
import tomllib

HERE = os.path.dirname(os.path.abspath(__file__))
BIN = os.path.realpath(os.environ.get("MCP_SNOOZE_BIN", os.path.join(HERE, "..", "mcp-snooze")))

CLAUDE = {
    "numStartups": 5,
    "mcpServers": {
        "maestro": {"type": "stdio", "command": "maestro", "args": ["mcp", "--no-viewer"], "env": {}},
        "appium": {"command": "npx", "args": ["-y", "appium-mcp@1"], "env": {"NO_UI": "true"}},
        "remote": {"type": "http", "url": "https://example.com/mcp"},
    },
    "projects": {
        "/tmp/p": {"allowedTools": [], "mcpServers": {"dart": {"type": "stdio", "command": "dart", "args": ["mcp-server"]}}},
    },
}

CODEX = '''model = "gpt-5"
# top comment stays
[mcp_servers.dart]
# dart comment stays
command = "dart"
args = ["mcp-server", "--force-roots-fallback"]
env = { FOO = "bar" }

[mcp_servers.remote]
url = "http://127.0.0.1:3721/mcp"

[profiles.fast]
model = "o4"
'''

STDIO = {("claude", "maestro"), ("claude", "appium"), ("claude", "dart"), ("codex", "dart")}


class Home:
    def __init__(self):
        self.dir = tempfile.mkdtemp(prefix="snooze-home-")
        self.claude = os.path.join(self.dir, ".claude.json")
        self.codex = os.path.join(self.dir, ".codex", "config.toml")
        os.makedirs(os.path.dirname(self.codex))
        with open(self.claude, "w") as f:
            json.dump(CLAUDE, f, indent=2)
        with open(self.codex, "w") as f:
            f.write(CODEX)

    def run(self, *args, ok=True):
        env = {k: v for k, v in os.environ.items() if k not in ("CODEX_HOME", "CLAUDE_CONFIG_DIR")}
        env["HOME"] = self.dir
        p = subprocess.run([BIN, *args], capture_output=True, text=True, env=env, timeout=30)
        if ok:
            assert p.returncode == 0, f"{args} exit {p.returncode}: {p.stderr}"
        return p

    def read(self):
        return open(self.claude, "rb").read(), open(self.codex, "rb").read()

    def scan(self):
        return json.loads(self.run("scan", "--json").stdout)


def split(server):
    """Return (wrapped?, original argv) for a server entry."""
    cmd, args = server["command"], server.get("args", [])
    if os.path.realpath(cmd) == BIN:
        assert "--" in args, f"wrapped entry without --: {args}"
        return True, args[args.index("--") + 1:]
    return False, [cmd, *args]


def main():
    fails = []

    def case(name, fn):
        try:
            fn()
            print(f"ok   {name}")
        except AssertionError as e:
            fails.append(name)
            print(f"FAIL {name}: {e}")

    def scan_lists_stdio_only():
        h = Home()
        rows = h.scan()
        got = {(r["harness"], r["name"]) for r in rows}
        assert got == STDIO, f"scan found {sorted(got)}"
        assert all(r["wrapped"] is False for r in rows), rows
        dart = [r for r in rows if r["harness"] == "claude" and r["name"] == "dart"][0]
        assert dart["scope"] == "/tmp/p", dart
    case("scan lists stdio servers of both harnesses", scan_lists_stdio_only)

    def dry_run():
        h = Home()
        before = h.read()
        h.run("wrap", "--dry-run")
        assert h.read() == before, "--dry-run modified files"
    case("wrap --dry-run writes nothing", dry_run)

    def wrap_unwrap():
        h = Home()
        before = h.read()
        h.run("wrap")
        c = json.load(open(h.claude))
        assert c["numStartups"] == 5 and c["projects"]["/tmp/p"]["allowedTools"] == [], "unrelated keys lost"
        assert c["mcpServers"]["remote"] == CLAUDE["mcpServers"]["remote"], "http server touched"
        for name, spec in [("maestro", CLAUDE["mcpServers"]["maestro"]), ("appium", CLAUDE["mcpServers"]["appium"])]:
            w, argv = split(c["mcpServers"][name])
            assert w and argv == [spec["command"], *spec["args"]], (name, c["mcpServers"][name])
            assert c["mcpServers"][name].get("env") == spec["env"], f"{name} env changed"
        w, argv = split(c["projects"]["/tmp/p"]["mcpServers"]["dart"])
        assert w and argv == ["dart", "mcp-server"], "project-scope server not wrapped"

        text = open(h.codex).read()
        assert "# top comment stays" in text and "# dart comment stays" in text, "TOML comments lost"
        t = tomllib.loads(text)
        w, argv = split(t["mcp_servers"]["dart"])
        assert w and argv == ["dart", "mcp-server", "--force-roots-fallback"], t["mcp_servers"]["dart"]
        assert t["mcp_servers"]["dart"]["env"] == {"FOO": "bar"}, "codex env changed"
        assert t["mcp_servers"]["remote"] == {"url": "http://127.0.0.1:3721/mcp"}, "codex http server touched"
        assert t["profiles"] == {"fast": {"model": "o4"}} and t["model"] == "gpt-5", "unrelated TOML changed"

        rows = h.scan()
        assert all(r["wrapped"] for r in rows), f"scan after wrap: {rows}"
        backups = os.path.join(h.dir, ".mcp-snooze", "backups")
        assert os.path.isdir(backups) and any(files for _, _, files in os.walk(backups)), "no backup written"

        once = h.read()
        h.run("wrap")
        assert h.read() == once, "second wrap is not a no-op"

        h.run("unwrap")
        cb, tb = h.read()
        assert tb == before[1], "codex config not restored byte-for-byte"
        assert json.loads(cb) == json.loads(before[0]), "claude config not restored"
    case("wrap / idempotent / unwrap round trip", wrap_unwrap)

    def filters():
        h = Home()
        h.run("wrap", "--harness", "claude", "--server", "maestro")
        c = json.load(open(h.claude))
        assert split(c["mcpServers"]["maestro"])[0], "maestro not wrapped"
        assert not split(c["mcpServers"]["appium"])[0], "--server filter ignored"
        assert not split(c["projects"]["/tmp/p"]["mcpServers"]["dart"])[0], "--server filter ignored (project)"
        assert open(h.codex).read() == CODEX, "--harness filter ignored"
    case("--harness / --server filters", filters)

    def idle_flag():
        h = Home()
        h.run("wrap", "--idle", "1800", "--server", "appium")
        args = json.load(open(h.claude))["mcpServers"]["appium"]["args"]
        head = args[:args.index("--")]
        assert "1800" in head and any(a.startswith("--idle") for a in head), f"--idle not passed: {args}"
    case("wrap --idle is forwarded to the proxy", idle_flag)

    def missing_files():
        h = Home()
        os.remove(h.codex)
        rows = h.scan()
        assert {r["harness"] for r in rows} == {"claude"}, rows
        h.run("wrap")
        assert not os.path.exists(h.codex), "wrap created a codex config"
    case("absent harness config is skipped", missing_files)

    print(f"\n{'FAILED ' + str(len(fails)) if fails else 'all passed'}")
    return 1 if fails else 0


if __name__ == "__main__":
    sys.exit(main())
