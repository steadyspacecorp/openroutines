package knowledge

import (
	"fmt"
	"strings"

	"github.com/steadyspacecorp/openroutines/internal/repository"
)

type SyncReport struct {
	NoOrigin      bool   // repo has no origin remote (local mode)
	RemoteMissing bool   // origin exists but has no knowledge branch yet
	Unreachable   bool   // origin exists but could not be contacted
	Rewritten     bool   // remote history was rewritten -- sync refused
	Conflict      bool   // rebase conflict -- sync refused
	Adopted       bool   // remote commits were adopted locally
	Detail        string // human-readable context for tasks/logs
}

func (store *Store) HasOrigin() bool {
	_, err := repository.Run(store.repoDir, "remote", "get-url", "origin")
	return err == nil
}

// Records, on origin, the last knowledge tip this agent accepted
// -- what makes rewrite refusal durable across fetches and container
// replacement. A force-push must also know to move this ref.
const acceptedRef = "refs/openroutines/accepted"

// Returns the last accepted knowledge tip recorded on origin, ""
// when none has been recorded yet (pre-upgrade repos, first boot).
func (store *Store) AcceptedTip() string {
	if _, err := repository.Run(store.repoDir, "fetch", "--quiet", "origin", "+"+acceptedRef+":"+acceptedRef); err != nil {
		return ""
	}
	tip, _ := repository.Run(store.repoDir, "rev-parse", "--verify", "--quiet", acceptedRef)
	return tip
}

// Publishes tip as the new accepted baseline (best effort --
// the next sync simply re-verifies from the previous baseline).
func (store *Store) recordAccepted(tip string) {
	current, _ := repository.Run(store.repoDir, "rev-parse", "--verify", "--quiet", acceptedRef)
	if current == tip {
		return
	}
	if _, err := repository.Run(store.repoDir, "push", "--quiet", "origin", "+"+tip+":"+acceptedRef); err == nil {
		_, _ = repository.Run(store.repoDir, "update-ref", acceptedRef, tip)
	}
}

// Reconciles the local knowledge branch with origin: fast-forward when
// behind, rebase when diverged, refuse rewritten history and conflicts --
// never resolve silently. The rewrite baseline is the durable accepted ref.
func (store *Store) Sync() SyncReport {
	wt := store.Worktree()
	if !store.HasOrigin() {
		return SyncReport{NoOrigin: true}
	}
	if _, err := repository.Run(store.repoDir, "ls-remote", "--exit-code", "origin", "refs/heads/"+Branch); err != nil {
		if strings.Contains(err.Error(), "exit status 2") {
			return SyncReport{RemoteMissing: true} // first push will create it
		}
		return SyncReport{Unreachable: true, Detail: err.Error()}
	}

	// The accepted tip, falling back to the remote-tracking ref for repos
	// that predate it.
	oldTip := store.AcceptedTip()
	if oldTip == "" {
		oldTip, _ = repository.Run(store.repoDir, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+Branch)
	}
	if _, err := repository.Run(store.repoDir, "fetch", "--quiet", "origin", Branch); err != nil {
		return SyncReport{Unreachable: true, Detail: err.Error()}
	}
	newTip, err := repository.Run(store.repoDir, "rev-parse", "refs/remotes/origin/"+Branch)
	if err != nil {
		return SyncReport{Unreachable: true, Detail: err.Error()}
	}
	if oldTip != "" && oldTip != newTip && !isAncestor(store.repoDir, oldTip, newTip) {
		return SyncReport{Rewritten: true, Detail: fmt.Sprintf("origin/%s rewritten: %s no longer reachable from %s", Branch, short(oldTip), short(newTip))}
	}

	local, err := repository.Run(wt, "rev-parse", "HEAD")
	if err != nil {
		return SyncReport{Detail: err.Error()}
	}
	switch {
	case local == newTip:
		store.recordAccepted(newTip)
		return SyncReport{}
	case isAncestor(store.repoDir, local, newTip):
		// Behind: fast-forward only.
		if _, err := repository.Run(wt, "merge", "--ff-only", "--quiet", newTip); err != nil {
			return SyncReport{Conflict: true, Detail: err.Error()}
		}
		store.recordAccepted(newTip)
		return SyncReport{Adopted: true}
	case isAncestor(store.repoDir, newTip, local):
		return SyncReport{} // ahead: the next push carries it
	default:
		// Diverged (human curation raced local commits): rebase ours on top.
		if _, err := repository.Run(wt, "rebase", "--quiet", newTip); err != nil {
			_, _ = repository.Run(wt, "rebase", "--abort")
			return SyncReport{Conflict: true, Detail: err.Error()}
		}
		store.recordAccepted(newTip)
		return SyncReport{Adopted: true}
	}
}

