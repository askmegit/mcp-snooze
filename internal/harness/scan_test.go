package harness

import (
	"reflect"
	"testing"
)

// ps output format consumed by parseProcessOutput: `pid rss(KiB) args...` per line
// (scan runs `ps -Ao pid=,rss=,args=`).

func TestScanMatchesWholeArgvAndSkipsProxies(t *testing.T) {
	servers := []Server{{Name: "srv", Argv: []string{"node", "srv.js"}}}
	ps := "  11 204800 node srv.js\n" +
		"  12 153600 node srv.js-extra\n" + // different argument: not this server
		"  13  10240 /opt/bin/mcp-snooze --idle 600 -- node srv.js\n" + // the proxy itself
		"  14   5120 grep node srv.js\n"
	got := parseProcessOutput(ps, servers)
	if got[0][0] != 1 || got[0][1] != 200 {
		t.Fatalf("got %v, want 1 process / 200 MiB", got[0])
	}
}

func TestScanMatchesCommandByBasename(t *testing.T) {
	servers := []Server{{Name: "dart", Argv: []string{"dart", "mcp-server"}}}
	got := parseProcessOutput("21 102400 /usr/local/bin/dart mcp-server\n", servers)
	if got[0][0] != 1 {
		t.Fatalf("got %v, want the absolute-path process matched", got[0])
	}
}

func TestScanCountsEachProcessOnce(t *testing.T) {
	servers := []Server{
		{Harness: "claude", Name: "dart", Argv: []string{"dart", "mcp-server"}},
		{Harness: "codex", Name: "dart", Argv: []string{"/x/dart", "mcp-server"}},
	}
	got := parseProcessOutput("31 102400 /x/dart mcp-server\n", servers)
	if got[0][0]+got[1][0] != 1 {
		t.Fatalf("one process counted %v times across rows: %v", got[0][0]+got[1][0], got)
	}
}

func TestMaskSecretArgs(t *testing.T) {
	in := []string{"-y", "srv", "--api-key", "abc123", "--token=xyz", "-e", "GITHUB_TOKEN=ghp_1", "PASSWORD=p", "--port", "8080"}
	want := []string{"-y", "srv", "--api-key", "***", "--token=***", "-e", "GITHUB_TOKEN=***", "PASSWORD=***", "--port", "8080"}
	if got := maskSecretArgs(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	if in[3] != "abc123" {
		t.Fatal("maskSecretArgs modified its input")
	}
}
