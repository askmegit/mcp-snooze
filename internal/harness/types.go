package harness

import (
	"errors"
	"path/filepath"
	"strings"
)

// Server is one stdio MCP server entry found in a harness config.
type Server struct {
	Harness    string // "claude" or "codex"
	Scope      string // "user", or the project path for Claude project-scope servers
	Name       string
	Argv       []string // the real server command line (unwrapped form)
	Wrapped    bool     // routed through mcp-snooze
	ProxyFlags []string // flags between the mcp-snooze binary and "--" when wrapped
	Enabled    bool     // false for Codex entries with enabled = false
}

// ErrNotFound: the named server does not exist (or is not a stdio server) in the given scope.
var ErrNotFound = errors.New("server not found")

// ErrUnsupported: the entry exists but is written in a form we do not rewrite (e.g. TOML inline table).
var ErrUnsupported = errors.New("unsupported config form")

// WrappedArgv builds the command line that routes argv through mcp-snooze.
func WrappedArgv(bin string, flags, argv []string) []string {
	out := append([]string{bin}, flags...)
	out = append(out, "--")
	return append(out, argv...)
}

// SplitWrapped reports whether command line full (command + args) is an mcp-snooze wrapper.
// Detection is by binary basename, not absolute path, so a moved or upgraded binary still matches.
func SplitWrapped(full []string) (wrapped bool, flags, argv []string) {
	if len(full) == 0 {
		return false, nil, full
	}
	base := strings.TrimSuffix(filepath.Base(strings.ReplaceAll(full[0], `\`, "/")), ".exe")
	if base != "mcp-snooze" {
		return false, nil, full
	}
	for i, a := range full[1:] {
		if a == "--" {
			return true, full[1 : 1+i], full[2+i:]
		}
	}
	return false, nil, full
}
