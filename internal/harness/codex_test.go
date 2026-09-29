package harness

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

const codexFixture = `model = "gpt-5"
# top comment
[mcp_servers.dart]
# dart comment
command = "dart" # trailing comment
args = [
  "mcp-server",
  "--force-roots-fallback",
]
env = { FOO = "bar" }

[mcp_servers."a.b"]
command = 'C:\tools\srv.exe'
args = ['--port', "8080"]

[mcp_servers.off]
command = "off-cmd"
enabled = false

[mcp_servers.remote]
url = "http://127.0.0.1:3721/mcp"

[mcp_servers.plain]
command = "node"

[mcp_servers.plain.env]
KEY = "value"

[mcp_servers.inline]
command = "x"
args = ["a"]

[other.mcp_servers.fake]
command = "nope"

[profiles.fast]
model = "o4"
`

func servers(t *testing.T, src string) map[string]Server {
	t.Helper()
	list, err := CodexServers([]byte(src))
	if err != nil {
		t.Fatalf("CodexServers: %v", err)
	}
	m := map[string]Server{}
	for _, s := range list {
		if s.Harness != "codex" || s.Scope != "user" {
			t.Fatalf("bad harness/scope: %+v", s)
		}
		m[s.Name] = s
	}
	return m
}

func TestCodexServers(t *testing.T) {
	m := servers(t, codexFixture)
	want := map[string][]string{
		"dart":   {"dart", "mcp-server", "--force-roots-fallback"},
		"a.b":    {`C:\tools\srv.exe`, "--port", "8080"},
		"off":    {"off-cmd"},
		"plain":  {"node"},
		"inline": {"x", "a"},
	}
	if len(m) != len(want) {
		t.Fatalf("got servers %v, want %v", keys(m), want)
	}
	for name, argv := range want {
		if s, ok := m[name]; !ok || !reflect.DeepEqual(s.Argv, argv) || s.Wrapped {
			t.Errorf("%s: got %+v want argv %q", name, s, argv)
		}
	}
	if m["off"].Enabled || !m["dart"].Enabled {
		t.Errorf("enabled flags wrong: off=%v dart=%v", m["off"].Enabled, m["dart"].Enabled)
	}
}

