// Package harness scans and rewrites AI-harness MCP configs.
package harness

import (
	"fmt"
	"io"
)

// Scan lists stdio MCP servers configured in supported harnesses.
func Scan(args []string, stdout, stderr io.Writer) int {
	fmt.Fprintln(stderr, "scan: not implemented")
	return 2
}

// Wrap routes stdio servers through mcp-snooze (or undoes it when undo is true).
func Wrap(args []string, stdout, stderr io.Writer, undo bool) int {
	fmt.Fprintln(stderr, "wrap: not implemented")
	return 2
}
