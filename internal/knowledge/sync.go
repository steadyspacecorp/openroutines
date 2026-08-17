package knowledge

import (
	"fmt"
	"strings"
)

type SyncReport struct {
	LocalOnly     bool
	RemoteMissing bool
	Unreachable   bool
	Rewritten     bool
	Conflict      bool
	Adopted       bool
	Detail        string
}

// This origin ref makes rewrite refusal durable across fetches and container
// replacement; a force-push must move it too.
const acceptedRef = "refs/openroutines/accepted"

func (store *Store) AcceptedTip() string {
	if _, err := store.repo.Run("fetch", "--quiet", "origin", "+"+acceptedRef+":"+acceptedRef); err != nil {
		return ""
	}
	tip, _ := store.repo.Run("rev-parse", "--verify", "--quiet", acceptedRef)
	return tip
}

func (store *Store) recordAccepted(tip string) {
	current, _ := store.repo.Run("rev-parse", "--verify", "--quiet", acceptedRef)
	if current == tip {
		return
	}
	if _, err := store.repo.Run("push", "--quiet", "origin", "+"+tip+":"+acceptedRef); err == nil {
		_, _ = store.repo.Run("update-ref", acceptedRef, tip)
	}
}

func (store *Store) Sync() SyncReport {
	if !store.repo.Remote() {
		return SyncReport{LocalOnly: true}
	}
	if _, exists, err := store.repo.RemoteRef("refs/heads/" + Branch); err != nil {
		return SyncReport{Unreachable: true, Detail: err.Error()}
	} else if !exists {
		return SyncReport{RemoteMissing: true}
	}

	oldTip := store.AcceptedTip()
	// The accepted tip, falling back to the remote-tracking ref for repos
	// that predate it.
	if oldTip == "" {
		oldTip, _ = store.repo.Run("rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+Branch)
	}
	if _, err := store.repo.Run("fetch", "--quiet", "origin", Branch); err != nil {
		return SyncReport{Unreachable: true, Detail: err.Error()}
	}
	newTip, err := store.repo.Run("rev-parse", "refs/remotes/origin/"+Branch)
	if err != nil {
		return SyncReport{Unreachable: true, Detail: err.Error()}
	}
	if oldTip != "" && oldTip != newTip {
		descends, aerr := store.repo.IsAncestor(oldTip, newTip)
		if aerr != nil {
			return SyncReport{Detail: aerr.Error()}
		}
		if !descends {
			return SyncReport{Rewritten: true, Detail: fmt.Sprintf("origin/%s rewritten: %s no longer reachable from %s", Branch, short(oldTip), short(newTip))}
		}
	}

	local, err := store.worktree.Run("rev-parse", "HEAD")
	if err != nil {
		return SyncReport{Detail: err.Error()}
	}
	if local == newTip {
		store.recordAccepted(newTip)
		return SyncReport{}
	}
	localBehind, err := store.repo.IsAncestor(local, newTip)
	if err != nil {
		return SyncReport{Detail: err.Error()}
	}
	if localBehind {

		if _, err := store.worktree.Run("merge", "--ff-only", "--quiet", newTip); err != nil {
			return SyncReport{Conflict: true, Detail: err.Error()}
		}
		store.recordAccepted(newTip)
		return SyncReport{Adopted: true}
	}
	localAhead, err := store.repo.IsAncestor(newTip, local)
	if err != nil {
		return SyncReport{Detail: err.Error()}
	}
	if localAhead {
		return SyncReport{}
	}

	if _, err := store.worktree.Run("rebase", "--quiet", newTip); err != nil {
		_, _ = store.worktree.Run("rebase", "--abort")
		return SyncReport{Conflict: true, Detail: conflictDetail(err)}
	}
	store.recordAccepted(newTip)
	return SyncReport{Adopted: true}
}

func conflictDetail(err error) string {
	lines := strings.Split(err.Error(), "\n")
	out := lines[:0]
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "hint:") {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func (store *Store) Push() error {
	if !store.repo.Remote() {
		return nil
	}
	if _, err := store.worktree.Run("push", "--quiet", "origin", Branch); err != nil {
		return err
	}
	if tip, err := store.worktree.Run("rev-parse", "HEAD"); err == nil {
		store.recordAccepted(tip)
	}
	return nil
}

// A blocked sync cannot safely update knowledge, so its committed state is
// stranded on this supervisor-owned ref instead of dying with the container.
const BlockedRef = "refs/openroutines/blocked"

type BlockedSnapshot struct {
	Tip  string
	When string
}

func (store *Store) PublishBlocked() error {
	if !store.repo.Remote() {
		return nil
	}
	tree, err := store.repo.Run("rev-parse", "refs/heads/"+Branch+"^{tree}")
	if err != nil {
		return err
	}
	// A parentless snapshot avoids dragging rewritten history into the blocked ref.
	snap, err := store.repo.Run("commit-tree", tree, "-m",
		"Knowledge the agent could not publish: sync to origin/"+Branch+" is blocked")
	if err != nil {
		return err
	}
	_, err = store.repo.Run("push", "--quiet", "origin", "+"+snap+":"+BlockedRef)
	return err
}

func (store *Store) ClearBlocked() {
	_, _ = store.repo.Run("push", "--quiet", "origin", ":"+BlockedRef)
}

func (store *Store) Blocked() BlockedSnapshot {
	if _, err := store.repo.Run("fetch", "--quiet", "origin", "+"+BlockedRef+":"+BlockedRef); err != nil {
		return BlockedSnapshot{}
	}
	tip, err := store.repo.Run("rev-parse", "--verify", "--quiet", BlockedRef)
	if err != nil || tip == "" {
		return BlockedSnapshot{}
	}
	when, _ := store.repo.Run("log", "-1", "--format=%cI", tip)
	return BlockedSnapshot{Tip: tip, When: when}
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
