// Package plugin reads, installs, and updates grouped bundles of routines,
// skills, and knowledge-ledger stubs described by a PLUGIN.md manifest (see
// design decision "Plugins"). Validation is all-or-nothing over the whole payload
// before anything is copied, and violation is refusal, not a skipped file.
package plugin

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/steadyspacecorp/openroutines/internal/creds"
	"github.com/steadyspacecorp/openroutines/internal/frontmatter"
	"github.com/steadyspacecorp/openroutines/internal/routine"
	"github.com/steadyspacecorp/openroutines/internal/skill"
)

// FileName is the plugin manifest at the bundle root.
const FileName = "PLUGIN.md"

// SourceFileName records the upstream identity of an installed plugin.
const SourceFileName = ".openroutines-source.yaml"

var revisionPattern = regexp.MustCompile(`^[0-9a-f]{40,64}$`)

// Source is framework-owned provenance stored beside a vendored plugin.
type Source struct {
	Repository string `yaml:"repository"`
	Path       string `yaml:"path,omitempty"`
	Revision   string `yaml:"revision"`
}

// Credential is one required credential: a description for the person who
// will fill it in, and for typed credentials the derived type to declare in
// openroutines.yml. Never a value -- secrets are not part of a plugin.
type Credential struct {
	Description string `yaml:"description"`
	Type        string `yaml:"type,omitempty"`
}

// Variable is one required non-secret configuration value.
type Variable struct {
	Description string `yaml:"description"`
}

// MCPServer is one MCP server the bundle's routines grant. A declaration of
// need, never configuration: the server is defined in the agent's
// opencode.json by a person, because an MCP entry is an endpoint plus auth
// headers -- exactly what a plugin must not be able to write or update
// (see the forbidden list). The URL and credential here are what the
// install prints for the person to review and paste.
type MCPServer struct {
	Description string `yaml:"description"`
	URL         string `yaml:"url"`
	Credential  string `yaml:"credential,omitempty"` // manifest credential its auth header references
}

// Manifest is PLUGIN.md's frontmatter.
type Manifest struct {
	Name        string                `yaml:"name"`
	Description string                `yaml:"description"`
	Credentials map[string]Credential `yaml:"credentials,omitempty"`
	Variables   map[string]Variable   `yaml:"variables,omitempty"`
	MCP         map[string]MCPServer  `yaml:"mcp,omitempty"`
}

// Plugin is a validated bundle, ready to summarize and install.
type Plugin struct {
	Manifest Manifest
	Body     string // manifest body: the plugin's README, shown at install
	Dir      string
	Routines []*routine.Routine
	Skills   []*skill.Skill
	Stubs    []string // knowledge/ledgers/<name>.md, relative to Dir
}

// benignRoot are repository housekeeping files tolerated (never copied) at
// the bundle root, so a standalone plugin repo can exist on a forge.
var benignRoot = map[string]bool{
	"README.md": true, "LICENSE": true, "LICENSE.md": true,
	"LICENSE.txt": true, ".gitignore": true, SourceFileName: true,
}

// forbidden are agent- or harness-owned files a plugin must never ship;
// naming them gets a sharper refusal than the generic allow-list one.
var forbidden = map[string]string{
	"opencode.json":     "opencode.json is the agent's harness config -- a plugin granting itself permissions or endpoints is exactly what the allow-list exists to stop",
	"openroutines.yml":  "openroutines.yml belongs to the agent; credential metadata and variables are declared in PLUGIN.md and printed as next steps",
	"openroutines.yaml": "openroutines.yaml (a legacy config name) belongs to the agent; declare needs in PLUGIN.md",
	"agent.yaml":        "agent.yaml (a legacy config name) belongs to the agent; declare needs in PLUGIN.md",
	"Dockerfile":        "the Dockerfile is framework-owned",
	".openroutines":     "the framework directory is reserved for the agent's framework state",
	"master.key":        "a plugin must never carry key material",
}

