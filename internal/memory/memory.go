// Package memory manages the agent's memory: a git worktree of the orphan
// `memory` branch, plus the staging pipeline that keeps model-directed
// processes away from git entirely (see design decision "Memory syncs per run").
package memory

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// Memory is a dedicated directory backed by its own branch.
const (
	Dir        = "memory"
	Branch     = "memory"
	maxFile    = 5 << 20 // per-file size cap in staged memory
	maxEntries = 2000    // total staged file cap
)

// primitives are the framework-blessed shared memory files, seeded on init.
// Each opens with a fenced example of its format: the file teaches its own
// shape at the point of use, and the retention trimmer preserves everything
// that isn't a record. Agents may reshape the headers in their own memory
// branch -- seeding is create-if-missing and never overwrites.
var primitives = map[string]string{
	"events.md": "# Events\n\nWhat happened: append-only outcomes and observations, full facts, no polish.\n" +
		"Includes explicit NO-OPs -- a run that found nothing to change still happened.\n\n" +
		"Format (one line per entry):\n\n```markdown\n" +
		"- YYYY-MM-DD <routine>: <what happened, why it matters, links, people>\n" +
		"- YYYY-MM-DD <routine> NO-OP: <what was checked and found clean>\n```\n",
	"tasks.md": "# Tasks\n\nWhat must happen, and who owns the next action. One canonical entry per\n" +
		"task, from discovery to resolution: update it in place (complete, cancel,\n" +
		"or move between sections) rather than re-recording it elsewhere. A blocked\n" +
		"task names what it is waiting on.\n\n" +
		"Format:\n\n```markdown\n" +
		"## Agent-owned\n\n" +
		"- [ ] `task-YYYYMMDD-<n>` <description> (routine: <handler>; source: <where it came from>; added YYYY-MM-DD)\n" +
		"- [x] `task-YYYYMMDD-<n>` <description> (source: ...; added YYYY-MM-DD; done YYYY-MM-DD)\n\n" +
		"## Human-owned\n\n" +
		"- [ ] `task-YYYYMMDD-<n>` <ask> (source: <where it came from>; added YYYY-MM-DD)\n```\n",
	"context.md": "# Context\n\nShared situational awareness: facts that may inform future decisions but\n" +
		"require no action. Refresh a fact when a run reaffirms it; stale entries\n" +
		"age out with retention.\n\n" +
		"Format (one line per entry):\n\n```markdown\n" +
		"- YYYY-MM-DD <routine>: <the fact, and where it came from>\n```\n",
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
		"-c", "protocol.file.allow=user",
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
		// No local branch. A deployed container's .git never has one, but the
		// agent's real memory usually exists on origin: adopt it rather than
		// minting a new root (found live: every container generation was
		// splicing a stray root commit into the lineage).
		if HasOrigin(repoDir) {
			if _, lerr := git(repoDir, "ls-remote", "--exit-code", "origin", "refs/heads/"+Branch); lerr == nil {
				if _, ferr := git(repoDir, "fetch", "--quiet", "origin", "+refs/heads/"+Branch+":refs/heads/"+Branch); ferr != nil {
					return fmt.Errorf("adopting memory branch from origin: %w", ferr)
				}
				// Adoption is where a restart used to launder a rewritten
				// history: a fresh container has no local baseline, so it
				// would take origin's branch wholesale. The accepted ref is
				// the baseline that survives container replacement -- refuse
				// to adopt a tip that does not descend from it. Fail closed,
				// like the sandbox probe: a human repairs and moves the ref.
				if accepted := AcceptedTip(repoDir); accepted != "" {
					tip, terr := git(repoDir, "rev-parse", "refs/heads/"+Branch)
					if terr == nil && tip != accepted && !isAncestor(repoDir, accepted, tip) {
						return fmt.Errorf("origin/%s does not descend from the last accepted tip %s -- memory history was rewritten while this agent was down; refusing to adopt it. Inspect origin/%s, then either restore the branch or move %s to the new tip to accept the rewrite deliberately", Branch, short(accepted), Branch, acceptedRef)
					}
				}
			}
		}
	}
	if _, err := git(repoDir, "show-ref", "--verify", "--quiet", "refs/heads/"+Branch); err != nil {
		// Truly first use: create the orphan branch from an empty tree via
		// plumbing. (worktree add --orphan needs git >= 2.42; this works
		// everywhere.)
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
		if info, err := d.Info(); err == nil {
			if info.Size() > maxFile {
				return fmt.Errorf("staged memory file %q exceeds %d bytes -- rejected", rel, maxFile)
			}
			// A hard link is a regular file that aliases another inode --
			// e.g. a file outside the staging tree, whose content would then
			// travel into the import.
			if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Nlink > 1 {
				return fmt.Errorf("staged memory file %q is a hard link -- rejected", rel)
			}
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
	// Refuse to import over uncommitted human curation: Import overwrites and
	// deletes, and uncommitted edits have no reflog to recover from. Only the
	// human-curated files gate -- supervisor-owned paths (state/, runs.jsonl)
	// legitimately carry this attempt's own in-flight bookkeeping.
	if out, err := git(wt, "status", "--porcelain"); err == nil && out != "" {
		for _, line := range strings.Split(out, "\n") {
			// Field-based parse: git() trims the output, which eats the first
			// line's leading status column. The path is the last field (a
			// rename's "old -> new" resolves to new); a path containing
			// spaces degrades toward refusal, never toward a silent import.
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			path := fields[len(fields)-1]
			if !supervisorOwned[topSegment(path)] {
				return fmt.Errorf("memory worktree has uncommitted changes (%s) -- refusing to import over them; commit or discard (git -C %s ...) and re-run", path, Dir)
			}
		}
	}
	// Copy staged files over the worktree.
	err := filepath.WalkDir(stagingDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(stagingDir, path)
		if rel == "." {
			return nil
		}
		if rel == ConsumeMarker {
			return nil // consume receipt for the runtime, never memory content
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

// RestoreFile puts the worktree's copy of one memory file back into the
// staged tree, undoing whatever the run staged there. The enforcement half
// of `events: false` (design decision "Memory records events, tasks, and
// context"): the instruction tells the routine not to write the file, this
// makes sure. Reports whether a staged change was discarded.
func RestoreFile(repoDir, stagingDir, name string) (bool, error) {
	src := filepath.Join(WorktreePath(repoDir), name)
	dest := filepath.Join(stagingDir, name)
	want, err := os.ReadFile(src)
	if os.IsNotExist(err) {
		// The worktree has no such file: the run must not create it either.
		if _, serr := os.Stat(dest); os.IsNotExist(serr) {
			return false, nil
		}
		return true, os.Remove(dest)
	}
	if err != nil {
		return false, err
	}
	got, err := os.ReadFile(dest)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	if err == nil && bytes.Equal(got, want) {
		return false, nil
	}
	return true, os.WriteFile(dest, want, 0o644)
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

// flatten collapses any whitespace runs -- including the newlines raw git
// and tool errors carry -- into single spaces, so supervisor-written entries
// always honor the one-line-per-entry format of events.md and tasks.md.
func flatten(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// AppendEvent records a supervisor-written event: the mechanism for outcomes
// the model never got to narrate (timeouts, crashes, sync trouble).
func AppendEvent(repoDir, line string) error {
	p := filepath.Join(WorktreePath(repoDir), "events.md")
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "- %s\n", flatten(line))
	return err
}

// AppendHumanTask records a supervisor-created human-owned task: the framework
// giving up on something (abandoned run, tripped breaker, blocked sync) hands
// it to a person. The entry lands at the end of the real "## Human-owned"
// section (fenced format examples don't count), created if missing. Idempotent
// by task id, so restart-prone callers can use deterministic ids.
func AppendHumanTask(repoDir, taskID, description string) error {
	p := filepath.Join(WorktreePath(repoDir), "tasks.md")
	raw, err := os.ReadFile(p)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	text := string(raw)
	if text == "" {
		text = "# Tasks\n"
	}
	if strings.Contains(text, "`"+taskID+"`") {
		return nil // one canonical record per task
	}
	entry := fmt.Sprintf("- [ ] `%s` %s", taskID, flatten(description))
	lines := strings.Split(text, "\n")

	section := -1
	inFence := false
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			inFence = !inFence
			continue
		}
		if !inFence && t == "## Human-owned" {
			section = i
		}
	}
	if section < 0 {
		text = strings.TrimRight(text, "\n") + "\n\n## Human-owned\n\n" + entry + "\n"
		return os.WriteFile(p, []byte(text), 0o644)
	}
	// End of the section: the next unfenced heading, else EOF; insert before
	// the section's trailing blank lines.
	end := len(lines)
	inFence = false
	for i := section + 1; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			inFence = !inFence
			continue
		}
		if !inFence && strings.HasPrefix(t, "## ") {
			end = i
			break
		}
	}
	for end > section+1 && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	out := append(lines[:end:end], append([]string{entry}, lines[end:]...)...)
	return os.WriteFile(p, []byte(strings.Join(out, "\n")), 0o644)
}

// ResolveHumanTasks completes every open human-owned task whose id starts
// with idPrefix -- the supervisor clearing its own stale blockers once the
// condition they reported has recovered. Prefix matching (not an exact id)
// makes recovery restart-proof: the supervisor need not remember which day's
// blocker it raised. Reports whether anything changed.
func ResolveHumanTasks(repoDir, idPrefix, resolution string) (bool, error) {
	p := filepath.Join(WorktreePath(repoDir), "tasks.md")
	raw, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	lines := strings.Split(string(raw), "\n")
	changed := false
	for i, line := range lines {
		if !strings.HasPrefix(strings.TrimSpace(line), "- [ ] `"+idPrefix) {
			continue
		}
		line = strings.Replace(line, "- [ ]", "- [x]", 1)
		if trimmed := strings.TrimRight(line, " "); strings.HasSuffix(trimmed, ")") {
			at := strings.LastIndex(line, ")")
			line = line[:at] + "; " + resolution + ")"
		} else {
			line += " (" + resolution + ")"
		}
		lines[i] = line
		changed = true
	}
	if !changed {
		return false, nil
	}
	return true, os.WriteFile(p, []byte(strings.Join(lines, "\n")), 0o644)
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
		_, _ = fmt.Sscanf(out, "%d", &st.Unpushed)
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
