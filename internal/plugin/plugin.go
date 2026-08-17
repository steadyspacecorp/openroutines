package plugin

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/steadyspacecorp/openroutines/internal/frontmatter"
	"github.com/steadyspacecorp/openroutines/internal/routine"
	"github.com/steadyspacecorp/openroutines/internal/skill"
	"github.com/steadyspacecorp/openroutines/internal/trigger"
)

const FileName = "PLUGIN.md"

const SourceFileName = ".openroutines-source.yaml"

var revisionPattern = regexp.MustCompile(`^[0-9a-f]{40,64}$`)

type Source struct {
	Repository string `yaml:"repository"`
	Path       string `yaml:"path,omitempty"`
	Revision   string `yaml:"revision"`
}

type Credential struct {
	Description string `yaml:"description"`
	Type        string `yaml:"type,omitempty"`
}

type Variable struct {
	Description string `yaml:"description"`
}

// MCP entries declare endpoint needs; endpoint configuration and credentials
// remain agent-owned so a plugin cannot grant itself access.
type MCPServer struct {
	Description string `yaml:"description"`
	URL         string `yaml:"url"`
	Credential  string `yaml:"credential,omitempty"`
}

type Manifest struct {
	Name        string                `yaml:"name"`
	Description string                `yaml:"description"`
	Credentials map[string]Credential `yaml:"credentials,omitempty"`
	Variables   map[string]Variable   `yaml:"variables,omitempty"`
	MCP         map[string]MCPServer  `yaml:"mcp,omitempty"`
}

type Plugin struct {
	Manifest Manifest
	Body     string
	Dir      string
	Routines []*routine.Routine
	Skills   []*skill.Skill
	Stubs    []string
}

var benignRoot = map[string]bool{
	"README.md": true, "LICENSE": true, "LICENSE.md": true,
	"LICENSE.txt": true, ".gitignore": true, SourceFileName: true,
}

var forbidden = map[string]string{
	"opencode.json":     "opencode.json is the agent's harness config -- a plugin granting itself permissions or endpoints is exactly what the allow-list exists to stop",
	"openroutines.yml":  "openroutines.yml belongs to the agent; credential metadata and variables are declared in PLUGIN.md and printed as next steps",
	"openroutines.yaml": "openroutines.yaml (a legacy config name) belongs to the agent; declare needs in PLUGIN.md",
	"agent.yaml":        "agent.yaml (a legacy config name) belongs to the agent; declare needs in PLUGIN.md",
	"Dockerfile":        "the Dockerfile is framework-owned",
	".openroutines":     "the framework directory is reserved for the agent's framework state",
	"master.key":        "a plugin must never carry key material",
}

func Load(dir string, agentSkills map[string]bool) (*Plugin, error) {
	manifest, body, err := parseManifestFile(filepath.Join(dir, FileName))
	if err != nil {
		return nil, err
	}
	p := &Plugin{Manifest: *manifest, Body: body, Dir: dir}
	validator := pluginValidator{plugin: p, agentSkills: agentSkills}
	validator.validateManifest()
	if err := validator.validatePayload(); err != nil {
		return nil, err
	}
	validator.loadContents()
	validator.validateContents()
	if err := validator.err(); err != nil {
		return nil, err
	}
	return p, nil
}

func parseManifestFile(path string) (*Manifest, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", fmt.Errorf("no %s found -- is this a plugin?", FileName)
		}
		return nil, "", err
	}
	doc, err := frontmatter.Split(raw)
	if errors.Is(err, frontmatter.ErrMissing) {
		return nil, "", fmt.Errorf("%s: missing frontmatter", FileName)
	}
	if errors.Is(err, frontmatter.ErrUnterminated) {
		return nil, "", fmt.Errorf("%s: unterminated frontmatter", FileName)
	}
	if err != nil {
		return nil, "", err
	}
	var m Manifest
	dec := yaml.NewDecoder(bytes.NewReader(doc.Frontmatter))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil && !errors.Is(err, io.EOF) {
		return nil, "", fmt.Errorf("%s: %w", FileName, err)
	}
	return &m, strings.TrimSpace(string(doc.Body)), nil
}

