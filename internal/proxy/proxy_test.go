package proxy

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCacheKeySeparatesCommandCWDAndProtocolVersion(t *testing.T) {
	base := cacheKey([]string{"server", "--stdio"}, "/work/a", "2025-06-18")
	for name, got := range map[string]string{
		"command": cacheKey([]string{"server", "--other"}, "/work/a", "2025-06-18"),
		"cwd":     cacheKey([]string{"server", "--stdio"}, "/work/b", "2025-06-18"),
		"version": cacheKey([]string{"server", "--stdio"}, "/work/a", "2025-03-26"),
	} {
		t.Run(name, func(t *testing.T) {
			if got == base {
				t.Fatalf("cache key did not change when %s changed", name)
			}
		})
	}
	if again := cacheKey([]string{"server", "--stdio"}, "/work/a", "2025-06-18"); again != base {
		t.Fatalf("identical cache inputs produced different keys: %q != %q", again, base)
	}
}

func TestReadCacheRejectsMissingListsAndErrorResponses(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{
			name:  "error response",
			value: `{"initialize":{"capabilities":{"tools":{}}},"lists":{"tools/list":{"error":{"code":-32000,"message":"not ready"}}}}`,
		},
		{
			name:  "missing required list",
			value: `{"initialize":{"capabilities":{"tools":{}}},"lists":{}}`,
		},
		{
			name:  "missing capability list",
			value: `{"initialize":{"capabilities":{"resources":{}}},"lists":{"tools/list":{"result":{}},"resources/list":{"result":{}}}}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "cache.json")
			if err := os.WriteFile(path, []byte(test.value), 0600); err != nil {
				t.Fatal(err)
			}
			if got := readCache(path, time.Minute); got != nil {
				t.Fatalf("invalid cache was accepted: %#v", got)
			}
		})
	}
}

func TestReadCacheAcceptsCompleteSuccessfulLists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	value := `{"initialize":{"capabilities":{"tools":{},"resources":{},"prompts":{}}},"lists":{"tools/list":{"result":{"tools":[]}},"resources/list":{"result":{"resources":[]}},"resources/templates/list":{"result":{"resourceTemplates":[]}},"prompts/list":{"result":{"prompts":[]}}}}`
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
	if got := readCache(path, time.Minute); got == nil {
		t.Fatal("complete successful cache was rejected")
	}
}
