// Package harness scans and rewrites AI-harness MCP configs.
package harness

import (
	"io"
	"os"
	"path/filepath"
)

// Scan lists stdio MCP servers configured in supported harnesses.
func Scan(args []string, stdout, stderr io.Writer) int { return runScan(args, stdout, stderr) }

// Wrap routes stdio servers through mcp-snooze (or undoes it when undo is true).
func Wrap(args []string, stdout, stderr io.Writer, undo bool) int {
	return runWrap(args, stdout, stderr, undo)
}

// configPaths returns harness name -> config file path. Missing files are the caller's to skip.
func configPaths() map[string]string {
	home, _ := os.UserHomeDir()
	claude := filepath.Join(home, ".claude.json")
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		claude = filepath.Join(d, ".claude.json")
	}
	codex := filepath.Join(home, ".codex", "config.toml")
	if d := os.Getenv("CODEX_HOME"); d != "" {
		codex = filepath.Join(d, "config.toml")
	}
	return map[string]string{"claude": claude, "codex": codex}
}

// loadServers parses the config of one harness.
func loadServers(harness string, src []byte) ([]Server, error) {
	if harness == "claude" {
		return ClaudeServers(src)
	}
	return CodexServers(src)
}
