// mcp-snooze: lazy-start proxy for stdio MCP servers.
package main

import (
	"fmt"
	"os"

	"github.com/askmegit/mcp-snooze/internal/harness"
	"github.com/askmegit/mcp-snooze/internal/proxy"
)

var version = "dev"

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "scan":
			return harness.Scan(args[1:], os.Stdout, os.Stderr)
		case "wrap":
			return harness.Wrap(args[1:], os.Stdout, os.Stderr, false)
		case "unwrap":
			return harness.Wrap(args[1:], os.Stdout, os.Stderr, true)
		case "version", "--version":
			fmt.Println(version)
			return 0
		}
	}
	return proxy.Main(args, os.Stdin, os.Stdout, os.Stderr)
}
