package harness

import (
	"fmt"
	"io"
)

func runWrap(args []string, stdout, stderr io.Writer, undo bool) int {
	fmt.Fprintln(stderr, "wrap: not implemented")
	return 2
}
