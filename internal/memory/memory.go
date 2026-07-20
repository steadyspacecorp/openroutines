// Package memory manages the agent's memory: a git worktree of the orphan
// `memory` branch, plus the staging pipeline that keeps model-directed
// processes away from git entirely (see DESIGN.md "Memory syncs per run").
package memory

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	Dir        = "memory"
	Branch     = "memory"
	maxFile    = 5 << 20 // per-file size cap in staged memory
	maxEntries = 2000    // total staged file cap
)

// primitives are the framework-blessed shared memory files, seeded on init.
var primitives = map[string]string{
	"worklog.md":    "# Worklog\n\nRaw facts of what runs accomplished. Append-only; full facts, no polish.\n",
	"intentions.md": "# Intentions\n\nOpen items the agent means to act on, and items waiting on a human.\n",
	"blockers.md":   "# Blockers\n\nImpediments: failures, expired credentials, things needing help.\n",
}

// supervisorOwned paths never travel into staging and are never touched by
// import: routines cannot read or rewrite scheduling state or run records.
var supervisorOwned = map[string]bool{
	"state":      true,
	"runs.jsonl": true,
}

// newGitCmd builds a git invocation with hermetic environment: no system or
// global config leaks in.
func newGitCmd(dir string, args []string) *exec.Cmd {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	if sshCommand != "" {
		cmd.Env = append(cmd.Env, "GIT_SSH_COMMAND="+sshCommand)
	}
	return cmd
}

// git runs a git command against the repo with hermetic configuration:
// no system/global config, no hooks, no file-protocol tricks.
func git(dir string, args ...string) (string, error) {
	base := []string{
		"-c", "core.hooksPath=/dev/null",
		"-c", "protocol.file.allow=never",
		"-c", "user.name=openroutines",
		"-c", "user.email=agent@openroutines.dev",
	}
	cmd := newGitCmd(dir, append(base, args...))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// WorktreePath returns the memory worktree location inside the agent repo.
func WorktreePath(repoDir string) string { return filepath.Join(repoDir, Dir) }

// EnsureWorktree materializes memory/ as a worktree of the memory branch,
// creating the orphan branch and seeding the primitives on first use.
// Self-heals: safe to call every run.
func EnsureWorktree(repoDir string) error {
	wt := WorktreePath(repoDir)
	if _, err := os.Stat(filepath.Join(wt, ".git")); err == nil {
		return nil // already materialized
	}
	// A production image carries .git from build time, which may register a
	// worktree whose directory was excluded from the image. Prune stale
	// registrations or the add below fails on first boot.
	_, _ = git(repoDir, "worktree", "prune")
	if _, err := git(repoDir, "show-ref", "--verify", "--quiet", "refs/heads/"+Branch); err != nil {
		// First use: create the orphan branch from an empty tree via plumbing.
		// (worktree add --orphan needs git >= 2.42; this works everywhere.)
		tree, err := gitStdin(repoDir, "", "mktree")
		if err != nil {
			return fmt.Errorf("creating memory branch: %w", err)
		}
		commit, err := git(repoDir, "-c", "user.name=openroutines", "-c", "user.email=agent@openroutines.dev", "commit-tree", tree, "-m", "Memory branch root")
		if err != nil {
			return fmt.Errorf("creating memory branch: %w", err)
		}
		if _, err := git(repoDir, "branch", Branch, commit); err != nil {
			return fmt.Errorf("creating memory branch: %w", err)
		}
	}
	if _, err := git(repoDir, "worktree", "add", wt, Branch); err != nil {
		return err
	}
	// Seed the primitives and the ledgers directory if absent.
	for name, content := range primitives {
		p := filepath.Join(wt, name)
		if _, err := os.Stat(p); os.IsNotExist(err) {
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				return err
			}
		}
	}
	if err := os.MkdirAll(filepath.Join(wt, "ledgers"), 0o755); err != nil {
		return err
	}
	if _, err := git(wt, "add", "-A"); err != nil {
		return err
	}
	if changed, _ := git(wt, "status", "--porcelain"); changed != "" {
		if _, err := git(wt, "commit", "--quiet", "-m", "Seed memory primitives"); err != nil {
			return err
		}
	}
	return nil
}

// Snapshot copies the memory worktree's files into a plain staging directory:
// regular files only, no git metadata. This staged copy is what a routine
// sees and writes as memory/.
func Snapshot(repoDir, stagingDir string) error {
	wt := WorktreePath(repoDir)
	return filepath.WalkDir(wt, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(wt, path)
		if err != nil || rel == "." {
			return err
		}
		if d.Name() == ".git" || supervisorOwned[topSegment(rel)] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		dest := filepath.Join(stagingDir, rel)
		if d.IsDir() {
			return os.MkdirAll(dest, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil // symlinks etc. never travel into staging
		}
		return copyFile(path, dest)
	})
}