// Load parses and validates the plugin at dir. agentSkills are skill names
// already present in the installing agent, for consistency checking. The
// returned error aggregates every problem found -- nothing is partially ok.
func Load(dir string, agentSkills map[string]bool) (*Plugin, error) {
	name, body, err := parseManifestFile(filepath.Join(dir, FileName))
	if err != nil {
		return nil, err
	}
	p := &Plugin{Manifest: *name, Body: body, Dir: dir}

	var problems []string
	badf := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	if !skill.NamePattern.MatchString(p.Manifest.Name) {
		badf("plugin name %q must be a bare lowercase-hyphen name (steady, github-docs)", p.Manifest.Name)
	}
	if strings.TrimSpace(p.Manifest.Description) == "" {
		badf("PLUGIN.md frontmatter needs a description")
	}
	for _, cname := range slices.Sorted(maps.Keys(p.Manifest.Credentials)) {
		c := p.Manifest.Credentials[cname]
		switch {
		case !creds.NamePattern.MatchString(cname):
			badf("credential name %q must be lowercase snake_case", cname)
		case strings.HasPrefix(cname, creds.ReservedPrefix):
			badf("credential name %q collides with the reserved %s_* prefix", cname, strings.ToUpper(creds.ReservedPrefix))
		case creds.ReservedEnvName(cname):
			badf("credential name %q would shadow the %s environment variable in the run", cname, strings.ToUpper(cname))
		}
		if strings.TrimSpace(c.Description) == "" {
			badf("credential %q needs a description -- someone has to know what to fill in", cname)
		}
		if c.Type != "" && !creds.KnownType(c.Type) {
			badf("credential %q has unknown type %q (supported: %s)", cname, c.Type, strings.Join(creds.DerivedTypes, ", "))
		}
	}
	for _, vname := range slices.Sorted(maps.Keys(p.Manifest.Variables)) {
		_, collidesWithCredential := p.Manifest.Credentials[vname]
		switch {
		case !creds.NamePattern.MatchString(vname):
			badf("variable name %q must be lowercase snake_case", vname)
		case strings.HasPrefix(vname, creds.ReservedPrefix):
			badf("variable name %q collides with the reserved %s_* prefix", vname, strings.ToUpper(creds.ReservedPrefix))
		case creds.ReservedEnvName(vname):
			badf("variable name %q would shadow the %s environment variable in the run", vname, strings.ToUpper(vname))
		case collidesWithCredential:
			badf("variable %q collides with a credential declared by the plugin", vname)
		}
		if strings.TrimSpace(p.Manifest.Variables[vname].Description) == "" {
			badf("variable %q needs a description", vname)
		}
	}
	for _, mname := range slices.Sorted(maps.Keys(p.Manifest.MCP)) {
		m := p.Manifest.MCP[mname]
		// Server names become opencode tool prefixes ("<name>_*") and land
		// in a person's opencode.json -- same grammar as credentials.
		if !creds.NamePattern.MatchString(mname) {
			badf("mcp server name %q must be lowercase snake_case", mname)
		}
		if strings.TrimSpace(m.Description) == "" {
			badf("mcp server %q needs a description -- someone has to know what they are connecting", mname)
		}
		if strings.TrimSpace(m.URL) == "" {
			badf("mcp server %q needs a url -- the declaration is what the person reviews and pastes into opencode.json", mname)
		}
		if m.Credential != "" {
			if _, declared := p.Manifest.Credentials[m.Credential]; !declared {
				badf("mcp server %q names credential %q, missing from the PLUGIN.md credentials block", mname, m.Credential)
			}
		}
	}

	// Walk the whole payload: everything must be classifiable, and violation
	// is refusal. Symlinks and other irregular files are refused everywhere.
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		rel, _ := filepath.Rel(dir, path)
		if rel == "." {
			return nil
		}
		if rel == ".git" || rel == ".github" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == ".git" {
			badf("%s: nested .git metadata is refused", rel)
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.IsDir() && !d.Type().IsRegular() {
			badf("%s: not a regular file -- symlinks and devices are refused", rel)
			return nil
		}
		if reason, bad := forbidden[rel]; bad {
			badf("%s: refused -- %s", rel, reason)
			return nil
		}
		switch {
		case rel == FileName || benignRoot[rel]:
		case rel == "routines" || rel == "skills" || rel == "knowledge" || rel == filepath.Join("knowledge", "ledgers"):
		case strings.HasPrefix(rel, "routines"+string(filepath.Separator)):
			if d.IsDir() {
				badf("%s: routines/ holds flat markdown files only", rel)
				return filepath.SkipDir
			}
			base := strings.TrimSuffix(d.Name(), ".md")
			if !strings.HasSuffix(d.Name(), ".md") || !routine.NamePattern.MatchString(base) {
				badf("%s: routine files are <name>.md with a lowercase name", rel)
			}
		case strings.HasPrefix(rel, "skills"+string(filepath.Separator)):
			parts := strings.Split(rel, string(filepath.Separator))
			if !skill.NamePattern.MatchString(parts[1]) {
				badf("skills/%s: not a valid Agent Skills directory name", parts[1])
				if d.IsDir() {
					return filepath.SkipDir
				}
			}
		case strings.HasPrefix(rel, filepath.Join("knowledge", "ledgers")+string(filepath.Separator)):
			base := strings.TrimSuffix(d.Name(), ".md")
			if d.IsDir() || !strings.HasSuffix(d.Name(), ".md") || !routine.NamePattern.MatchString(base) {
				badf("%s: ledger stubs are flat knowledge/ledgers/<routine>.md files", rel)
			} else {
				p.Stubs = append(p.Stubs, rel)
			}
		case strings.HasPrefix(rel, "knowledge"+string(filepath.Separator)):
			badf("%s: plugins may seed only knowledge/ledgers/ stubs, never shared knowledge files", rel)
		default:
			badf("%s: not part of a plugin -- the payload is allow-listed (PLUGIN.md, routines/, skills/, knowledge/ledgers/)", rel)
			if d.IsDir() {
				return filepath.SkipDir
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	routines, parseErrs := routine.LoadDir(filepath.Join(dir, "routines"))
	p.Routines = routines
	for _, e := range parseErrs {
		// Every other finding names the file relative to the payload; a load
		// error carries an absolute path into the clone, which says nothing
		// to the person reviewing the plugin.
		var re *routine.Error
		if errors.As(e, &re) && re.Path != "" {
			rel, relErr := filepath.Rel(dir, re.Path)
			if relErr == nil {
				badf("%s: %v", rel, re.Err)
				continue
			}
		}
		badf("%v", e)
	}
	skills, skillErrs := skill.List(filepath.Join(dir, "skills"))
	p.Skills = skills
	for _, e := range skillErrs {
		badf("%v", e)
	}

	if len(p.Routines) == 0 && len(p.Skills) == 0 && len(p.Stubs) == 0 {
		badf("nothing to install: a plugin bundles at least one routine, skill, or ledger stub")
	}

	// Internal consistency: no dangling grants, and the manifest tells the
	// whole credential story the grant summary will print.
	shipped := map[string]bool{}
	for _, s := range p.Skills {
		shipped[s.Name] = true
	}
	for _, r := range p.Routines {
		for _, sk := range r.FM.Skills {
			if !shipped[sk] && !agentSkills[sk] {
				badf("routine %s declares skill %q, which neither the plugin nor the agent has", r.Name, sk)
			}
		}
		for _, c := range r.FM.Credentials {
			if _, declared := p.Manifest.Credentials[c]; !declared {
				badf("routine %s declares credential %q, missing from the PLUGIN.md credentials block", r.Name, c)
			}
		}
		// Strict like credentials, not lenient like skills: the manifest
		// tells the whole story the grant summary prints, so a grant on an
		// agent-defined server still needs the plugin's own declaration.
		for _, m := range r.FM.MCP {
			if _, declared := p.Manifest.MCP[m]; !declared {
				badf("routine %s grants mcp server %q, missing from the PLUGIN.md mcp block", r.Name, m)
			}
		}
	}

	if len(problems) != 0 {
		return nil, errors.New("invalid plugin:\n  " + strings.Join(problems, "\n  "))
	}
	return p, nil
}

// parseManifestFile splits PLUGIN.md into frontmatter and body.
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

// agentNamespace returns the installed plugin routine namespace and the
// global skill namespace, or an error if the agent does not load cleanly.
// Agent-owned routines deliberately do not participate in plugin collisions:
// they shadow same-named plugin routines. Plugin routines are loaded directly
// so a shadowed routine still prevents a second plugin from claiming its name.
func agentNamespace(agentDir string) ([]*routine.Routine, []*skill.Skill, error) {
	_, agentRoutineErrs := routine.LoadDir(filepath.Join(agentDir, "routines"))
	routines, pluginRoutineErrs := routine.LoadPlugins(agentDir)
	skills, skillErrs := skill.ListAgent(agentDir)
	if errs := slices.Concat(agentRoutineErrs, pluginRoutineErrs, skillErrs); len(errs) > 0 {
		return nil, nil, errors.Join(errs...)
	}
	return routines, skills, nil
}

// Summary renders the grant summary: every authority the bundle asks for,
// stated before anything is copied. Review is the only gate, so this is it.
func (p *Plugin) Summary() string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }

	if len(p.Routines) > 0 {
		w("Routines:\n")
		for _, r := range p.Routines {
			w("  %s", r.Name)
			if r.FM.Schedule != "" {
				w("  schedule %q", r.FM.Schedule)
			}
			if r.FM.Model != "" {
				w("  model %s", r.FM.Model)
			}
			if r.FM.Reports {
				w("  reports")
			}
			w("\n")
			if len(r.FM.Credentials) > 0 {
				w("    credentials: %s\n", strings.Join(r.FM.Credentials, ", "))
			}
			if len(r.FM.Skills) > 0 {
				w("    skills: %s\n", strings.Join(r.FM.Skills, ", "))
			}
			if len(r.FM.MCP) > 0 {
				w("    mcp: %s\n", strings.Join(r.FM.MCP, ", "))
			}
			// Web access is a grant like any other, and the one a reviewer of an
			// unfamiliar bundle most wants named: it is how untrusted text gets in.
			var web []string
			if r.FM.Webfetch {
				web = append(web, "webfetch")
			}
			if r.FM.Websearch {
				web = append(web, "websearch")
			}
			if len(web) > 0 {
				w("    web access: %s\n", strings.Join(web, ", "))
			}
			if t := r.FM.Trigger; t != nil {
				w("    trigger: polls %s", t.Poll)
				if t.Credential != "" {
					w(" with credential %s as a bearer token", t.Credential)
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

// ReadSource strictly decodes an installed plugin's provenance.
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

// validate reports whether provenance is usable: a repository, a full commit
// hash, and a path that stays inside the source repository. Both ReadSource
// and PrepareInstall enforce it, so the recorded identity means the same
// thing whether it was just written or read back later.
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
