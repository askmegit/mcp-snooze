package harness

import (
	"reflect"
	"testing"
)

func TestWrapSplitRoundTrip(t *testing.T) {
	argv := []string{"npx", "-y", "srv", "--", "x"}
	full := WrappedArgv("/opt/homebrew/bin/mcp-snooze", []string{"--idle", "900"}, argv)
	w, flags, got := SplitWrapped(full)
	if !w || !reflect.DeepEqual(flags, []string{"--idle", "900"}) || !reflect.DeepEqual(got, argv) {
		t.Fatalf("split(%q) = %v %q %q", full, w, flags, got)
	}
	for _, bin := range []string{`C:\Tools\mcp-snooze.exe`, "/old/Cellar/mcp-snooze/0.1/bin/mcp-snooze"} {
		if w, _, _ := SplitWrapped([]string{bin, "--", "a"}); !w {
			t.Fatalf("%s not detected as wrapped", bin)
		}
	}
	for _, full := range [][]string{{"mcp-snooze"}, {"node", "--", "x"}, {"mcp-snoozer", "--", "x"}, nil} {
		if w, _, _ := SplitWrapped(full); w {
			t.Fatalf("%q wrongly detected as wrapped", full)
		}
	}
}