func topSegment(rel string) string {
	return strings.Split(rel, string(filepath.Separator))[0]
}

// Validate rejects a staged memory tree that contains anything but regular
// files under sane limits. A rejected tree fails the whole run.
func Validate(stagingDir string) error {
	entries := 0
	return filepath.WalkDir(stagingDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(stagingDir, path)
		if err != nil || rel == "." {
			return err
		}
		name := d.Name()
		if name == ".git" || name == ".gitattributes" || name == ".gitmodules" || name == ".gitignore" {
			return fmt.Errorf("staged memory contains git control file %q -- rejected", rel)
		}
		if supervisorOwned[topSegment(rel)] {
			return fmt.Errorf("staged memory touches supervisor-owned path %q -- rejected", rel)
		}
		if d.IsDir() {
			if strings.Count(rel, string(filepath.Separator)) > 8 {
				return fmt.Errorf("staged memory path %q too deep -- rejected", rel)
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("staged memory contains non-regular file %q -- rejected", rel)
		}
		entries++
		if entries > maxEntries {
			return fmt.Errorf("staged memory exceeds %d files -- rejected", maxEntries)
		}
		if info, err := d.Info(); err == nil && info.Size() > maxFile {
			return fmt.Errorf("staged memory file %q exceeds %d bytes -- rejected", rel, maxFile)
		}
		return nil
	})
}

// Import applies the staged tree to the worktree: copy every staged file in,
// delete worktree files that no longer exist in staging. Caller commits.
func Import(repoDir, stagingDir string) error {
	if err := Validate(stagingDir); err != nil {
		return err
	}
	wt := WorktreePath(repoDir)
	// Copy staged files over the worktree.
	err := filepath.WalkDir(stagingDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(stagingDir, path)
		if rel == "." {
			return nil
		}
		dest := filepath.Join(wt, rel)
		if d.IsDir() {
			return os.MkdirAll(dest, 0o755)
		}
		return copyFile(path, dest)
	})
	if err != nil {
		return err
	}
	// Remove worktree files the routine deleted in staging.
	return filepath.WalkDir(wt, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(wt, path)
		if rel == "." {
			return nil
		}
		if d.Name() == ".git" || supervisorOwned[topSegment(rel)] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if _, err := os.Stat(filepath.Join(stagingDir, rel)); os.IsNotExist(err) {
			return os.Remove(path)
		}
		return nil
	})
}

// Commit records the current worktree state on the memory branch.
func Commit(repoDir, message string) (string, error) {
	wt := WorktreePath(repoDir)
	if _, err := git(wt, "add", "-A"); err != nil {
		return "", err
	}
	if changed, _ := git(wt, "status", "--porcelain"); changed == "" {
		return "", nil // nothing to commit
	}
	if _, err := git(wt, "commit", "--quiet", "-m", message); err != nil {
		return "", err
	}
	return git(wt, "rev-parse", "--short", "HEAD")
}

// AppendBlocker records a supervisor-written blocker: the mechanism for
// failures the model never got to narrate.
func AppendBlocker(repoDir, line string) error {
	p := filepath.Join(WorktreePath(repoDir), "blockers.md")
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "- %s\n", line)
	return err
}

// AppendRunRecord appends one JSONL run record to the supervisor-owned log.
func AppendRunRecord(repoDir, record string) error {
	p := filepath.Join(WorktreePath(repoDir), "runs.jsonl")
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintln(f, record)
	return err
}

// WorktreeStatus reports the memory worktree's state for `openroutines
// status` -- root `git status` never shows memory churn, so this must.
type WorktreeStatus struct {
	Materialized bool
	Uncommitted  int    // files with uncommitted changes (human curation in progress)
	LastCommit   string // subject of the latest memory commit
	Unpushed     int    // commits origin hasn't seen yet
}

// Status inspects the memory worktree; zero value when not yet materialized.
func Status(repoDir string) WorktreeStatus {
	wt := WorktreePath(repoDir)
	if _, err := os.Stat(filepath.Join(wt, ".git")); err != nil {
		return WorktreeStatus{}
	}
	st := WorktreeStatus{Materialized: true}
	if out, err := git(wt, "status", "--porcelain"); err == nil && out != "" {
		st.Uncommitted = len(strings.Split(out, "\n"))
	}
	if out, err := git(wt, "log", "-1", "--format=%h %s"); err == nil {
		st.LastCommit = out
	}
	if out, err := git(wt, "rev-list", "--count", "refs/remotes/origin/"+Branch+"..HEAD"); err == nil {
		fmt.Sscanf(out, "%d", &st.Unpushed)
	}
	return st
}

func copyFile(src, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
