package plugin

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/steadyspacecorp/openroutines/internal/routine"
	"github.com/steadyspacecorp/openroutines/internal/skill"
)

// A prepared update: the recorded and current upstream revisions,
// what changed between them, and the validated upstream bundle. Apply is
// reachable only through PrepareUpdate, so the merge cannot run past a
// failed check.
type Update struct {
	Old, New Source
	Changes  []string // upstream additions, modifications, and removals
	Upstream *Plugin  // unset when Current

	name         string
	agentDir     string
	base, theirs string
	agentSkills  map[string]bool
	cleanups     []func()
}

// Reports that the installed plugin already matches upstream; a
// current Update carries no Changes or Upstream and has nothing to Apply.
func (u *Update) Current() bool { return u.New.Revision == u.Old.Revision }

// Releases the temporary clones.
func (u *Update) Close() {
	for _, f := range u.cleanups {
		f()
	}
}

// Fetches the plugin's current upstream and its recorded
// revision, then validates everything the update depends on: the upstream
// payload, its name, and collisions with agent content outside the plugin
// itself.
func PrepareUpdate(agentDir, name string) (*Update, error) {
	if !skill.NamePattern.MatchString(name) {
		return nil, fmt.Errorf("invalid plugin name %q", name)
	}
	oldSource, err := ReadSource(filepath.Join(agentDir, ".openroutines", "plugins", name))
	if err != nil {
		return nil, fmt.Errorf("plugin %s: %w", name, err)
	}
	u := &Update{Old: oldSource, name: name, agentDir: agentDir}
	ok := false
	defer func() {
		if !ok {
			u.Close()
		}
	}()
	var cleanup func()
	u.theirs, u.New, cleanup, err = Fetch(oldSource.Repository, oldSource.Path, "")
	if err != nil {
		return nil, fmt.Errorf("load upstream: %w", err)
	}
	u.cleanups = append(u.cleanups, cleanup)
	if u.Current() {
		ok = true
		return u, nil
	}
	u.base, _, cleanup, err = Fetch(oldSource.Repository, oldSource.Path, oldSource.Revision)
	if err != nil {
		return nil, fmt.Errorf("load recorded revision: %w", err)
	}
	u.cleanups = append(u.cleanups, cleanup)
	routines, skills, err := agentNamespace(agentDir)
	if err != nil {
		return nil, fmt.Errorf("agent routines and skills must be valid before updating: %w", err)
	}
	u.agentSkills = map[string]bool{}
	for _, s := range skills {
		u.agentSkills[s.Name] = true
	}
	upstream, err := Load(u.theirs, u.agentSkills)
	if err != nil {
		return nil, fmt.Errorf("new upstream revision is invalid: %w", err)
	}
	if upstream.Manifest.Name != name {
		return nil, fmt.Errorf("upstream plugin name changed from %q to %q", name, upstream.Manifest.Name)
	}
	if taken := upstream.collisions(routines, skills, filepath.Join(agentDir, ".openroutines", "plugins", name)); len(taken) > 0 {
		return nil, fmt.Errorf("updated plugin collides with agent content: %s", strings.Join(taken, ", "))
	}
	u.Changes, err = changes(u.base, u.theirs)
	if err != nil {
		return nil, err
	}
	u.Upstream = upstream
	ok = true
	return u, nil
}

// Merges local edits, the recorded base, and upstream file-wise into a
// staging directory, deactivates routines new since the base, revalidates
// the merged bundle, and swaps it in atomically. Provenance advances only on
// a conflict-free merge, so conflicted files keep their markers and rerunning
// the update converges.
func (u *Update) Apply() (conflicts []string, err error) {
	ours := filepath.Join(u.agentDir, ".openroutines", "plugins", u.name)
	merged, err := os.MkdirTemp(u.agentDir, ".openroutines-plugin-update-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(merged)
	conflicts, err = mergeTrees(u.base, ours, u.theirs, merged)
	if err != nil {
		return nil, err
	}
	basePlugin, err := Load(u.base, u.agentSkills)
	if err != nil {
		return nil, fmt.Errorf("recorded upstream revision is invalid: %w", err)
	}
	baseRoutines := map[string]bool{}
	for _, r := range basePlugin.Routines {
		baseRoutines[r.Name] = true
	}
	for _, r := range u.Upstream.Routines {
		if baseRoutines[r.Name] {
			continue
		}
		path := filepath.Join(merged, "routines", r.Name+".md")
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		raw, err = routine.WithActive(raw, false)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			return nil, err
		}
	}
	sourceToWrite := u.New
	if len(conflicts) > 0 {
		sourceToWrite = u.Old
	}
	if err := writeSource(merged, sourceToWrite); err != nil {
		return nil, err
	}
	if len(conflicts) == 0 {
		mergedPlugin, err := Load(merged, u.agentSkills)
		if err != nil {
			return nil, fmt.Errorf("merged plugin is invalid: %w", err)
		}
		routines, skills, err := agentNamespace(u.agentDir)
		if err != nil {
			return nil, fmt.Errorf("agent routines and skills must be valid before updating: %w", err)
		}
		if taken := mergedPlugin.collisions(routines, skills, ours); len(taken) > 0 {
			return nil, fmt.Errorf("merged plugin collides with agent content: %s", strings.Join(taken, ", "))
		}
	}
	backup := ours + ".update-backup"
	if err := os.Rename(ours, backup); err != nil {
		return nil, err
	}
	if err := os.Rename(merged, ours); err != nil {
		_ = os.Rename(backup, ours)
		return nil, err
	}
	if err := os.RemoveAll(backup); err != nil {
		return nil, err
	}
	return conflicts, nil
}