func keys(m map[string]Server) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// linesOutside returns the lines of src that are not inside [mcp_servers.<name>] (header excluded).
func linesOutside(src, header string) string {
	var out []string
	in := false
	for _, l := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "[") {
			in = strings.TrimSpace(l) == header
		}
		if !in {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

func TestCodexSetArgvWrapKeepsEverythingElse(t *testing.T) {
	wrapped := WrappedArgv("/usr/local/bin/mcp-snooze", []string{"--idle", "600"}, []string{"dart", "mcp-server", "--force-roots-fallback"})
	out, err := CodexSetArgv([]byte(codexFixture), "dart", wrapped)
	if err != nil {
		t.Fatal(err)
	}
	s := servers(t, string(out))["dart"]
	if !s.Wrapped || !reflect.DeepEqual(s.ProxyFlags, []string{"--idle", "600"}) ||
		!reflect.DeepEqual(s.Argv, []string{"dart", "mcp-server", "--force-roots-fallback"}) {
		t.Fatalf("after wrap: %+v", s)
	}
	if linesOutside(string(out), "[mcp_servers.dart]") != linesOutside(codexFixture, "[mcp_servers.dart]") {
		t.Fatal("bytes outside [mcp_servers.dart] changed")
	}
	for _, keep := range []string{"# dart comment", `env = { FOO = "bar" }`} {
		if !strings.Contains(string(out), keep) {
			t.Fatalf("lost %q inside the edited table:\n%s", keep, out)
		}
	}
}

func TestCodexCanonicalRoundTripIsByteExact(t *testing.T) {
	src := "[mcp_servers.dart]\ncommand = \"dart\"\nargs = [\"mcp-server\", \"--x\"]\nenv = { A = \"1\" }\n"
	w, err := CodexSetArgv([]byte(src), "dart", WrappedArgv("/b/mcp-snooze", nil, []string{"dart", "mcp-server", "--x"}))
	if err != nil {
		t.Fatal(err)
	}
	back, err := CodexSetArgv(w, "dart", []string{"dart", "mcp-server", "--x"})
	if err != nil {
		t.Fatal(err)
	}
	if string(back) != src {
		t.Fatalf("round trip changed bytes:\n%q\n%q", src, back)
	}
}

func TestCodexMissingArgsInsertedThenRemoved(t *testing.T) {
	w, err := CodexSetArgv([]byte(codexFixture), "plain", WrappedArgv("/b/mcp-snooze", nil, []string{"node"}))
	if err != nil {
		t.Fatal(err)
	}
	s := servers(t, string(w))["plain"]
	if !s.Wrapped || !reflect.DeepEqual(s.Argv, []string{"node"}) {
		t.Fatalf("plain after wrap: %+v", s)
	}
	if !strings.Contains(string(w), "[mcp_servers.plain.env]\nKEY = \"value\"") {
		t.Fatal("env subtable damaged")
	}
	back, err := CodexSetArgv(w, "plain", []string{"node"})
	if err != nil {
		t.Fatal(err)
	}
	if string(back) != codexFixture {
		t.Fatalf("unwrap did not restore original bytes:\n%s", back)
	}
}

func TestCodexWindowsPathAndQuotedName(t *testing.T) {
	argv := []string{`C:\Program Files\snooze\mcp-snooze.exe`, "--", `C:\tools\srv.exe`, `quote"arg`}
	out, err := CodexSetArgv([]byte(codexFixture), "a.b", argv)
	if err != nil {
		t.Fatal(err)
	}
	s := servers(t, string(out))["a.b"]
	if !s.Wrapped || !reflect.DeepEqual(s.Argv, []string{`C:\tools\srv.exe`, `quote"arg`}) {
		t.Fatalf("a.b after wrap: %+v\n%s", s, out)
	}
}

func TestCodexCRLF(t *testing.T) {
	src := strings.ReplaceAll(codexFixture, "\n", "\r\n")
	out, err := CodexSetArgv([]byte(src), "dart", []string{"mcp-snooze", "--", "dart"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(out), "\n") != strings.Count(string(out), "\r\n") {
		t.Fatal("bare LF introduced into CRLF file")
	}
	if s := servers(t, string(out))["dart"]; !s.Wrapped || !reflect.DeepEqual(s.Argv, []string{"dart"}) {
		t.Fatalf("CRLF parse after edit: %+v", s)
	}
}

func TestCodexErrorsLeaveInputAlone(t *testing.T) {
	for _, name := range []string{"remote", "fake", "missing"} {
		if _, err := CodexSetArgv([]byte(codexFixture), name, []string{"x"}); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: err = %v, want ErrNotFound", name, err)
		}
	}
	inline := "[mcp_servers]\nsrv = { command = \"x\", args = [\"a\"] }\n"
	if _, err := CodexSetArgv([]byte(inline), "srv", []string{"y"}); !errors.Is(err, ErrUnsupported) && !errors.Is(err, ErrNotFound) {
		t.Errorf("inline table: err = %v, want ErrUnsupported or ErrNotFound", err)
	}
}

// Review P1-1: a triple-quoted command used to parse as "" and wrap wrote invalid TOML.
func TestCodexTripleQuotedCommandUnsupported(t *testing.T) {
	for _, q := range []string{`"""`, `'''`} {
		src := "[mcp_servers.a]\ncommand = " + q + "npx" + q + "\nargs = [\"-y\"]\n"
		for _, s := range servers(t, src) {
			if len(s.Argv) == 0 || s.Argv[0] == "" {
				t.Errorf("%s: server reported with empty command: %+v", q, s)
			}
		}
		if _, err := CodexSetArgv([]byte(src), "a", []string{"/b/mcp-snooze", "--", "npx", "-y"}); !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: err = %v, want ErrUnsupported", q, err)
		}
	}
}

// Review P2-1: table headers inside a multi-line string are text, not tables.
func TestCodexMultilineStringHidesNoTables(t *testing.T) {
	for _, q := range []string{`"""`, `'''`} {
		src := "[mcp_servers.a]\ncommand = \"x\"\ndescription = " + q + "\n[mcp_servers.b]\ncommand = \"evil\"\n" + q + "\n"
		m := servers(t, src)
		if _, ok := m["b"]; ok || len(m) != 1 {
			t.Errorf("%s: got %v, want only a", q, m)
		}
		if _, err := CodexSetArgv([]byte(src), "b", []string{"y"}); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: edit inside string: err = %v, want ErrNotFound", q, err)
		}
	}
}

