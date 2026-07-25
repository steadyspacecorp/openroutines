// Package plugin reads and installs plugins: copy-in bundles of routines,
// skills, and memory-ledger stubs described by a PLUGIN.md manifest (see
// DESIGN.md "Plugins"). Validation is all-or-nothing over the whole payload
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
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/steadyspacecorp/openroutines/internal/creds"
	"github.com/steadyspacecorp/openroutines/internal/memory"
	"github.com/steadyspacecorp/openroutines/internal/routine"
	"github.com/steadyspacecorp/openroutines/internal/skill"
)

// FileName is the plugin manifest at the bundle root.
const FileName = "PLUGIN.md"

// Credential is one required credential: a description for the person who
// will fill it in, and for typed credentials the derived type to declare in
// agent.yaml. Never a value -- secrets are not part of a plugin.
type Credential struct {
	Description string `yaml:"description"`
	Type        string `yaml:"type,omitempty"`
}

// Variable is one required non-secret configuration value.
type Variable struct {
	Description string `yaml:"description"`
}

// Manifest is PLUGIN.md's frontmatter.
type Manifest struct {
	Name        string                `yaml:"name"`
	Description string                `yaml:"description"`
	Credentials map[string]Credential `yaml:"credentials,omitempty"`
	Variables   map[string]Variable   `yaml:"variables,omitempty"`
}

// Plugin is a validated bundle, ready to summarize and install.
type Plugin struct {
	Manifest Manifest
	Body     string // manifest body: the plugin's README, shown at install
	Dir      string
	Routines []*routine.Routine
	Skills   []*skill.Skill
	Stubs    []string // memory/ledgers/<name>.md, relative to Dir
}

// benignRoot are repository housekeeping files tolerated (never copied) at
// the bundle root, so a standalone plugin repo can exist on a forge.
var benignRoot = map[string]bool{
	"README.md": true, "LICENSE": true, "LICENSE.md": true,
	"LICENSE.txt": true, ".gitignore": true,
}

