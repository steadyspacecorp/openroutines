// Package knowledge manages the agent's knowledge: a git worktree of the orphan
// `knowledge` branch, plus the staging pipeline that keeps model-directed
// processes away from git entirely.
package knowledge

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// Knowledge is a dedicated directory backed by its own branch.
const (
	Dir        = "knowledge"
	Branch     = "knowledge"
	maxFile    = 5 << 20 // per-file size cap in staged knowledge
	maxEntries = 2000    // total staged file cap
)

// stateDirName is the supervisor-owned directory inside the worktree holding
// per-routine bookkeeping: scheduling state at its root, trigger baselines
// and consumer cursors in subdirectories.
const stateDirName = "state"

// Store is one agent repository's knowledge. The handle binds the repository
// root; every operation that reads or maintains the knowledge branch, its
// worktree, and the supervisor-owned state inside it goes through here.
type Store struct {
	repoDir string
}

// NewStore binds the agent repository at repoDir. No I/O: the worktree may not be
// materialized yet (Status reports that; Ensure fixes it).
func NewStore(repoDir string) *Store { return &Store{repoDir: repoDir} }

// primitives are the framework-blessed shared knowledge files, seeded on init.
// Each opens with a fenced example of its format: the file teaches its own
// shape at the point of use, and the retention trimmer preserves everything
// that isn't a record. Agents may reshape the headers in their own knowledge
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
	stateDirName: true,
	"runs.jsonl": true,
}

// Worktree returns the materialized knowledge worktree path.
func (store *Store) Worktree() string { return filepath.Join(store.repoDir, Dir) }

// StateDir returns the supervisor-owned state directory inside the worktree.
// Per-routine state lives at <StateDir>/<name>.json (scheduling) and
// <StateDir>/<subdir>/<name>.json (trigger baselines, consumer cursors).
func (store *Store) StateDir() string { return filepath.Join(store.Worktree(), stateDirName) }

// Ensure materializes knowledge/ as a worktree of the knowledge branch,
// creating the orphan branch and seeding the primitives on first use.
// Self-heals: safe to call every run.
func (store *Store) Ensure() error {
	wt := store.Worktree()
	if _, err := os.Stat(filepath.Join(wt, ".git")); err == nil {
		return nil // already materialized
	}
	// The image's .git may register a worktree whose directory was excluded
	// from the image; prune or the add below fails on first boot.
	_, _ = git(store.repoDir, "worktree", "prune")
	if _, err := git(store.repoDir, "show-ref", "--verify", "--quiet", "refs/heads/"+Branch); err != nil {
		// No local branch: adopt origin's rather than minting a new root
		// that splices into the lineage.
		if store.HasOrigin() {
			if _, lerr := git(store.repoDir, "ls-remote", "--exit-code", "origin", "refs/heads/"+Branch); lerr == nil {
				if _, ferr := git(store.repoDir, "fetch", "--quiet", "origin", "+refs/heads/"+Branch+":refs/heads/"+Branch); ferr != nil {
					return fmt.Errorf("adopting knowledge branch from origin: %w", ferr)
				}
				// Adoption is where a restart could launder a rewritten
				// history: refuse a tip that does not descend from the
				// accepted baseline, which survives container replacement.
				if accepted := store.AcceptedTip(); accepted != "" {
					tip, terr := git(store.repoDir, "rev-parse", "refs/heads/"+Branch)
					if terr == nil && tip != accepted && !isAncestor(store.repoDir, accepted, tip) {
						return fmt.Errorf("origin/%s does not descend from the last accepted tip %s -- knowledge history was rewritten while this agent was down; refusing to adopt it. Inspect origin/%s, then either restore the branch or move %s to the new tip to accept the rewrite deliberately", Branch, short(accepted), Branch, acceptedRef)
					}
				}
				tip, _ := git(store.repoDir, "rev-parse", "refs/heads/"+Branch)
				slog.Info("knowledge: adopted the knowledge branch from origin", "tip", tip)
			} else if !strings.Contains(lerr.Error(), "exit status 2") {
				slog.Warn("knowledge: could not reach origin to adopt the knowledge branch -- creating a local root; this will diverge if origin has one", "error", lerr)
			}
		}
	}
	if _, err := git(store.repoDir, "show-ref", "--verify", "--quiet", "refs/heads/"+Branch); err != nil {
		// First use: orphan branch from an empty tree via plumbing
		// (worktree add --orphan needs git >= 2.42).
		tree, err := gitStdin(store.repoDir, "", "mktree")
		if err != nil {
			return fmt.Errorf("creating knowledge branch: %w", err)
		}
		commit, err := git(store.repoDir, "commit-tree", tree, "-m", "Knowledge branch root")
		if err != nil {
			return fmt.Errorf("creating knowledge branch: %w", err)
		}
		if _, err := git(store.repoDir, "branch", Branch, commit); err != nil {
			return fmt.Errorf("creating knowledge branch: %w", err)
		}
		slog.Info("knowledge: created the knowledge branch", "commit", commit)
	}
	if _, err := git(store.repoDir, "worktree", "add", wt, Branch); err != nil {
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
		if _, err := git(wt, "commit", "--quiet", "-m", "Seed knowledge primitives"); err != nil {
			return err
		}
	}
	return nil
}