// Re-review P1-A and friends: every edit must either be refused or leave a document that a real
// TOML parser reads as the original with only the target's command/args changed.
func TestCodexEditIsRefusedOrExact(t *testing.T) {
	cases := map[string]string{
		"4-quote basic close":   "[mcp_servers.s]\ncommand = \"node\"\nenv = { A = \"\"\"say \"hi\"\"\"\", B = \"\"\"y\"\"\" }\nargs = [\"srv.js\"]\n",
		"4-quote literal close": "[mcp_servers.s]\ncommand = \"node\"\nnote = '''n'''' # x''''\nargs = [\"srv.js\"]\n",
		"quoted args key":       "[mcp_servers.s]\ncommand = \"node\"\n\"args\" = [\"srv.js\"]\n",
		"array of arrays":       "[mcp_servers.s]\ncommand = \"node\"\nm = [\n  [1],\n]\nargs = [\"srv.js\"]\n",
		"5-quote close":         "[mcp_servers.s]\ncommand = \"node\"\nnote = \"\"\"a\"\"\"\"\" # \"\"\"\nargs = [\"srv.js\"]\n",
	}
	want := []string{"/b/mcp-snooze", "--", "node", "srv.js"}
	for name, src := range cases {
		var before map[string]any
		if _, err := toml.Decode(src, &before); err != nil {
			t.Fatalf("%s: fixture is not valid TOML: %v", name, err)
		}
		if s, ok := servers(t, src)["s"]; ok && !reflect.DeepEqual(s.Argv, []string{"node", "srv.js"}) {
			t.Errorf("%s: CodexServers argv = %v, want [node srv.js]", name, s.Argv)
		}
		out, err := CodexSetArgv([]byte(src), "s", want)
		if err != nil {
			continue // refusing is fine
		}
		var after map[string]any
		if _, err := toml.Decode(string(out), &after); err != nil {
			t.Errorf("%s: wrap wrote invalid TOML: %v\n%s", name, err, out)
			continue
		}
		srv := before["mcp_servers"].(map[string]any)["s"].(map[string]any)
		srv["command"] = want[0]
		args := make([]any, len(want)-1)
		for i, a := range want[1:] {
			args[i] = a
		}
		srv["args"] = args
		if !reflect.DeepEqual(after, before) {
			t.Errorf("%s: edit changed more than command/args:\n got %v\nwant %v", name, after, before)
		}
	}
}

func TestCodexUnterminatedStringIsAnError(t *testing.T) {
	src := "[mcp_servers.s]\ncommand = \"node\"\nnote = \"\"\"a\n"
	if _, err := CodexSetArgv([]byte(src), "s", []string{"/b/mcp-snooze", "--", "node"}); err == nil {
		t.Fatal("edited a document that is not valid TOML")
	}
}

// Re-review P2-2: a nan anywhere in the file made the verify gate refuse every edit (NaN != NaN).
func TestCodexNaNElsewhereStillEdits(t *testing.T) {
	src := "v = nan\nw = [nan]\n[mcp_servers.s]\ncommand = \"node\"\nargs = [\"srv.js\"]\n"
	out, err := CodexSetArgv([]byte(src), "s", []string{"/b/mcp-snooze", "--", "node", "srv.js"})
	if err != nil {
		t.Fatalf("err = %v, want the edit to succeed", err)
	}
	if !strings.HasPrefix(string(out), "v = nan\nw = [nan]\n") {
		t.Fatalf("bytes outside the edit changed:\n%s", out)
	}
}

// Re-review P2-4: comments inside a multi-line args array were dropped by the one-line rewrite.
func TestCodexArgsWithCommentsUnsupported(t *testing.T) {
	src := "[mcp_servers.s]\ncommand = \"node\"\nargs = [ # entry\n  \"srv.js\", # main\n]\n"
	if _, err := CodexSetArgv([]byte(src), "s", []string{"/b/mcp-snooze", "--", "node", "srv.js"}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}
