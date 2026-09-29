// Package proxy implements the lazy stdio MCP proxy.
package proxy

import (
	"fmt"
	"io"
)

// Main runs the proxy: mcp-snooze [flags] -- <command> [args...].
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fmt.Fprintln(stderr, "proxy: not implemented")
	return 2
}
