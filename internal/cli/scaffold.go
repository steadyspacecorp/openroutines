package cli

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	openroutines "github.com/steadyspacecorp/openroutines"
	"github.com/steadyspacecorp/openroutines/internal/version"
)

const templateRoot = "template"

const scaffoldUsage = "usage: openroutines scaffold <path>"

// cmdScaffold stamps out a new agent repository from the embedded template.
func cmdScaffold(args []string) int {
	positional, _, help, err := parseFlags(args, nil)
	if err != nil {
		return fail(err)
	}
	if help {
		fmt.Println(scaffoldUsage)
		return 0
	}
	if len(positional) != 1 {
		return fail(fmt.Errorf("%s", scaffoldUsage))
	}
	target := positional[0]
	name := filepath.Base(target)

	if entries, err := os.ReadDir(target); err == nil && len(entries) > 0 {
		return fail(fmt.Errorf("%s already exists and is not empty", target))
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return fail(err)
	}

	// Substitutions applied to every templated text file.
	replacer := strings.NewReplacer(
		"{{AGENT_NAME}}", name,
		"{{OPENROUTINES_VERSION}}", version.Version,
	)

	err = fs.WalkDir(openroutines.TemplateFS, templateRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(path, templateRoot)
		rel = strings.TrimPrefix(rel, "/")
		if rel == "" {
			return nil
		}
		dest := filepath.Join(target, rel)
		if d.IsDir() {
			return os.MkdirAll(dest, 0o755)
		}
		raw, err := openroutines.TemplateFS.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dest, []byte(replacer.Replace(string(raw))), 0o644)
	})
	if err != nil {
		return fail(err)
	}

	// Claude Code reads CLAUDE.md, not AGENTS.md; a symlink keeps one source
	// of truth, so update's AGENTS.md rewrites flow through it. Created here
	// because go:embed refuses symlinks, so it cannot live in template/.
	if err := os.Symlink("AGENTS.md", filepath.Join(target, "CLAUDE.md")); err != nil {
		return fail(err)
	}

	// The version pin: this binary's version is what the agent runs against.
	if err := os.WriteFile(filepath.Join(target, ".openroutines-version"), []byte(version.Version+"\n"), 0o644); err != nil {
		return fail(err)
	}

	// A fresh agent is a fresh git repository, with the scaffold as its
	// initial commit -- so configure's changes land as a reviewable diff
	// and git log works from minute one.
	if out, err := exec.Command("git", "init", "--quiet", "--initial-branch=main", target).CombinedOutput(); err != nil {
		return fail(fmt.Errorf("git init: %v: %s", err, out))
	}
	git := func(args ...string) error {
		cmd := exec.Command("git", append([]string{"-C", target}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return nil
	}
	if err := git("add", "-A"); err != nil {
		return fail(err)
	}
	if err := git("-c", "user.name=openroutines", "-c", "user.email=agent@openroutines.dev", "commit", "--quiet", "-m", "Scaffold agent "+name); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not create the initial commit: %v\n", err)
	}

	fmt.Printf("Created agent %q at %s\n\nNext steps:\n  cd %s\n  openroutines configure\n", name, target, target)
	return 0
}
