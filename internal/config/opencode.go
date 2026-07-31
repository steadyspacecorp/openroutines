package config

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// OpenCodeFileName is the harness's config file, named for opencode.
const OpenCodeFileName = "opencode.json"

// OpenCode is the framework's read of opencode.json. The file is harness
// config -- the permission policy, provider endpoint definitions, MCP server
// definitions with transports and auth headers -- interpreted by opencode
// alone. The framework never acts on those blocks; it reads names and shapes
// only, to enforce per-routine grants and to flag configuration drift. The
// zero value is the view of an agent with no opencode.json.
type OpenCode struct {
	cfg map[string]any

	// Missing distinguishes an absent file from an empty one: absent means
	// the scaffolded baseline policy is gone, which check warns about.
	Missing bool
}

// LoadOpenCode parses dir's opencode.json. A missing file is an agent
// without harness config -- an empty view, no error. An unparseable file is
// an error: opencode itself could not load it, so every run would fail.
// The returned view is always usable, empty on error.
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

// ProviderBaseURL returns the baseURL a provider block declares, or "" when
// none does. Quoted in run diagnostics when that endpoint rejects a run's
// credentials; quoting is not interpreting -- the endpoint definition stays
// opencode's alone, and the framework never requests against it.
func (o *OpenCode) ProviderBaseURL(id string) string {
	providers, _ := o.cfg["provider"].(map[string]any)
	entry, _ := providers[id].(map[string]any)
	options, _ := entry["options"].(map[string]any)
	u, _ := options["baseURL"].(string)
	return u
}

// MCPServers returns the names defined in the `mcp` block, sorted. The
// entries themselves stay opaque -- the names are what grants reference and
// what permission rules close over.
func (o *OpenCode) MCPServers() []string {
	mcp, _ := o.cfg["mcp"].(map[string]any)
	return slices.Sorted(maps.Keys(mcp))
}

// envRefPattern matches opencode's {env:NAME} config placeholders.
var envRefPattern = regexp.MustCompile(`\{env:([A-Za-z_][A-Za-z0-9_]*)\}`)

// MCPEnvRefs returns the environment names a server's entry references via
// {env:...} placeholders (auth headers, typically), sorted. check
// cross-checks them against each granting routine's planned run environment:
// runs construct their environment from scratch, so a reference no grant
// satisfies resolves empty at run time and surfaces as an opaque auth
// failure instead of a configuration problem.
func (o *OpenCode) MCPEnvRefs(name string) []string {
	mcp, _ := o.cfg["mcp"].(map[string]any)
	entry, present := mcp[name]
	if !present {
		return nil
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		return nil
	}
	set := map[string]struct{}{}
	for _, m := range envRefPattern.FindAllStringSubmatch(string(raw), -1) {
		set[m[1]] = struct{}{}
	}
	return slices.Sorted(maps.Keys(set))
}

// Drift returns warnings about framework concerns that have crept into the
// harness's file. Model *choice* is framework config (the framework
// interprets it for per-routine resolution and provider-key injection), so a
// harness-side default model or anything else beyond the known blocks is
// drift worth flagging (it has arrived via coding-agent sessions before) --
// including a model pinned inside an agent override. Defined providers are
// cross-checked against modelPrefixes, the providers referenced by
// openroutines.yml defaults and routine frontmatter: an unreferenced id
// usually means a typo on one side.
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

// mcpEntry renders a declared server as the opencode.json entry a person
// consents to. Always remote (the framework's only supported transport); a
// named credential becomes the standard bearer header referencing the
// credential's run-environment name.
func mcpEntry(url, credential string) map[string]any {
	entry := map[string]any{"type": "remote", "url": url}
	if credential != "" {
		entry["headers"] = map[string]any{"Authorization": "Bearer {env:" + strings.ToUpper(credential) + "}"}
	}
	return entry
}

// MCPSnippet is a declared server's entry as the exact JSON shown at the
// consent prompt and in the paste step -- what you read is what AddMCPServer
// lands.
func MCPSnippet(name, url, credential string) string {
	entry, _ := json.Marshal(mcpEntry(url, credential))
	return fmt.Sprintf("%q: %s", name, entry)
}

// AddMCPServer inserts one declared server into opencode.json's mcp block.
// Refuses to overwrite: an existing entry is the person's. The caller is
// responsible for the consent gate -- this writes an endpoint definition
// into harness config, which is why plugins may only ever declare servers
// and a person confirms each insertion interactively. Rewrites the file
// with two-space indentation; encoding/json sorts object keys, so hand
// ordering is not preserved.
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
