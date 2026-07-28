package config

import (
	"os"
	"path/filepath"
	"slices"
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
