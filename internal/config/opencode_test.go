package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestMCPServers(t *testing.T) {
	dir := t.TempDir()
	if got := MCPServers(dir); got != nil {
		t.Fatalf("no opencode.json should mean no servers, got %v", got)
	}
	write := func(body string) {
		if err := os.WriteFile(filepath.Join(dir, "opencode.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"permission": {}}`)
	if got := MCPServers(dir); got != nil {
		t.Fatalf("no mcp block should mean no servers, got %v", got)
	}
	write(`{"mcp": {"steady": {"type": "remote"}, "acme": {"type": "remote"}}}`)
	if got := MCPServers(dir); !slices.Equal(got, []string{"acme", "steady"}) {
		t.Fatalf("expected sorted server names, got %v", got)
	}
}

// AddMCPServer inserts exactly one consented entry, creates the block when
// absent, and never overwrites -- an existing entry is the person's.
func TestAddMCPServer(t *testing.T) {
	dir := t.TempDir()
	entry := map[string]any{"type": "remote", "url": "https://example.test/mcp"}

	if err := AddMCPServer(dir, "steady", entry); err == nil {
		t.Fatal("no opencode.json should refuse, not create harness config")
	}

	path := filepath.Join(dir, "opencode.json")
	if err := os.WriteFile(path, []byte(`{"permission": {"question": "deny"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := AddMCPServer(dir, "steady", entry); err != nil {
		t.Fatal(err)
	}
	if got := MCPServers(dir); !slices.Equal(got, []string{"steady"}) {
		t.Fatalf("server not inserted: %v", got)
	}
	raw, _ := os.ReadFile(path)
	for _, want := range []string{`"question": "deny"`, `"url": "https://example.test/mcp"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("rewritten opencode.json missing %q:\n%s", want, raw)
		}
	}

	if err := AddMCPServer(dir, "steady", map[string]any{"url": "https://evil.test"}); err == nil || !strings.Contains(err.Error(), "already defined") {
		t.Fatalf("existing entry must be refused, got %v", err)
	}
	if raw, _ := os.ReadFile(path); strings.Contains(string(raw), "evil.test") {
		t.Fatal("refused insert must not touch the file")
	}
}