// forbidden are agent- or harness-owned files a plugin must never ship;
// naming them gets a sharper refusal than the generic allow-list one.
var forbidden = map[string]string{
	"opencode.json":         "opencode.json is the agent's harness config -- a plugin granting itself permissions or endpoints is exactly what the allow-list exists to stop",
	"agent.yaml":            "agent.yaml belongs to the agent; credential metadata and variables are declared in PLUGIN.md and printed as next steps",
	"Dockerfile":            "the Dockerfile is framework-owned",
	".openroutines-version": "the version pin is framework-owned",
	"master.key":            "a plugin must never carry key material",
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
		}
		if strings.TrimSpace(c.Description) == "" {
			badf("credential %q needs a description -- someone has to know what to fill in", cname)
		}
		if c.Type != "" && c.Type != "github_app" {
			badf("credential %q has unknown type %q (supported: github_app)", cname, c.Type)
		}
	}
	for _, vname := range slices.Sorted(maps.Keys(p.Manifest.Variables)) {
		switch {
		case !creds.NamePattern.MatchString(vname):
			badf("variable name %q must be lowercase snake_case", vname)
		case strings.HasPrefix(vname, creds.ReservedPrefix):
			badf("variable name %q collides with the reserved %s_* prefix", vname, strings.ToUpper(creds.ReservedPrefix))
		}
		if strings.TrimSpace(p.Manifest.Variables[vname].Description) == "" {
			badf("variable %q needs a description", vname)
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
		case rel == "routines" || rel == "skills" || rel == "memory" || rel == filepath.Join("memory", "ledgers"):
			// structural directories
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
		case strings.HasPrefix(rel, filepath.Join("memory", "ledgers")+string(filepath.Separator)):
			base := strings.TrimSuffix(d.Name(), ".md")
			if d.IsDir() || !strings.HasSuffix(d.Name(), ".md") || !routine.NamePattern.MatchString(base) {
				badf("%s: ledger stubs are flat memory/ledgers/<routine>.md files", rel)
			} else {
				p.Stubs = append(p.Stubs, rel)
			}
		case strings.HasPrefix(rel, "memory"+string(filepath.Separator)):
			badf("%s: plugins may seed only memory/ledgers/ stubs, never shared memory files", rel)
		default:
			badf("%s: not part of a plugin -- the payload is allow-listed (PLUGIN.md, routines/, skills/, memory/ledgers/)", rel)
			if d.IsDir() {
				return filepath.SkipDir
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Parse the payload's routines and skills.
	routines, parseErrs := routine.LoadDir(filepath.Join(dir, "routines"))
	p.Routines = routines
	for _, e := range parseErrs {
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
		if t := r.FM.Trigger; t != nil && t.Credential != "" {
			if p.Manifest.Credentials[t.Credential].Type != "" {
				badf("routine %s: trigger credential %q is typed -- a poll sends its credential verbatim; use a raw credential", r.Name, t.Credential)
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
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return nil, "", fmt.Errorf("%s: missing frontmatter", FileName)
	}
	rest := text[len("---\n"):]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		return nil, "", fmt.Errorf("%s: unterminated frontmatter", FileName)
	}
	var m Manifest
	dec := yaml.NewDecoder(bytes.NewReader([]byte(rest[:end])))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil && err != io.EOF {
		return nil, "", fmt.Errorf("%s: %w", FileName, err)
	}
	return &m, strings.TrimSpace(rest[end+len("\n---\n"):]), nil
}

// Collisions reports the agent paths this plugin would overwrite. Install
// refuses to replace anything: an existing path is the user's.
func (p *Plugin) Collisions(agentDir string) []string {
	var out []string
	for _, r := range p.Routines {
		rel := filepath.Join("routines", r.Name+".md")
		if _, err := os.Stat(filepath.Join(agentDir, rel)); err == nil {
			out = append(out, rel)
		}
	}
	for _, s := range p.Skills {
		rel := filepath.Join("skills", s.Name)
		if _, err := os.Stat(filepath.Join(agentDir, rel)); err == nil {
			out = append(out, rel)
		}
	}
	return out
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
			if r.FM.Consumes != "" {
				w("  consumes %s", r.FM.Consumes)
			}
			w("\n")
			if len(r.FM.Credentials) > 0 {
				w("    credentials: %s\n", strings.Join(r.FM.Credentials, ", "))
			}
			if len(r.FM.Skills) > 0 {
				w("    skills: %s\n", strings.Join(r.FM.Skills, ", "))
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
			w(" (typed: %s -- needs an agent.yaml credentials entry)", c.Type)
		}
		w("\n")
	}
	for _, name := range slices.Sorted(maps.Keys(p.Manifest.Variables)) {
		w("Variable to set: %s -- %s\n", name, p.Manifest.Variables[name].Description)
	}
	for _, s := range p.Stubs {
		w("Ledger stub: %s\n", s)
	}
	return b.String()
}

// Install copies the bundle into the agent: routines and skills always;
// ledger stubs only when the memory worktree already exists (the supervisor
// creates it on first run), otherwise they are returned as pending for the
// caller to surface. Collisions must be checked before calling.
func (p *Plugin) Install(agentDir string) (installed, pendingStubs []string, err error) {
	var created []string
	rollback := func() {
		for i := len(created) - 1; i >= 0; i-- {
			_ = os.RemoveAll(created[i])
		}
		installed = nil
	}
	for _, r := range p.Routines {
		rel := filepath.Join("routines", r.Name+".md")
		src := filepath.Join(p.Dir, rel)
		raw, readErr := os.ReadFile(src)
		if readErr != nil {
			rollback()
			return nil, nil, readErr
		}
		raw, readErr = routine.WithActive(raw, false)
		if readErr != nil {
			rollback()
			return nil, nil, fmt.Errorf("%s: %w", rel, readErr)
		}
		dest := filepath.Join(agentDir, rel)
		if err := writeFileExclusive(dest, raw); err != nil {
			rollback()
			return installed, nil, err
		}
		created = append(created, dest)
		installed = append(installed, rel)
	}
	for _, s := range p.Skills {
		rel := filepath.Join("skills", s.Name)
		dest := filepath.Join(agentDir, rel)
		if err := copyTreeExclusive(filepath.Join(p.Dir, rel), dest); err != nil {
			rollback()
			return installed, nil, err
		}
		created = append(created, dest)
		installed = append(installed, rel)
	}
	wt := memory.WorktreePath(agentDir)
	haveWorktree := false
	if fi, err := os.Stat(wt); err == nil && fi.IsDir() {
		haveWorktree = true
	}
	for _, stub := range p.Stubs {
		if !haveWorktree {
			pendingStubs = append(pendingStubs, stub)
			continue
		}
		dest := filepath.Join(wt, strings.TrimPrefix(stub, "memory"+string(filepath.Separator)))
		if _, err := os.Stat(dest); err == nil {
			pendingStubs = append(pendingStubs, stub) // never clobber live memory
			continue
		}
		if err := copyFile(filepath.Join(p.Dir, stub), dest); err != nil {
			rollback()
			return installed, pendingStubs, err
		}
		created = append(created, dest)
		installed = append(installed, stub)
	}
	return installed, pendingStubs, nil
}

func copyFile(src, dest string) error {
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return writeFileExclusive(dest, raw)
}

func writeFileExclusive(dest string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		_ = os.Remove(dest)
		return err
	}
	return f.Close()
}

// copyTreeExclusive independently enforces the payload boundary while copying:
// validation and copy can be separated in time for a local development source.
func copyTreeExclusive(src, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	if err := os.Mkdir(dest, 0o755); err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(dest)
		}
	}()
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		rel, _ := filepath.Rel(src, path)
		if rel == "." {
			return nil
		}
		if d.Name() == ".git" {
			return fmt.Errorf("%s: nested .git metadata is refused", filepath.Join(dest, rel))
		}
		target := filepath.Join(dest, rel)
		if d.IsDir() {
			return os.Mkdir(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s: not a regular file", path)
		}
		return copyFile(path, target)
	})
	if err != nil {
		return err
	}
	ok = true
	return nil
}
