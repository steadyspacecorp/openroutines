package plugin

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/steadyspacecorp/openroutines/internal/creds"
	"github.com/steadyspacecorp/openroutines/internal/routine"
	"github.com/steadyspacecorp/openroutines/internal/skill"
)

type pluginValidator struct {
	plugin      *Plugin
	agentSkills map[string]bool
	problems    []string
}

func (v *pluginValidator) badf(format string, args ...any) {
	v.problems = append(v.problems, fmt.Sprintf(format, args...))
}

func (v *pluginValidator) err() error {
	if len(v.problems) == 0 {
		return nil
	}
	return errors.New("invalid plugin:\n  " + strings.Join(v.problems, "\n  "))
}

func (v *pluginValidator) validateManifest() {
	manifest := v.plugin.Manifest
	if !skill.NamePattern.MatchString(manifest.Name) {
		v.badf("plugin name %q must be a bare lowercase-hyphen name (steady, github-docs)", manifest.Name)
	}
	if strings.TrimSpace(manifest.Description) == "" {
		v.badf("PLUGIN.md frontmatter needs a description")
	}
	for _, name := range slices.Sorted(maps.Keys(manifest.Credentials)) {
		credential := manifest.Credentials[name]
		switch {
		case !creds.NamePattern.MatchString(name):
			v.badf("credential name %q must be lowercase snake_case", name)
		case strings.HasPrefix(name, creds.ReservedPrefix):
			v.badf("credential name %q collides with the reserved %s_* prefix", name, strings.ToUpper(creds.ReservedPrefix))
		case creds.ReservedEnvName(name):
			v.badf("credential name %q would shadow the %s environment variable in the run", name, strings.ToUpper(name))
		}
		if strings.TrimSpace(credential.Description) == "" {
			v.badf("credential %q needs a description -- someone has to know what to fill in", name)
		}
		if credential.Type != "" && !creds.KnownType(credential.Type) {
			v.badf("credential %q has unknown type %q (supported: %s)", name, credential.Type, strings.Join(creds.DerivedTypes, ", "))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(manifest.Variables)) {
		_, collidesWithCredential := manifest.Credentials[name]
		switch {
		case !creds.NamePattern.MatchString(name):
			v.badf("variable name %q must be lowercase snake_case", name)
		case strings.HasPrefix(name, creds.ReservedPrefix):
			v.badf("variable name %q collides with the reserved %s_* prefix", name, strings.ToUpper(creds.ReservedPrefix))
		case creds.ReservedEnvName(name):
			v.badf("variable name %q would shadow the %s environment variable in the run", name, strings.ToUpper(name))
		case collidesWithCredential:
			v.badf("variable %q collides with a credential declared by the plugin", name)
		}
		if strings.TrimSpace(manifest.Variables[name].Description) == "" {
			v.badf("variable %q needs a description", name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(manifest.MCP)) {
		server := manifest.MCP[name]
		if !creds.NamePattern.MatchString(name) {
			v.badf("mcp server name %q must be lowercase snake_case", name)
		}
		if strings.TrimSpace(server.Description) == "" {
			v.badf("mcp server %q needs a description -- someone has to know what they are connecting", name)
		}
		if strings.TrimSpace(server.URL) == "" {
			v.badf("mcp server %q needs a url -- the declaration is what the person reviews and pastes into opencode.json", name)
		}
		if server.Credential != "" {
			if _, declared := manifest.Credentials[server.Credential]; !declared {
				v.badf("mcp server %q names credential %q, missing from the PLUGIN.md credentials block", name, server.Credential)
			}
		}
	}
}

func (v *pluginValidator) validatePayload() error {
	dir := v.plugin.Dir
	return filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, _ := filepath.Rel(dir, path)
		if rel == "." {
			return nil
		}
		if rel == ".git" || rel == ".github" {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() == ".git" {
			v.badf("%s: nested .git metadata is refused", rel)
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.IsDir() && !entry.Type().IsRegular() {
			v.badf("%s: not a regular file -- symlinks and devices are refused", rel)
			return nil
		}
		if reason, bad := forbidden[rel]; bad {
			v.badf("%s: refused -- %s", rel, reason)
			return nil
		}
		return v.validatePayloadEntry(rel, entry)
	})
}

func (v *pluginValidator) validatePayloadEntry(rel string, entry fs.DirEntry) error {
	switch {
	case rel == FileName || benignRoot[rel]:
	case rel == "routines" || rel == "skills" || rel == "knowledge" || rel == filepath.Join("knowledge", "ledgers"):
	case strings.HasPrefix(rel, "routines"+string(filepath.Separator)):
		if entry.IsDir() {
			v.badf("%s: routines/ holds flat markdown files only", rel)
			return filepath.SkipDir
		}
		base := strings.TrimSuffix(entry.Name(), ".md")
		if !strings.HasSuffix(entry.Name(), ".md") || !routine.NamePattern.MatchString(base) {
			v.badf("%s: routine files are <name>.md with a lowercase name", rel)
		}
	case strings.HasPrefix(rel, "skills"+string(filepath.Separator)):
		parts := strings.Split(rel, string(filepath.Separator))
		if !skill.NamePattern.MatchString(parts[1]) {
			v.badf("skills/%s: not a valid Agent Skills directory name", parts[1])
			if entry.IsDir() {
				return filepath.SkipDir
			}
		}
	case strings.HasPrefix(rel, filepath.Join("knowledge", "ledgers")+string(filepath.Separator)):
		base := strings.TrimSuffix(entry.Name(), ".md")
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") || !routine.NamePattern.MatchString(base) {
			v.badf("%s: ledger stubs are flat knowledge/ledgers/<routine>.md files", rel)
		} else {
			v.plugin.Stubs = append(v.plugin.Stubs, rel)
		}
	case strings.HasPrefix(rel, "knowledge"+string(filepath.Separator)):
		v.badf("%s: plugins may seed only knowledge/ledgers/ stubs, never shared knowledge files", rel)
	default:
		v.badf("%s: not part of a plugin -- the payload is allow-listed (PLUGIN.md, routines/, skills/, knowledge/ledgers/)", rel)
		if entry.IsDir() {
			return filepath.SkipDir
		}
	}
	return nil
}

func (v *pluginValidator) loadContents() {
	dir := v.plugin.Dir
	var routineErrs []error
	v.plugin.Routines, routineErrs = routine.LoadDir(filepath.Join(dir, "routines"))
	for _, err := range routineErrs {
		var routineErr *routine.Error
		if errors.As(err, &routineErr) && routineErr.Path != "" {
			if rel, relErr := filepath.Rel(dir, routineErr.Path); relErr == nil {
				v.badf("%s: %v", rel, routineErr.Err)
				continue
			}
		}
		v.badf("%v", err)
	}
	var skillErrs []error
	v.plugin.Skills, skillErrs = skill.List(filepath.Join(dir, "skills"))
	for _, err := range skillErrs {
		v.badf("%v", err)
	}
}

func (v *pluginValidator) validateContents() {
	p := v.plugin
	if len(p.Routines) == 0 && len(p.Skills) == 0 && len(p.Stubs) == 0 {
		v.badf("nothing to install: a plugin bundles at least one routine, skill, or ledger stub")
	}
	shipped := map[string]bool{}
	for _, shippedSkill := range p.Skills {
		shipped[shippedSkill.Name] = true
	}
	for _, pluginRoutine := range p.Routines {
		for _, skillName := range pluginRoutine.Frontmatter.Skills {
			if !shipped[skillName] && !v.agentSkills[skillName] {
				v.badf("routine %s declares skill %q, which neither the plugin nor the agent has", pluginRoutine.Name, skillName)
			}
		}
		for _, credential := range pluginRoutine.Frontmatter.Credentials {
			if _, declared := p.Manifest.Credentials[credential]; !declared {
				v.badf("routine %s declares credential %q, missing from the PLUGIN.md credentials block", pluginRoutine.Name, credential)
			}
		}
		for _, server := range pluginRoutine.Frontmatter.MCP {
			if _, declared := p.Manifest.MCP[server]; !declared {
				v.badf("routine %s grants mcp server %q, missing from the PLUGIN.md mcp block", pluginRoutine.Name, server)
			}
		}
	}
}
