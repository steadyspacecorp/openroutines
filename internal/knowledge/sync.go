package knowledge

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// SyncReport describes what happened when reconciling with origin.
type SyncReport struct {
	NoOrigin      bool   // repo has no origin remote (local mode)
	RemoteMissing bool   // origin exists but has no knowledge branch yet
	Unreachable   bool   // origin exists but could not be contacted
	Rewritten     bool   // remote history was rewritten -- sync refused
	Conflict      bool   // rebase conflict -- sync refused
	Adopted       bool   // remote commits were adopted locally
	Detail        string // human-readable context for tasks/logs
}

// HasOrigin reports whether the repo has an origin remote configured.
func (store *Store) HasOrigin() bool {
	_, err := git(store.repoDir, "remote", "get-url", "origin")
	return err == nil
}

// acceptedRef records, on origin, the last knowledge tip this agent accepted
// -- what makes rewrite refusal durable across fetches and container
// replacement. A force-push must also know to move this ref.
const acceptedRef = "refs/openroutines/accepted"

// AcceptedTip returns the last accepted knowledge tip recorded on origin, ""
// when none has been recorded yet (pre-upgrade repos, first boot).
func (store *Store) AcceptedTip() string {
	if _, err := git(store.repoDir, "fetch", "--quiet", "origin", "+"+acceptedRef+":"+acceptedRef); err != nil {
		return ""
	}
	tip, _ := git(store.repoDir, "rev-parse", "--verify", "--quiet", acceptedRef)
	return tip
}

// recordAccepted publishes tip as the new accepted baseline (best effort --
// the next sync simply re-verifies from the previous baseline).
func (store *Store) recordAccepted(tip string) {
	current, _ := git(store.repoDir, "rev-parse", "--verify", "--quiet", acceptedRef)
	if current == tip {
		return
	}
	if _, err := git(store.repoDir, "push", "--quiet", "origin", "+"+tip+":"+acceptedRef); err == nil {
		_, _ = git(store.repoDir, "update-ref", acceptedRef, tip)
	}
}

// Sync reconciles the local knowledge branch with origin: fast-forward when
// behind, rebase when diverged, refuse rewritten history and conflicts --
// never resolve silently. The rewrite baseline is the durable accepted ref.
func (store *Store) Sync() SyncReport {
	wt := store.Worktree()
	if !store.HasOrigin() {
		return SyncReport{NoOrigin: true}
	}
	if _, err := git(store.repoDir, "ls-remote", "--exit-code", "origin", "refs/heads/"+Branch); err != nil {
		if strings.Contains(err.Error(), "exit status 2") {
			return SyncReport{RemoteMissing: true} // first push will create it
		}
		return SyncReport{Unreachable: true, Detail: err.Error()}
	}

	// The accepted tip, falling back to the remote-tracking ref for repos
	// that predate it.
	oldTip := store.AcceptedTip()
	if oldTip == "" {
		oldTip, _ = git(store.repoDir, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+Branch)
	}
	if _, err := git(store.repoDir, "fetch", "--quiet", "origin", Branch); err != nil {
		return SyncReport{Unreachable: true, Detail: err.Error()}
	}
	newTip, err := git(store.repoDir, "rev-parse", "refs/remotes/origin/"+Branch)
	if err != nil {
		return SyncReport{Unreachable: true, Detail: err.Error()}
	}
	if oldTip != "" && oldTip != newTip && !isAncestor(store.repoDir, oldTip, newTip) {
		return SyncReport{Rewritten: true, Detail: fmt.Sprintf("origin/%s rewritten: %s no longer reachable from %s", Branch, short(oldTip), short(newTip))}
	}

	local, err := git(wt, "rev-parse", "HEAD")
	if err != nil {
		return SyncReport{Detail: err.Error()}
	}
	switch {
	case local == newTip:
		store.recordAccepted(newTip)
		return SyncReport{}
	case isAncestor(store.repoDir, local, newTip):
		// Behind: fast-forward only.
		if _, err := git(wt, "merge", "--ff-only", "--quiet", newTip); err != nil {
			return SyncReport{Conflict: true, Detail: err.Error()}
		}
		store.recordAccepted(newTip)
		return SyncReport{Adopted: true}
	case isAncestor(store.repoDir, newTip, local):
		return SyncReport{} // ahead: the next push carries it
	default:
		// Diverged (human curation raced local commits): rebase ours on top.
		if _, err := git(wt, "rebase", "--quiet", newTip); err != nil {
			_, _ = git(wt, "rebase", "--abort")
			return SyncReport{Conflict: true, Detail: err.Error()}
		}
		store.recordAccepted(newTip)
		return SyncReport{Adopted: true}
	}
}

// Push publishes the knowledge branch. Fast-forward only: rejections are
// reported, never forced. A successful push advances the accepted baseline:
// origin's tip is now our own history.
func (store *Store) Push() error {
	if !store.HasOrigin() {
		return nil
	}
	wt := store.Worktree()
	if _, err := git(wt, "push", "--quiet", "origin", Branch); err != nil {
		return err
	}
	if tip, err := git(wt, "rev-parse", "HEAD"); err == nil {
		store.recordAccepted(tip)
	}
	return nil
}

// BlockedRef is where the supervisor strands knowledge it cannot put on the
// branch: a blocked sync refuses the branch, which is also where the blocker
// record lives, and a commit that never leaves the container dies with it.
// Supervisor-owned and uncontended -- origin's branch stays as the human
// left it.
const BlockedRef = "refs/openroutines/blocked"

// BlockedSnapshot is what a blocked supervisor left on origin. Tip is "" when
// nothing is stranded.
type BlockedSnapshot struct {
	Tip  string
	When string // when the supervisor stranded it, RFC3339
}

