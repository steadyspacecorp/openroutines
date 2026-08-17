package knowledge

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steadyspacecorp/openroutines/internal/repository"
)

const (
	Dir        = "knowledge"
	Branch     = "knowledge"
	maxFile    = 5 << 20
	maxEntries = 2000
)

const stateDirName = "state"

type Store struct {
	repo     *repository.Repository
	worktree *repository.Worktree
}

func NewStore(repoDir string) *Store {
	return NewStoreForRepository(repository.Open(repoDir))
}

func NewStoreForRepository(repo *repository.Repository) *Store {
	return &Store{
		repo:     repo,
		worktree: repo.Worktree(filepath.Join(repo.Dir(), Dir)),
	}
}

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

var supervisorOwned = map[string]bool{
	stateDirName: true,
	"runs.jsonl": true,
}

func (store *Store) Worktree() string { return store.worktree.Dir() }

func (store *Store) StateDir() string { return filepath.Join(store.Worktree(), stateDirName) }

func (store *Store) Ensure() error {
	wt := store.Worktree()
	if _, err := os.Stat(filepath.Join(wt, ".git")); err == nil {
		if _, err := store.worktree.Run("rev-parse", "--is-inside-work-tree"); err == nil {
			return nil
		} else if !danglingWorktreeLink(filepath.Join(wt, ".git")) {
			return fmt.Errorf("checking knowledge worktree: %w", err)
		}
		orphaned := fmt.Sprintf("%s.orphaned-%d", wt, time.Now().UnixNano())
		if err := os.Rename(wt, orphaned); err != nil {
			return fmt.Errorf("preserving unattached knowledge worktree: %w", err)
		}
		slog.Warn("knowledge: moved an unattached worktree aside", "path", orphaned)
	}

	_, _ = store.repo.Run("worktree", "prune")
	if _, err := store.repo.Run("show-ref", "--verify", "--quiet", "refs/heads/"+Branch); err != nil {

		if store.repo.Remote() {
			if _, exists, lerr := store.repo.RemoteRef("refs/heads/" + Branch); lerr == nil && exists {
				if _, ferr := store.repo.Run("fetch", "--quiet", "origin", "+refs/heads/"+Branch+":refs/heads/"+Branch); ferr != nil {
					return fmt.Errorf("adopting knowledge branch from origin: %w", ferr)
				}

				if accepted := store.AcceptedTip(); accepted != "" {
					// Adoption is where a restart could launder a rewritten
					// history: refuse a tip that does not descend from the
					// accepted baseline, which survives container replacement.
					tip, terr := store.repo.Run("rev-parse", "refs/heads/"+Branch)
					if terr == nil && tip != accepted {
						descends, aerr := store.repo.IsAncestor(accepted, tip)
						if aerr != nil {
							return fmt.Errorf("checking adopted knowledge history: %w", aerr)
						}
						if !descends {
							return fmt.Errorf("origin/%s does not descend from the last accepted tip %s -- knowledge history was rewritten while this agent was down; refusing to adopt it. Inspect origin/%s, then either restore the branch or move %s to the new tip to accept the rewrite deliberately", Branch, short(accepted), Branch, acceptedRef)
						}
					}
				}
				tip, _ := store.repo.Run("rev-parse", "refs/heads/"+Branch)
				slog.Info("knowledge: adopted the knowledge branch from origin", "tip", tip)
			} else if lerr != nil {
				slog.Error("knowledge: cannot verify origin before creating the knowledge branch", "detail", lerr)
				return fmt.Errorf("origin state is unknown -- refusing to create the knowledge branch")
			}
		}
	}
	if _, err := store.repo.Run("show-ref", "--verify", "--quiet", "refs/heads/"+Branch); err != nil {

		tree, err := store.repo.RunStdin("", "mktree")
		if err != nil {
			return fmt.Errorf("creating knowledge branch: %w", err)
		}
		commit, err := store.repo.Run("commit-tree", tree, "-m", "Knowledge branch root")
		if err != nil {
			return fmt.Errorf("creating knowledge branch: %w", err)
		}
		if _, err := store.repo.Run("branch", Branch, commit); err != nil {
			return fmt.Errorf("creating knowledge branch: %w", err)
		}
		slog.Info("knowledge: created the knowledge branch", "commit", commit)
	}
	if _, err := store.repo.Run("worktree", "add", wt, Branch); err != nil {
		return err
	}

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
	if _, err := store.worktree.Run("add", "-A"); err != nil {
		return err
	}
	if changed, _ := store.worktree.Run("status", "--porcelain"); changed != "" {
		if _, err := store.worktree.Run("commit", "--quiet", "-m", "Seed knowledge primitives"); err != nil {
			return err
		}
	}
	return nil
}

func danglingWorktreeLink(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	gitDir, ok := strings.CutPrefix(strings.TrimSpace(string(raw)), "gitdir: ")
	if !ok || gitDir == "" {
		return false
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(filepath.Dir(path), gitDir)
	}
	_, err = os.Stat(gitDir)
	return os.IsNotExist(err)
}

func (store *Store) Commit(message string) (string, error) {
	if _, err := store.worktree.Run("add", "-A"); err != nil {
		return "", err
	}
	if changed, _ := store.worktree.Run("status", "--porcelain"); changed == "" {
		return "", nil
	}
	if _, err := store.worktree.Run("commit", "--quiet", "-m", message); err != nil {
		return "", err
	}
	return store.worktree.Run("rev-parse", "--short", "HEAD")
}

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

	if _, err := store.worktree.Run(append([]string{"add", "--"}, present...)...); err != nil {
		return "", err
	}
	if changed, _ := store.worktree.Run(append([]string{"status", "--porcelain", "--"}, present...)...); changed == "" {
		return "", nil
	}
	if _, err := store.worktree.Run(append([]string{"commit", "--quiet", "-m", message, "--"}, present...)...); err != nil {
		return "", err
	}
	return store.worktree.Run("rev-parse", "--short", "HEAD")
}

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
			rel, _ := filepath.Rel(store.repo.Dir(), p)
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

type WorktreeStatus struct {
	Durable         bool
	Materialized    bool
	RemoteKnowledge bool
	Uncommitted     int
	LastCommit      string
	Unpushed        int
	Behind          int
}

func (store *Store) Status() WorktreeStatus {
	st := WorktreeStatus{Durable: store.repo.Remote()}
	if _, err := store.repo.Run("rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+Branch); err == nil {
		st.RemoteKnowledge = true
	}
	wt := store.Worktree()
	if _, err := os.Stat(filepath.Join(wt, ".git")); err != nil {
		return st
	}
	st.Materialized = true
	if out, err := store.worktree.Run("status", "--porcelain"); err == nil && out != "" {
		st.Uncommitted = len(strings.Split(out, "\n"))
	}
	if out, err := store.worktree.Run("log", "-1", "--format=%h %s"); err == nil {
		st.LastCommit = out
	}
	if out, err := store.worktree.Run("rev-list", "--count", "refs/remotes/origin/"+Branch+"..HEAD"); err == nil {
		_, _ = fmt.Sscanf(out, "%d", &st.Unpushed)
	}

	if out, err := store.worktree.Run("rev-list", "--count", "HEAD..refs/remotes/origin/"+Branch); err == nil {
		_, _ = fmt.Sscanf(out, "%d", &st.Behind)
	}
	return st
}