// Commit records the current worktree state on the knowledge branch.
func (store *Store) Commit(message string) (string, error) {
	wt := store.Worktree()
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

// commitPaths commits only the named paths: a maintenance commit must not
// carry work that merely happened to be dirty when it fired. Missing paths
// are skipped.
func (store *Store) commitPaths(message string, paths ...string) (string, error) {
	wt := store.Worktree()
	var present []string
	for _, p := range paths {
		if _, err := os.Stat(filepath.Join(wt, p)); err == nil {
			present = append(present, p)
		}
	}
	if len(present) == 0 {
		return "", nil
	}
	// A pathspec commit only covers tracked paths -- add first so a new file
	// isn't a pathspec that matches nothing.
	if _, err := git(wt, append([]string{"add", "--"}, present...)...); err != nil {
		return "", err
	}
	if changed, _ := git(wt, append([]string{"status", "--porcelain", "--"}, present...)...); changed == "" {
		return "", nil // nothing to commit
	}
	if _, err := git(wt, append([]string{"commit", "--quiet", "-m", message, "--"}, present...)...); err != nil {
		return "", err
	}
	return git(wt, "rev-parse", "--short", "HEAD")
}

// RemoveRoutineState deletes every per-routine state file for name: the
// scheduling state plus the entry in every state subdirectory. Filenames are
// compared, never globbed, so name cannot alter the matching. Returns the
// removed paths relative to the repository root; the caller commits.
func (store *Store) RemoveRoutineState(name string) ([]string, error) {
	stateDir := store.StateDir()
	entries, err := os.ReadDir(stateDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var removed []string
	remove := func(dir string) error {
		p := filepath.Join(dir, name+".json")
		switch err := os.Remove(p); {
		case err == nil:
			rel, _ := filepath.Rel(store.repoDir, p)
			removed = append(removed, rel)
		case !os.IsNotExist(err):
			return err
		}
		return nil
	}
	if err := remove(stateDir); err != nil {
		return removed, err
	}
	for _, e := range entries {
		if e.IsDir() {
			if err := remove(filepath.Join(stateDir, e.Name())); err != nil {
				return removed, err
			}
		}
	}
	return removed, nil
}

// WorktreeStatus reports the knowledge worktree's state for `openroutines
// status` -- root `git status` never shows knowledge churn, so this must.
type WorktreeStatus struct {
	Materialized    bool
	RemoteKnowledge bool   // origin/knowledge ref exists locally: the agent has history even if this checkout hasn't adopted it
	Uncommitted     int    // files with uncommitted changes (human curation in progress)
	LastCommit      string // subject of the latest knowledge commit
	Unpushed        int    // commits origin hasn't seen yet
	Behind          int    // commits on origin this worktree has not taken
}

// Status inspects the knowledge worktree; only RemoteKnowledge is set when not
// yet materialized -- it distinguishes a fresh clone of a running agent
// (adopt with sync) from an agent that has never run.
func (store *Store) Status() WorktreeStatus {
	var st WorktreeStatus
	if _, err := git(store.repoDir, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+Branch); err == nil {
		st.RemoteKnowledge = true
	}
	wt := store.Worktree()
	if _, err := os.Stat(filepath.Join(wt, ".git")); err != nil {
		return st
	}
	st.Materialized = true
	if out, err := git(wt, "status", "--porcelain"); err == nil && out != "" {
		st.Uncommitted = len(strings.Split(out, "\n"))
	}
	if out, err := git(wt, "log", "-1", "--format=%h %s"); err == nil {
		st.LastCommit = out
	}
	if out, err := git(wt, "rev-list", "--count", "refs/remotes/origin/"+Branch+"..HEAD"); err == nil {
		_, _ = fmt.Sscanf(out, "%d", &st.Unpushed)
	}
	// As of the last fetch: the remote-tracking ref is local, so this stays
	// offline.
	if out, err := git(wt, "rev-list", "--count", "HEAD..refs/remotes/origin/"+Branch); err == nil {
		_, _ = fmt.Sscanf(out, "%d", &st.Behind)
	}
	return st
}
