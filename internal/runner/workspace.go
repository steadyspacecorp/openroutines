package runner

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/template"
	"time"

	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/filetree"
	"github.com/steadyspacecorp/openroutines/internal/knowledge"
	"github.com/steadyspacecorp/openroutines/internal/routine"
	"github.com/steadyspacecorp/openroutines/internal/skill"
)

func buildWorkspace(dir, workspace, name string) error {
	// Copy an allow-list; a deny-list once exposed the encrypted credential store.
	for _, file := range []string{filepath.Base(config.Path(dir)), config.OpenCodeFileName} {
		raw, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if err := os.WriteFile(filepath.Join(workspace, file), raw, 0o644); err != nil {
			return err
		}
	}
	routines, errs := routine.LoadAgent(dir)
	for _, err := range errs {
		if routine.Concerns(err, name) {
			return err
		}
	}
	for _, r := range routines {
		raw, err := os.ReadFile(r.Path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(workspace, "routines"), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(workspace, "routines", r.Name+".md"), raw, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func copyDeclaredSkills(dir, workspace string, names []string) error {
	for _, name := range names {
		if !skill.NamePattern.MatchString(name) {
			return fmt.Errorf("declared skill %q is not a valid Agent Skills name", name)
		}
		found, err := skill.Find(dir, name)
		if err != nil {
			return fmt.Errorf("declared skill unavailable: %w", err)
		}
		src := found.Dir
		dest := filepath.Join(workspace, ".opencode", "skills", name)
		err = filetree.CopyRegular(src, dest, filetree.Options{Mode: filetree.PreserveExecutables})
		if err != nil {
			return err
		}
	}
	return nil
}

func applyDeclaredMCP(workspace string, granted []string) error {
	path := filepath.Join(workspace, config.OpenCodeFileName)
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil
	}
	mcpRaw, ok := cfg["mcp"]
	if !ok {
		return nil
	}
	var mcp map[string]json.RawMessage
	if err := json.Unmarshal(mcpRaw, &mcp); err != nil || len(mcp) == 0 {
		return nil
	}
	filtered := map[string]json.RawMessage{}
	for _, name := range granted {
		if entry, ok := mcp[name]; ok {
			filtered[name] = entry
		}
	}
	if len(filtered) == len(mcp) {
		return nil
	}
	if len(filtered) == 0 {
		delete(cfg, "mcp")
	} else {
		filteredRaw, err := json.Marshal(filtered)
		if err != nil {
			return err
		}
		cfg["mcp"] = filteredRaw
	}
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), 0o644)
}

//go:embed instruction.md
var instructionSrc string

var instructionTmpl = template.Must(template.New("instruction").Parse(instructionSrc))

type instructionData struct {
	AgentName     string
	Instructions  string
	RoutineName   string
	RunID         string
	RecordsEvents bool
	Reports       bool
	Changes       string
	Marker        string
	Variables     string
}

func writeAgentDefinition(workspace string, agent *config.Agent, r *routine.Routine, servers []string, attempt Attempt) error {
	def, err := renderDefinition(agent, r, servers, attempt)
	if err != nil {
		return err
	}
	dir := filepath.Join(workspace, ".opencode", "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "routine.md"), []byte(def), 0o644)
}

func renderDefinition(agent *config.Agent, r *routine.Routine, servers []string, attempt Attempt) (string, error) {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "description: Generated for routine %s -- derived from frontmatter, do not edit\n", r.Name)
	b.WriteString("mode: primary\n")
	b.WriteString("permission:\n")
	if attempt.ReadOnly {
		// A knowledge briefing reads its prepared snapshot and nothing else.
		// Provider traffic is the harness, not a model tool. Start closed so a
		// new built-in tool cannot accidentally add authority to this surface;
		// the last matching rule wins, so the three readers reopen afterwards.
		b.WriteString("  \"*\": deny\n")
		for _, tool := range []string{"read", "glob", "grep"} {
			fmt.Fprintf(&b, "  %s: allow\n", tool)
		}
	}
	// Web access is a grant, not a default: opencode allows webfetch out of
	// the box, and fetched content is a prompt-injection vector.
	for _, w := range []struct {
		tool    string
		granted bool
	}{{"webfetch", r.Frontmatter.Webfetch}, {"websearch", r.Frontmatter.Websearch}} {
		action := "deny"
		if w.granted {
			action = "allow"
		}
		fmt.Fprintf(&b, "  %s: %s\n", w.tool, action)
	}
	// opencode registers MCP tools as <server>_<tool>, so one glob per
	// configured server closes or opens its whole surface.
	for _, server := range servers {
		action := "deny"
		if slices.Contains(r.Frontmatter.MCP, server) {
			action = "allow"
		}
		fmt.Fprintf(&b, "  %q: %s\n", server+"_*", action)
	}
	b.WriteString("  skill:\n")
	b.WriteString("    \"*\": deny\n") // order matters: last matching rule wins
	for _, s := range r.Frontmatter.Skills {
		fmt.Fprintf(&b, "    %q: allow\n", s)
	}
	b.WriteString("---\n\n")

	if err := instructionTmpl.Execute(&b, instructionData{
		AgentName:     agent.Name,
		Instructions:  strings.TrimSpace(agent.Instructions),
		RoutineName:   r.Name,
		RunID:         attempt.RunID,
		RecordsEvents: r.Frontmatter.RecordsEvents(),
		Reports:       r.Frontmatter.Reports,
		Changes:       knowledge.ChangesFileName,
		Marker:        knowledge.ConsumeMarker,
		Variables:     variablesLine(agent),
	}); err != nil {
		return "", err
	}
	return b.String(), nil
}

func RenderDefinition(agent *config.Agent, r *routine.Routine, servers []string) (string, error) {
	return renderDefinition(agent, r, servers, Attempt{RunID: "run_check"})
}

func formatAttemptTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

func variablesLine(agent *config.Agent) string {
	names := slices.Sorted(maps.Keys(agent.Variables))
	for i, n := range names {
		names[i] = "$" + strings.ToUpper(n)
	}
	return strings.Join(names, ", ")
}