// Publishes the knowledge branch. Fast-forward only: rejections are
// reported, never forced. A successful push advances the accepted baseline:
// origin's tip is now our own history.
func (store *Store) Push() error {
	if !store.HasOrigin() {
		return nil
	}
	wt := store.Worktree()
	if _, err := repository.Run(wt, "push", "--quiet", "origin", Branch); err != nil {
		return err
	}
	if tip, err := repository.Run(wt, "rev-parse", "HEAD"); err == nil {
		store.recordAccepted(tip)
	}
	return nil
}

// Where the supervisor strands knowledge it cannot put on the
// branch: a blocked sync refuses the branch, which is also where the blocker
// record lives, and a commit that never leaves the container dies with it.
// Supervisor-owned and uncontended -- origin's branch stays as the human
// left it.
const BlockedRef = "refs/openroutines/blocked"

// What a blocked supervisor left on origin. Tip is "" when
// nothing is stranded.
type BlockedSnapshot struct {
	Tip  string
	When string // when the supervisor stranded it, RFC3339
}

// Strands the committed knowledge state on the blocked ref as
// a parentless snapshot -- pushing the local tip would drag along the very
// lineage a rewrite may have just purged. Force is safe: the ref is the
// supervisor's own, and each snapshot supersedes the last.
func (store *Store) PublishBlocked() error {
	if !store.HasOrigin() {
		return nil
	}
	tree, err := repository.Run(store.repoDir, "rev-parse", "refs/heads/"+Branch+"^{tree}")
	if err != nil {
		return err
	}
	snap, err := repository.Run(store.repoDir, "commit-tree", tree, "-m",
		"Knowledge the agent could not publish: sync to origin/"+Branch+" is blocked")
	if err != nil {
		return err
	}
	_, err = repository.Run(store.repoDir, "push", "--quiet", "origin", "+"+snap+":"+BlockedRef)
	return err
}

// Drops the stranded ref, for the caller that has just published
// the same state on the branch itself. Best effort: a ref left behind costs
// nothing but a second copy of what the branch already carries.
func (store *Store) ClearBlocked() {
	_, _ = repository.Run(store.repoDir, "push", "--quiet", "origin", ":"+BlockedRef)
}

// Reports what a blocked supervisor stranded on origin. Fetching is
// part of the answer: the ref is outside the namespaces git replicates, so
// nothing else in a checkout would ever show it.
func (store *Store) Blocked() BlockedSnapshot {
	if _, err := repository.Run(store.repoDir, "fetch", "--quiet", "origin", "+"+BlockedRef+":"+BlockedRef); err != nil {
		return BlockedSnapshot{}
	}
	tip, err := repository.Run(store.repoDir, "rev-parse", "--verify", "--quiet", BlockedRef)
	if err != nil || tip == "" {
		return BlockedSnapshot{}
	}
	when, _ := repository.Run(store.repoDir, "log", "-1", "--format=%cI", tip)
	return BlockedSnapshot{Tip: tip, When: when}
}

func isAncestor(repoDir, a, b string) bool {
	cmdOut, err := repository.Run(repoDir, "merge-base", "--is-ancestor", a, b)
	_ = cmdOut
	return err == nil
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
