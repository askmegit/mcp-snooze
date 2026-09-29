package harness

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const claudeFixture = `{
  "numStartups": 12345678901234567890,
  "mcpServers": {
    "maestro": {"type": "stdio", "command": "maestro", "args": ["mcp", "--no-viewer"], "env": {}},
    "appium": {"command": "npx", "args": ["-y", "appium-mcp@1"], "env": {"NO_UI": "true"}},
    "bare": {"command": "srv"},
    "remote": {"type": "http", "url": "https://example.com/mcp"},
    "sse": {"type": "sse", "url": "https://example.com/sse"}
  },
  "projects": {
    "/tmp/p": {"allowedTools": ["x"], "mcpServers": {"dart": {"type": "stdio", "command": "dart", "args": ["mcp-server"]}}},
    "/tmp/empty": {"allowedTools": []}
  }
}`

func claudeMap(t *testing.T, src []byte) map[string]Server {
	t.Helper()
	list, err := ClaudeServers(src)
	if err != nil {
		t.Fatalf("ClaudeServers: %v", err)
	}
	m := map[string]Server{}
	for _, s := range list {
		if s.Harness != "claude" || !s.Enabled {
			t.Fatalf("bad entry %+v", s)
		}
		m[s.Scope+"/"+s.Name] = s
	}
	return m
}

func TestClaudeServers(t *testing.T) {
	m := claudeMap(t, []byte(claudeFixture))
	want := map[string][]string{
		"user/maestro": {"maestro", "mcp", "--no-viewer"},
		"user/appium":  {"npx", "-y", "appium-mcp@1"},
		"user/bare":    {"srv"},
		"/tmp/p/dart":  {"dart", "mcp-server"},
	}
	if len(m) != len(want) {
		t.Fatalf("got %d servers: %v", len(m), m)
	}
	for k, argv := range want {
		if !reflect.DeepEqual(m[k].Argv, argv) || m[k].Wrapped {
			t.Errorf("%s: %+v", k, m[k])
		}
	}
}

func generic(t *testing.T, b []byte) map[string]any {
	t.Helper()
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.UseNumber()
	var v map[string]any
	if err := d.Decode(&v); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, b)
	}
	return v
}

func TestClaudeSetArgvWrapUnwrap(t *testing.T) {
	src := []byte(claudeFixture)
	w, err := ClaudeSetArgv(src, "user", "appium", WrappedArgv("/b/mcp-snooze", []string{"--idle", "1800"}, []string{"npx", "-y", "appium-mcp@1"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(w), "12345678901234567890") {
		t.Fatal("large integer was not preserved")
	}
	if !strings.Contains(string(w), "\n  \"") {
		t.Fatal("output is not 2-space indented")
	}
	s := claudeMap(t, w)["user/appium"]
	if !s.Wrapped || !reflect.DeepEqual(s.ProxyFlags, []string{"--idle", "1800"}) || !reflect.DeepEqual(s.Argv, []string{"npx", "-y", "appium-mcp@1"}) {
		t.Fatalf("after wrap: %+v", s)
	}
	gw, g0 := generic(t, w), generic(t, src)
	envW := gw["mcpServers"].(map[string]any)["appium"].(map[string]any)["env"]
	env0 := g0["mcpServers"].(map[string]any)["appium"].(map[string]any)["env"]
	if !reflect.DeepEqual(envW, env0) {
		t.Fatal("env changed")
	}
	for _, k := range []string{"remote", "sse", "maestro"} {
		if !reflect.DeepEqual(gw["mcpServers"].(map[string]any)[k], g0["mcpServers"].(map[string]any)[k]) {
			t.Errorf("%s touched", k)
		}
	}
	if !reflect.DeepEqual(gw["projects"], g0["projects"]) {
		t.Error("projects touched by a user-scope edit")
	}
	back, err := ClaudeSetArgv(w, "user", "appium", []string{"npx", "-y", "appium-mcp@1"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(generic(t, back), g0) {
		t.Fatal("unwrap did not restore the document")
	}
}

func TestClaudeArgsKeyRemovedForBareCommand(t *testing.T) {
	src := []byte(claudeFixture)
	w, err := ClaudeSetArgv(src, "user", "bare", []string{"/b/mcp-snooze", "--", "srv"})
	if err != nil {
		t.Fatal(err)
	}
	back, err := ClaudeSetArgv(w, "user", "bare", []string{"srv"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(generic(t, back), generic(t, src)) {
		t.Fatalf("bare command round trip added keys:\n%s", back)
	}
}

func TestClaudeProjectScopeAndErrors(t *testing.T) {
	src := []byte(claudeFixture)
	w, err := ClaudeSetArgv(src, "/tmp/p", "dart", []string{"/b/mcp-snooze", "--", "dart", "mcp-server"})
	if err != nil {
		t.Fatal(err)
	}
	if !claudeMap(t, w)["/tmp/p/dart"].Wrapped {
		t.Fatal("project-scope edit not applied")
	}
	for _, c := range [][2]string{{"user", "remote"}, {"user", "dart"}, {"/tmp/empty", "dart"}, {"/nope", "x"}} {
		if _, err := ClaudeSetArgv(src, c[0], c[1], []string{"x"}); !errors.Is(err, ErrNotFound) {
			t.Errorf("%v: err = %v, want ErrNotFound", c, err)
		}
	}
}

func TestWriteFileAtomicKeepsModeAndSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.json")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if err := writeFileAtomic(link, []byte("new")); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink was replaced by a regular file")
	}
	b, _ := os.ReadFile(target)
	fi, _ := os.Stat(target)
	if string(b) != "new" || fi.Mode().Perm() != 0o600 {
		t.Fatalf("target = %q mode %v", b, fi.Mode().Perm())
	}
	left, _ := filepath.Glob(filepath.Join(dir, "*"))
	if len(left) != 2 {
		t.Fatalf("temp files left behind: %v", left)
	}
}

// Review P2-2: non-string args used to be dropped silently, so wrap changed the command line.
func TestClaudeNonStringArgsUnsupported(t *testing.T) {
	src := []byte(`{"mcpServers": {"s": {"command": "srv", "args": ["--port", 8080, "x"]}}}`)
	if s, ok := claudeMap(t, src)["user/s"]; ok {
		t.Fatalf("server with non-string args listed as %v", s.Argv)
	}
	if _, err := ClaudeSetArgv(src, "user", "s", []string{"/b/mcp-snooze", "--", "srv"}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}
