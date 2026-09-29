---
name: mcp-snooze
description: Use when the machine is slow or using high memory from many MCP server processes, when many Claude Code or Codex sessions are open, when the user asks to snooze or lazy-load MCP servers, or when undoing with unwrap.
---

# Snooze stdio MCP servers

Guide the user through scan, wrap, verification, and (if requested) unwrap. Only wrap after showing the dry-run plan.

## Install check

1. Run `command -v mcp-snooze`.
2. If missing, install with `go install github.com/askmegit/mcp-snooze@latest` or download a release binary.
3. Ensure the executable is named exactly `mcp-snooze`; `wrap` refuses a differently named binary.

## 1. Scan

Run `mcp-snooze scan` to inspect configured stdio MCP servers. Use `mcp-snooze scan --json` when structured output is useful.

Read the table as follows:

- `SNOOZED` is `yes` when the server is wrapped to start on demand; otherwise it is `no`.
- `PROCS` is the number of matching server processes currently running.
- `RSS` is their resident memory in MB.
- The summary reports the count of enabled, unsnoozed stdio servers and their combined process count and RSS. If none remain unsnoozed, it says all stdio servers are snoozed.

## 2. Preview and wrap

1. Run `mcp-snooze wrap --dry-run` first.
2. Show the user the proposed server changes and summary. Wrap only after the user approves the plan.
3. Run `mcp-snooze wrap` to apply it.

Optional flags:

- `--harness claude` or `--harness codex` limits the config to one harness; repeat the flag to select both.
- `--server NAME` selects a server by name; repeat it to select more than one.
- `--idle SECONDS` sets the proxy idle timeout. The default is 600 seconds.

App-bundled servers are skipped unless explicitly selected with `--server NAME`. HTTP and SSE servers are never touched. Wrap backs up changed configs under `~/.mcp-snooze/backups/<UTC timestamp>/`; directories use mode 0700 and files use mode 0600. The newest 10 backup runs are kept.

## 3. Verify

1. Restart the Claude Code or Codex session. Existing sessions keep their old server processes.
2. For Claude Code, run `claude mcp list` and confirm the servers show `Connected`.
3. Run `mcp-snooze scan`. Confirm `SNOOZED` is `yes` and `PROCS` drops to 0 until the first tool call starts the server.

## Undo

When the user asks to restore the original launch configuration, run `mcp-snooze unwrap`. It restores each server's original argv from the wrapped argv; it does not restore from backup. Backups are for disaster recovery.

## Caveats

- The first tool call after the idle period pays the server cold-start cost; JVM servers can take several seconds.
- Wrap re-encodes `.claude.json` with sorted keys.
- Do not wrap while unsure whether a config edit is in flight. Wrap aborts if the config file changes during its write.
