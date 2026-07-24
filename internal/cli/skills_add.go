package cli

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/steadyspacecorp/openroutines/internal/skill"
)

// skillsAdd vendors a skill from a git repository into skills/<name>,
// recording provenance (source + commit) in the SKILL.md frontmatter.
// A skill is an executable dependency: the user reviews the diff like code.
func skillsAdd(args []string) int {
	var source, subPath string
	rest := args[:0:0]
	for i := 0; i < len(args); i++ {
		if args[i] == "--path" && i+1 < len(args) {
			subPath = args[i+1]
			i++
			continue
		}
		rest = append(rest, args[i])
	}
	if len(rest) != 1 {
		return fail(fmt.Errorf("usage: openroutines skills add <git-url | owner/repo> [--path sub/dir]"))
	}
	source = rest[0]
	cloneURL := source
	// GitHub shorthand: owner/repo.
	if !strings.Contains(source, "://") && !strings.Contains(source, "@") && strings.Count(source, "/") == 1 {
		cloneURL = "https://github.com/" + source + ".git"
	}

	tmp, err := os.MkdirTemp("", "openroutines-skill-*")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(tmp)

	// Ambient git on purpose: vendoring happens on a dev machine and may
	// need the user's own auth for private sources. But the URL is pasted
	// input: disable the ext:: command-execution transport and terminate
	// option parsing so a leading-dash "URL" can't become a flag.
	clone := exec.Command("git", "-c", "protocol.ext.allow=never", "clone", "--quiet", "--depth", "1", "--", cloneURL, tmp)
	clone.Stderr = os.Stderr
	if err := clone.Run(); err != nil {
		return fail(fmt.Errorf("clone %s: %w", cloneURL, err))
	}
	revBytes, _ := exec.Command("git", "-C", tmp, "rev-parse", "--short", "HEAD").Output()
	revision := strings.TrimSpace(string(revBytes))

	// Locate the skill: --path if given, SKILL.md at the root, or exactly one
	// SKILL.md anywhere in the repo.
	root := tmp
	if subPath != "" {
		root = filepath.Join(tmp, filepath.Clean(subPath))
	}
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
	if _, err := os.Stat(dest); err == nil {
		return fail(fmt.Errorf("%s already exists -- remove it first to re-vendor", dest))
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

	if err := stampProvenance(filepath.Join(dest, "SKILL.md"), source, revision); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not record provenance: %v\n", err)
	}

	fmt.Printf("Vendored %q -> %s (from %s @ %s)\n", s.Name, dest, source, revision)
	fmt.Printf("  %s\n\n", firstLine(s.Description))
	fmt.Println("A skill is instructions -- and sometimes code -- your agent will follow")
	fmt.Println("unattended. Review the diff like a dependency before committing, then")
	fmt.Println("grant it to a routine via its `skills:` frontmatter.")
	return 0
}

// findSkillDir returns the directory containing the skill's SKILL.md: the
// root itself, or exactly one SKILL.md found below it.
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

// stampProvenance records source and revision in the SKILL.md frontmatter
// metadata block (string key-values, allowed by the Agent Skills spec).
func stampProvenance(path, source, revision string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	text := string(raw)
	if !strings.HasPrefix(text, "---\n") {
		return fmt.Errorf("missing frontmatter")
	}
	end := strings.Index(text[4:], "\n---")
	if end < 0 {
		return fmt.Errorf("unterminated frontmatter")
	}
	fmEnd := 4 + end
	stamp := fmt.Sprintf("\nmetadata:\n  source: %q\n  revision: %q", source, revision)
	return os.WriteFile(path, []byte(text[:fmEnd]+stamp+text[fmEnd:]), 0o644)
}
