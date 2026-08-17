package config

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const OpenCodeFileName = "opencode.json"

type OpenCode struct {
	cfg map[string]any

	// Distinguishes an absent file from an empty one: absent means the
	// scaffolded baseline policy is gone, which check warns about.
	Missing bool
}

func LoadOpenCode(dir string) (*OpenCode, error) {
	raw, err := os.ReadFile(filepath.Join(dir, OpenCodeFileName))
	if os.IsNotExist(err) {
		return &OpenCode{Missing: true}, nil
	}
	if err != nil {
		return &OpenCode{}, err
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return &OpenCode{}, fmt.Errorf("%s: %w", OpenCodeFileName, err)
	}
	return &OpenCode{cfg: cfg}, nil
}

func (o *OpenCode) ProviderBaseURL(id string) string {
	providers, _ := o.cfg["provider"].(map[string]any)
	entry, _ := providers[id].(map[string]any)
	options, _ := entry["options"].(map[string]any)
	u, _ := options["baseURL"].(string)
	return u
}

func (o *OpenCode) MCPServers() []string {
	mcp, _ := o.cfg["mcp"].(map[string]any)
	return slices.Sorted(maps.Keys(mcp))
}

func (o *OpenCode) Drift(modelPrefixes []string) []string {
	var warnings []string
	for _, key := range slices.Sorted(maps.Keys(o.cfg)) {
		if key != "$schema" && key != "permission" && key != "provider" && key != "agent" && key != "mcp" {
			warnings = append(warnings, fmt.Sprintf("opencode.json contains %q -- model choice belongs in openroutines.yml and frontmatter, not here", key))
		}
	}
	if agents, ok := o.cfg["agent"].(map[string]any); ok {
		for _, name := range slices.Sorted(maps.Keys(agents)) {
			if entry, ok := agents[name].(map[string]any); ok {
				if _, has := entry["model"]; has {
					warnings = append(warnings, fmt.Sprintf("agent %q in opencode.json sets a model -- model choice belongs in openroutines.yml and frontmatter, not here", name))
				}
			}
		}
	}
	if providers, ok := o.cfg["provider"].(map[string]any); ok {
		for _, id := range slices.Sorted(maps.Keys(providers)) {
			if !slices.Contains(modelPrefixes, id) {
				warnings = append(warnings, fmt.Sprintf("provider %q in opencode.json is not referenced by any model in openroutines.yml defaults or routine frontmatter", id))
			}
		}
	}
	return warnings
}

func mcpEntry(url, credential string) map[string]any {
	entry := map[string]any{"type": "remote", "url": url}
	if credential != "" {
		entry["headers"] = map[string]any{"Authorization": "Bearer {env:" + strings.ToUpper(credential) + "}"}
	}
	return entry
}

func MCPSnippet(name, url, credential string) string {
	entry, _ := json.Marshal(mcpEntry(url, credential))
	return fmt.Sprintf("%q: %s", name, entry)
}

func AddMCPServer(dir, name, url, credential string) error {
	path := filepath.Join(dir, OpenCodeFileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", OpenCodeFileName, err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("%s: %w", OpenCodeFileName, err)
	}
	mcp, _ := cfg["mcp"].(map[string]any)
	if mcp == nil {
		mcp = map[string]any{}
	}
	if _, exists := mcp[name]; exists {
		return fmt.Errorf("mcp server %q is already defined in %s", name, OpenCodeFileName)
	}
	mcp[name] = mcpEntry(url, credential)
	cfg["mcp"] = mcp
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), 0o644)
}
