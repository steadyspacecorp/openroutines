package cli

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/steadyspacecorp/openroutines/internal/frontmatter"
	"github.com/steadyspacecorp/openroutines/internal/skill"
	sourcepkg "github.com/steadyspacecorp/openroutines/internal/source"
)

// Vendors a skill from a git repository into skills/<name>, recording
// provenance (source + commit) in the SKILL.md frontmatter. A skill is an
// executable dependency: the user reviews the diff like code.
const skillsAddUsage = "usage: openroutines skills add <git-url | owner/repo> [--path sub/dir]"

func skillsAdd(args []string) int {
	rest, flags, help, err := parseFlags(args, map[string]flagSpec{"--path": {value: true}})
	if err != nil {
		return fail(err)
	}
	if help {
		fmt.Println(skillsAddUsage)
		return 0
	}
	if len(rest) != 1 {
		return fail(fmt.Errorf("%s", skillsAddUsage))
	}
	source := rest[0]
	subPath := flags["--path"]
	root, provenance, cleanup, err := sourcepkg.Fetch(source, subPath, "")
	if err != nil {
		return fail(fmt.Errorf("fetch skill: %w", err))
	}
	defer cleanup()
	skillDir, err := findSkillDir(root)
	if err != nil {
		return fail(err)
	}
	s, err := skill.Parse(filepath.Join(skillDir, "SKILL.md"))
	if err != nil {
		return fail(err)
	}
	if !skill.NamePattern.MatchString(s.Name) {
		return fail(fmt.Errorf("skill name %q in SKILL.md is not a valid Agent Skills name", s.Name))
	}
	dest := filepath.Join("skills", s.Name)
	if existing, err := skill.Find(".", s.Name); err == nil {
		return fail(fmt.Errorf("skill %q already exists at %s -- remove it first to re-vendor", s.Name, existing.Dir))
	}

	// Copy the skill folder (regular files only -- no symlinks, no .git).
	err = filepath.WalkDir(skillDir, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		rel, _ := filepath.Rel(skillDir, path)
		if rel == "." {
			return nil
		}
		if d.Name() == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dest, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, raw, 0o644)
	})
	if err != nil {
		_ = os.RemoveAll(dest)
		return fail(err)
	}

	if err := stampProvenance(filepath.Join(dest, "SKILL.md"), source, provenance.Revision); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not record provenance: %v\n", err)
	}

	fmt.Printf("Vendored %q -> %s (from %s @ %s)\n", s.Name, dest, source, provenance.Revision)
	fmt.Printf("  %s\n\n", firstLine(s.Description))
	fmt.Println("A skill is instructions -- and sometimes code -- your agent will follow")
	fmt.Println("unattended. Review the diff like a dependency before committing, then")
	fmt.Println("grant it to a routine via its `skills:` frontmatter.")
	return 0
}

// Returns the directory containing SKILL.md: root itself, or the one
// SKILL.md found below it.
func findSkillDir(root string) (string, error) {
	if _, err := os.Stat(filepath.Join(root, "SKILL.md")); err == nil {
		return root, nil
	}
	var found []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Name() == ".git" && d.IsDir() {
			return filepath.SkipDir
		}
		if !d.IsDir() && d.Name() == "SKILL.md" {
			found = append(found, filepath.Dir(path))
		}
		return nil
	})
	switch len(found) {
	case 0:
		return "", fmt.Errorf("no SKILL.md found -- is this an Agent Skills repository?")
	case 1:
		return found[0], nil
	default:
		var names []string
		for _, f := range found {
			rel, _ := filepath.Rel(root, f)
			names = append(names, rel)
		}
		return "", fmt.Errorf("multiple skills found (%s) -- pick one with --path", strings.Join(names, ", "))
	}
}

// Records source and revision in the SKILL.md frontmatter's metadata block
// (string key-values, allowed by the Agent Skills spec).
func stampProvenance(path, source, revision string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	doc, err := frontmatter.Split(raw)
	if err != nil {
		return err
	}
	stamp := fmt.Sprintf("%smetadata:%s  source: %q%s  revision: %q", doc.LineEnding(), doc.LineEnding(), source, doc.LineEnding(), revision)
	frontmatter := append([]byte(nil), doc.Frontmatter...)
	frontmatter = append(frontmatter, stamp...)
	return os.WriteFile(path, doc.WithFrontmatter(frontmatter), 0o644)
}
