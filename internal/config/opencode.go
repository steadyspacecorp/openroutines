package config

import (
	"encoding/json"
	"fmt"
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

// AddMCPServer inserts one server entry into opencode.json's mcp block.
// Refuses to overwrite: an existing entry is the person's. The caller is
// responsible for the consent gate -- this writes an endpoint definition
// into harness config, which is why plugins may only ever declare servers
// and a person confirms each insertion interactively. Rewrites the file
// with two-space indentation; encoding/json sorts object keys, so hand
// ordering is not preserved.
func AddMCPServer(dir, name string, entry map[string]any) error {
	path := filepath.Join(dir, "opencode.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading opencode.json: %w", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("opencode.json: %w", err)
	}
	mcp, _ := cfg["mcp"].(map[string]any)
	if mcp == nil {
		mcp = map[string]any{}
	}
	if _, exists := mcp[name]; exists {
		return fmt.Errorf("mcp server %q is already defined in opencode.json", name)
	}
	mcp[name] = entry
	cfg["mcp"] = mcp
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), 0o644)
}
