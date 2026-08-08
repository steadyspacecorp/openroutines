package plugin

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/steadyspacecorp/openroutines/internal/knowledge"
	"github.com/steadyspacecorp/openroutines/internal/routine"
	"github.com/steadyspacecorp/openroutines/internal/skill"
)

// Install is a validated install: the bundle, the agent it targets, and the
// provenance to record. Apply is reachable only through PrepareInstall, so
// nothing can be copied past a failed check.
type Install struct {
	Plugin *Plugin

	agentDir string
	source   Source
}

// PrepareInstall validates everything an install depends on: the provenance
// to record, the whole payload at bundleDir, and the agent's namespace.
// Install refuses to replace anything -- an existing path is the user's.
func PrepareInstall(agentDir, bundleDir string, source Source) (*Install, error) {
	if err := source.validate(); err != nil {
		return nil, err
	}
	routines, skills, err := agentNamespace(agentDir)
	if err != nil {
		return nil, fmt.Errorf("agent routines and skills must be valid before installing: %w", err)
	}
	agentSkills := map[string]bool{}
	for _, s := range skills {
		agentSkills[s.Name] = true
	}
	p, err := Load(bundleDir, agentSkills)
	if err != nil {
		return nil, err
	}
	var taken []string
	if _, err := os.Stat(filepath.Join(agentDir, ".openroutines", "plugins", p.Manifest.Name)); err == nil {
		taken = append(taken, filepath.Join(".openroutines", "plugins", p.Manifest.Name))
	}
	taken = append(taken, p.collisions(routines, skills, "")...)
	if len(taken) > 0 {
		return nil, fmt.Errorf("already present, refusing to replace: %s -- remove them first to reinstall", strings.Join(taken, ", "))
	}
	return &Install{Plugin: p, agentDir: agentDir, source: source}, nil
}

// Reports the bundle's routine and skill names already taken in
// the agent namespace. Content under ownDir is the bundle's own installed
// copy, not a collision; ownDir is empty for a fresh install.
func (p *Plugin) collisions(routines []*routine.Routine, skills []*skill.Skill, ownDir string) []string {
	own := func(path string) bool {
		if ownDir == "" {
			return false
		}
		clean := filepath.Clean(path)
		return clean == filepath.Clean(ownDir) || strings.HasPrefix(clean, filepath.Clean(ownDir)+string(filepath.Separator))
	}
	routineNames := map[string]bool{}
	for _, r := range routines {
		if !own(r.Path) {
			routineNames[r.Name] = true
		}
	}
	skillNames := map[string]bool{}
	for _, s := range skills {
		if !own(s.Dir) {
			skillNames[s.Name] = true
		}
	}
	var taken []string
	for _, r := range p.Routines {
		if routineNames[r.Name] {
			taken = append(taken, "routine "+r.Name)
		}
	}
	for _, s := range p.Skills {
		if skillNames[s.Name] {
			taken = append(taken, "skill "+s.Name)
		}
	}
	return taken
}

// Apply copies the bundle into the agent: routines and skills always; ledger
// stubs only when the knowledge worktree already exists (the supervisor
// creates it on first run), otherwise they're returned as pending.
func (i *Install) Apply() (installed, pendingStubs []string, err error) {
	p := i.Plugin
	destRoot := filepath.Join(i.agentDir, ".openroutines", "plugins", p.Manifest.Name)
	if err := copyTreeExclusive(p.Dir, destRoot); err != nil {
		return nil, nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(destRoot)
			installed = nil
		}
	}()
	for _, r := range p.Routines {
		rel := filepath.Join("routines", r.Name+".md")
		dest := filepath.Join(destRoot, rel)
		raw, readErr := os.ReadFile(dest)
		if readErr != nil {
			return nil, nil, readErr
		}
		raw, readErr = routine.WithActive(raw, false)
		if readErr != nil {
			return nil, nil, fmt.Errorf("%s: %w", rel, readErr)
		}
		if err := os.WriteFile(dest, raw, 0o644); err != nil {
			return nil, nil, err
		}
	}
	sourceRaw, err := yaml.Marshal(i.source)
	if err != nil {
		return nil, nil, err
	}
	if err := writeFileExclusive(filepath.Join(destRoot, SourceFileName), sourceRaw); err != nil {
		return nil, nil, err
	}
	installed = append(installed, filepath.Join(".openroutines", "plugins", p.Manifest.Name))
	wt := knowledge.At(i.agentDir).Worktree()
	haveWorktree := false
	if fi, err := os.Stat(wt); err == nil && fi.IsDir() {
		haveWorktree = true
	}
	for _, stub := range p.Stubs {
		if !haveWorktree {
			pendingStubs = append(pendingStubs, stub)
			continue
		}
		dest := filepath.Join(wt, strings.TrimPrefix(stub, "knowledge"+string(filepath.Separator)))
		if _, err := os.Stat(dest); err == nil {
			pendingStubs = append(pendingStubs, stub) // never clobber live knowledge
			continue
		}
		if err := copyFile(filepath.Join(p.Dir, stub), dest); err != nil {
			return installed, pendingStubs, err
		}
		installed = append(installed, stub)
	}
	ok = true
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

// Re-enforces the payload boundary during the copy itself, since
// validation and copy can be separated in time for a local dev source.
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
		if rel == ".git" || rel == ".github" {
			if d.IsDir() {
				return filepath.SkipDir
			}
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