func writeSource(dir string, source Source) error {
	raw, err := yaml.Marshal(source)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, SourceFileName), raw, 0o644)
}

func changes(base, next string) ([]string, error) {
	files := map[string]bool{}
	for _, root := range []string{base, next} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			if rel == "." || rel == SourceFileName {
				return nil
			}
			if rel == ".git" || rel == ".github" {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if !d.IsDir() {
				files[rel] = true
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	var out []string
	for _, rel := range slices.Sorted(maps.Keys(files)) {
		before, beforeOK, err := readOptional(filepath.Join(base, rel))
		if err != nil {
			return nil, err
		}
		after, afterOK, err := readOptional(filepath.Join(next, rel))
		if err != nil {
			return nil, err
		}
		switch {
		case !beforeOK && afterOK:
			out = append(out, "A "+rel)
		case beforeOK && !afterOK:
			out = append(out, "D "+rel)
		case !bytes.Equal(before, after):
			out = append(out, "M "+rel)
		}
	}
	return out, nil
}

// Performs a file-wise three-way merge into dest. Intentionally
// excludes provenance: Apply advances that only after a clean merge.
func mergeTrees(base, ours, theirs, dest string) ([]string, error) {
	files := map[string]bool{}
	for _, root := range []string{base, ours, theirs} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			if rel == "." || rel == SourceFileName {
				return nil
			}
			if rel == ".git" || rel == ".github" {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				return nil
			}
			if !d.Type().IsRegular() {
				return fmt.Errorf("%s: not a regular file", path)
			}
			files[rel] = true
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	var conflicts []string
	for _, rel := range slices.Sorted(maps.Keys(files)) {
		baseRaw, baseOK, err := readOptional(filepath.Join(base, rel))
		if err != nil {
			return nil, err
		}
		oursRaw, oursOK, err := readOptional(filepath.Join(ours, rel))
		if err != nil {
			return nil, err
		}
		theirsRaw, theirsOK, err := readOptional(filepath.Join(theirs, rel))
		if err != nil {
			return nil, err
		}
		var out []byte
		write := true
		switch {
		case oursOK == baseOK && bytes.Equal(oursRaw, baseRaw):
			out, write = theirsRaw, theirsOK
		case theirsOK == baseOK && bytes.Equal(theirsRaw, baseRaw):
			out, write = oursRaw, oursOK
		case oursOK && theirsOK && bytes.Equal(oursRaw, theirsRaw):
			out = oursRaw
		default:
			var conflict bool
			out, conflict, err = mergeFile(oursRaw, baseRaw, theirsRaw)
			if err != nil {
				return nil, fmt.Errorf("merge %s: %w", rel, err)
			}
			if conflict {
				conflicts = append(conflicts, rel)
			}
		}
		if !write {
			continue
		}
		target := filepath.Join(dest, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(target, out, 0o644); err != nil {
			return nil, err
		}
	}
	return conflicts, nil
}

func readOptional(path string) ([]byte, bool, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	return raw, err == nil, err
}

func mergeFile(ours, base, theirs []byte) ([]byte, bool, error) {
	tmp, err := os.MkdirTemp("", "openroutines-plugin-merge-*")
	if err != nil {
		return nil, false, err
	}
	defer os.RemoveAll(tmp)
	paths := []string{filepath.Join(tmp, "ours"), filepath.Join(tmp, "base"), filepath.Join(tmp, "theirs")}
	for i, raw := range [][]byte{ours, base, theirs} {
		if err := os.WriteFile(paths[i], raw, 0o644); err != nil {
			return nil, false, err
		}
	}
	cmd := exec.Command("git", "merge-file", "-p", "-L", "local", "-L", "upstream base", "-L", "upstream", paths[0], paths[1], paths[2])
	out, runErr := cmd.Output()
	var exit *exec.ExitError
	if errors.As(runErr, &exit) && exit.ExitCode() == 1 {
		return out, true, nil
	}
	if runErr != nil {
		return nil, false, runErr
	}
	return out, false, nil
}
