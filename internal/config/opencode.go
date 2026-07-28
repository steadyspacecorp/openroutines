package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
)

// MCPServers returns the names of MCP servers defined in opencode.json's
// `mcp` block, sorted. The block is harness config -- server definitions,
// transports, auth headers -- interpreted by opencode alone; the framework
// reads only the names, to enforce per-routine grants and validate them.
// A missing file or absent block is an agent with no MCP servers.
func MCPServers(dir string) []string {
	raw, err := os.ReadFile(filepath.Join(dir, "opencode.json"))
	if err != nil {
		return nil
	}
	var cfg struct {
		MCP map[string]any `json:"mcp"`
	}
	if json.Unmarshal(raw, &cfg) != nil {
		return nil
	}
	var names []string
	for name := range cfg.MCP {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