// PublishBlocked strands the committed knowledge state on the blocked ref as
// a parentless snapshot -- pushing the local tip would drag along the very
// lineage a rewrite may have just purged. Force is safe: the ref is the
// supervisor's own, and each snapshot supersedes the last.
func (store *Store) PublishBlocked() error {
	if !store.HasOrigin() {
		return nil
	}
	tree, err := git(store.repoDir, "rev-parse", "refs/heads/"+Branch+"^{tree}")
	if err != nil {
		return err
	}
	snap, err := git(store.repoDir, "commit-tree", tree, "-m",
		"Knowledge the agent could not publish: sync to origin/"+Branch+" is blocked")
	if err != nil {
		return err
	}
	_, err = git(store.repoDir, "push", "--quiet", "origin", "+"+snap+":"+BlockedRef)
	return err
}

// ClearBlocked drops the stranded ref, for the caller that has just published
// the same state on the branch itself. Best effort: a ref left behind costs
// nothing but a second copy of what the branch already carries.
func (store *Store) ClearBlocked() {
	_, _ = git(store.repoDir, "push", "--quiet", "origin", ":"+BlockedRef)
}

// Blocked reports what a blocked supervisor stranded on origin. Fetching is
// part of the answer: the ref is outside the namespaces git replicates, so
// nothing else in a checkout would ever show it.
func (store *Store) Blocked() BlockedSnapshot {
	if _, err := git(store.repoDir, "fetch", "--quiet", "origin", "+"+BlockedRef+":"+BlockedRef); err != nil {
		return BlockedSnapshot{}
	}
	tip, err := git(store.repoDir, "rev-parse", "--verify", "--quiet", BlockedRef)
	if err != nil || tip == "" {
		return BlockedSnapshot{}
	}
	when, _ := git(store.repoDir, "log", "-1", "--format=%cI", tip)
	return BlockedSnapshot{Tip: tip, When: when}
}

func isAncestor(repoDir, a, b string) bool {
	cmdOut, err := git(repoDir, "merge-base", "--is-ancestor", a, b)
	_ = cmdOut
	return err == nil
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// --- Lease: "one writer" enforced, not assumed -------------------------------

// The lease: a single-writer heartbeat ref with a bounded TTL. Heartbeats
// happen per dispatch and on a quarter-TTL cadence during runs, so the TTL
// bounds takeover latency, not run length.
const (
	leaseRef = "refs/openroutines/lease"
	LeaseTTL = 30 * time.Minute
)

// leaseContent formats the heartbeat blob.
func leaseContent(instanceID string, now time.Time) string {
	return fmt.Sprintf("%s %d\n", instanceID, now.Unix())
}

// Lease is the current holder's heartbeat, plus the blob SHA used as the
// compare-and-swap token for atomic takeover.
type Lease struct {
	Holder string
	At     time.Time
	SHA    string
}

// ReadLease fetches the current lease from origin; nil when none exists.
func (store *Store) ReadLease() (*Lease, error) {
	if _, lerr := git(store.repoDir, "fetch", "--quiet", "origin", "+"+leaseRef+":"+leaseRef); lerr != nil {
		if strings.Contains(lerr.Error(), "couldn't find remote ref") {
			return nil, nil
		}
		return nil, lerr
	}
	sha, err := git(store.repoDir, "rev-parse", leaseRef)
	if err != nil {
		return nil, err
	}
	content, cerr := git(store.repoDir, "cat-file", "blob", leaseRef)
	if cerr != nil {
		return nil, cerr
	}
	fields := strings.Fields(content)
	if len(fields) != 2 {
		return nil, fmt.Errorf("malformed lease %q", content)
	}
	var unix int64
	if _, serr := fmt.Sscanf(fields[1], "%d", &unix); serr != nil {
		return nil, serr
	}
	return &Lease{Holder: fields[0], At: time.Unix(unix, 0), SHA: sha}, nil
}

// WriteLease publishes this instance's heartbeat atomically: the push
// succeeds only if origin's lease is still exactly expectedSHA (empty means
// "must not exist"). Two instances racing for the same lease cannot both
// win. Returns the new lease SHA for the next renewal's expectation.
func (store *Store) WriteLease(instanceID string, now time.Time, expectedSHA string) (string, error) {
	blob, err := gitStdin(store.repoDir, leaseContent(instanceID, now), "hash-object", "-w", "--stdin")
	if err != nil {
		return "", err
	}
	cas := "--force-with-lease=" + leaseRef + ":" + expectedSHA
	if _, err := git(store.repoDir, "push", "--quiet", cas, "origin", blob+":"+leaseRef); err != nil {
		return "", err
	}
	return blob, nil
}

// ReleaseLease removes this instance's lease (best effort), but only if it
// is still the one this instance last wrote -- unconditional deletion let a
// stale instance delete the new holder's live lease. ownedSHA "" means this
// instance never held it.
func (store *Store) ReleaseLease(ownedSHA string) {
	if ownedSHA == "" {
		return
	}
	_, _ = git(store.repoDir, "push", "--quiet", "--force-with-lease="+leaseRef+":"+ownedSHA, "origin", ":"+leaseRef)
}

func gitStdin(dir, stdin string, args ...string) (string, error) {
	cmd := newGitCmd(dir, append(hermeticConfig, args...))
	defer cmd.cancel()
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", cmd.fail(args, err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// InstanceID identifies this supervisor process for the lease.
func InstanceID() string {
	host, _ := os.Hostname()
	return fmt.Sprintf("%s-%d", host, os.Getpid())
}