func agentNamespace(agentDir string) ([]*routine.Routine, []*skill.Skill, error) {
	_, agentRoutineErrs := routine.LoadDir(filepath.Join(agentDir, "routines"))
	routines, pluginRoutineErrs := routine.LoadPlugins(agentDir)
	skills, skillErrs := skill.ListAgent(agentDir)
	if errs := slices.Concat(agentRoutineErrs, pluginRoutineErrs, skillErrs); len(errs) > 0 {
		return nil, nil, errors.Join(errs...)
	}
	return routines, skills, nil
}

func (p *Plugin) Summary() string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }

	if len(p.Routines) > 0 {
		w("Routines:\n")
		for _, r := range p.Routines {
			w("  %s", r.Name)
			if r.Frontmatter.Schedule != "" {
				w("  schedule %q", r.Frontmatter.Schedule)
			}
			if r.Frontmatter.Model != "" {
				w("  model %s", r.Frontmatter.Model)
			}
			if r.Frontmatter.Reports {
				w("  reports")
			}
			w("\n")
			if len(r.Frontmatter.Credentials) > 0 {
				w("    credentials: %s\n", strings.Join(r.Frontmatter.Credentials, ", "))
			}
			if len(r.Frontmatter.Skills) > 0 {
				w("    skills: %s\n", strings.Join(r.Frontmatter.Skills, ", "))
			}
			if len(r.Frontmatter.MCP) > 0 {
				w("    mcp: %s\n", strings.Join(r.Frontmatter.MCP, ", "))
			}
			// Web access is a grant like any other, and the one a reviewer of an
			// unfamiliar bundle most wants named: it is how untrusted text gets in.
			var web []string
			if r.Frontmatter.Webfetch {
				web = append(web, "webfetch")
			}
			if r.Frontmatter.Websearch {
				web = append(web, "websearch")
			}
			if len(web) > 0 {
				w("    web access: %s\n", strings.Join(web, ", "))
			}
			if t := r.Frontmatter.Trigger; t != nil {
				w("    trigger: polls %s", t.Poll)
				if t.Credential != "" {
					if strings.Contains(t.Poll, trigger.CredentialReference(t.Credential)) {
						w(" with credential %s substituted into the URL", t.Credential)
					} else {
						w(" with credential %s as a bearer token", t.Credential)
					}
				}
				w("\n")
			}
		}
	}
	if len(p.Skills) > 0 {
		var names []string
		for _, s := range p.Skills {
			names = append(names, s.Name)
		}
		w("Skills: %s\n", strings.Join(names, ", "))
	}
	for _, name := range slices.Sorted(maps.Keys(p.Manifest.Credentials)) {
		c := p.Manifest.Credentials[name]
		w("Credential to fill in: %s -- %s", name, c.Description)
		if c.Type != "" {
			w(" (typed: %s -- needs an openroutines.yml credentials entry)", c.Type)
		}
		w("\n")
	}
	for _, name := range slices.Sorted(maps.Keys(p.Manifest.Variables)) {
		w("Variable to set: %s -- %s\n", name, p.Manifest.Variables[name].Description)
	}
	for _, name := range slices.Sorted(maps.Keys(p.Manifest.MCP)) {
		m := p.Manifest.MCP[name]
		w("MCP server to define: %s -- %s (%s", name, m.Description, m.URL)
		if m.Credential != "" {
			w("; auth via credential %s", m.Credential)
		}
		w(") -- needs an opencode.json mcp entry\n")
	}
	for _, s := range p.Stubs {
		w("Ledger stub: %s\n", s)
	}
	return b.String()
}

func ReadSource(dir string) (Source, error) {
	var source Source
	raw, err := os.ReadFile(filepath.Join(dir, SourceFileName))
	if err != nil {
		return source, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&source); err != nil {
		return source, fmt.Errorf("%s: %w", SourceFileName, err)
	}
	return source, source.validate()
}

func (s Source) validate() error {
	if s.Repository == "" || s.Revision == "" {
		return fmt.Errorf("%s needs repository and revision", SourceFileName)
	}
	cleanPath := filepath.Clean(filepath.FromSlash(s.Path))
	if s.Path != "" && (filepath.IsAbs(cleanPath) || cleanPath == ".." || strings.HasPrefix(cleanPath, ".."+string(filepath.Separator))) {
		return fmt.Errorf("%s path %q escapes the source repository", SourceFileName, s.Path)
	}
	if !revisionPattern.MatchString(s.Revision) {
		return fmt.Errorf("%s revision must be a full git commit hash", SourceFileName)
	}
	return nil
}
