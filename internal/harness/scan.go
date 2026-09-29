package harness

import (
	"fmt"
	"io"
)

func runScan(args []string, stdout, stderr io.Writer) int {
	fmt.Fprintln(stderr, "scan: not implemented")
	return 2
}
