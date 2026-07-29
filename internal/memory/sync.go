package memory

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// SyncReport describes what happened when reconciling with origin.
type SyncReport struct {
	NoOrigin      bool   // repo has no origin remote (local mode)
	RemoteMissing bool   // origin exists but has no memory branch yet
	Unreachable   bool   // origin exists but could not be contacted
	Rewritten     bool   // remote history was rewritten -- sync refused
	Conflict      bool   // rebase conflict -- sync refused
	Adopted       bool   // remote commits were adopted locally
	Detail        string // human-readable context for tasks/logs
}

// HasOrigin reports whether the repo has an origin remote configured.
func (m *Memory) HasOrigin() bool {
	_, err := git(m.repoDir, "remote", "get-url", "origin")
	return err == nil
}

// acceptedRef records, on origin, the last memory tip this agent accepted.
// It is what makes rewrite refusal durable: the remote-tracking ref is
// overwritten by every fetch (so the rewritten tip would become the next
// comparison's baseline) and dies with the container (so a redeploy would
// adopt anything). An attacker force-pushing the memory branch must now also
// know to move this ref -- and the refusal survives both the next sync call
// and a container replacement.
const acceptedRef = "refs/openroutines/accepted"

// AcceptedTip returns the last accepted memory tip recorded on origin, ""
// when none has been recorded yet (pre-upgrade repos, first boot).
func (m *Memory) AcceptedTip() string {
	if _, err := git(m.repoDir, "fetch", "--quiet", "origin", "+"+acceptedRef+":"+acceptedRef); err != nil {
		return ""
	}
	tip, _ := git(m.repoDir, "rev-parse", "--verify", "--quiet", acceptedRef)
	return tip
}

// recordAccepted publishes tip as the new accepted baseline (best effort --
// the next sync simply re-verifies from the previous baseline).
func (m *Memory) recordAccepted(tip string) {
	current, _ := git(m.repoDir, "rev-parse", "--verify", "--quiet", acceptedRef)
	if current == tip {
		return
	}
	if _, err := git(m.repoDir, "push", "--quiet", "origin", "+"+tip+":"+acceptedRef); err == nil {
		_, _ = git(m.repoDir, "update-ref", acceptedRef, tip)
	}
}

// Sync reconciles the local memory branch with origin, defensively
// (design decision "Memory"): fast-forward when behind; rebase local commits when
// diverged (append-only files rebase cleanly); refuse rewritten remote
// history and conflicts -- never resolve silently. The rewrite baseline is
// the durable accepted ref, so refusal holds across repeated syncs and
// process restarts alike.
func (m *Memory) Sync() SyncReport {
	wt := m.Worktree()
	if !m.HasOrigin() {
		return SyncReport{NoOrigin: true}
	}
	if _, err := git(m.repoDir, "ls-remote", "--exit-code", "origin", "refs/heads/"+Branch); err != nil {
		if strings.Contains(err.Error(), "exit status 2") {
			return SyncReport{RemoteMissing: true} // first push will create it
		}
		return SyncReport{Unreachable: true, Detail: err.Error()}
	}

	// The baseline for rewrite detection: the durably recorded accepted tip,
	// falling back to the remote-tracking ref for repos that predate it.
	oldTip := m.AcceptedTip()
	if oldTip == "" {
		oldTip, _ = git(m.repoDir, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+Branch)
	}
	if _, err := git(m.repoDir, "fetch", "--quiet", "origin", Branch); err != nil {
		return SyncReport{Unreachable: true, Detail: err.Error()}
	}
	newTip, err := git(m.repoDir, "rev-parse", "refs/remotes/origin/"+Branch)
	if err != nil {
		return SyncReport{Unreachable: true, Detail: err.Error()}
	}
	if oldTip != "" && oldTip != newTip && !isAncestor(m.repoDir, oldTip, newTip) {
		return SyncReport{Rewritten: true, Detail: fmt.Sprintf("origin/%s rewritten: %s no longer reachable from %s", Branch, short(oldTip), short(newTip))}
	}

	local, err := git(wt, "rev-parse", "HEAD")
	if err != nil {
		return SyncReport{Detail: err.Error()}
	}
	switch {
	case local == newTip:
		m.recordAccepted(newTip)
		return SyncReport{}
	case isAncestor(m.repoDir, local, newTip):
		// Behind: fast-forward only.
		if _, err := git(wt, "merge", "--ff-only", "--quiet", newTip); err != nil {
			return SyncReport{Conflict: true, Detail: err.Error()}
		}
		m.recordAccepted(newTip)
		return SyncReport{Adopted: true}
	case isAncestor(m.repoDir, newTip, local):
		return SyncReport{} // ahead: the next push carries it
	default:
		// Diverged (human curation raced local commits): rebase ours on top.
		if _, err := git(wt, "rebase", "--quiet", newTip); err != nil {
			_, _ = git(wt, "rebase", "--abort")
			return SyncReport{Conflict: true, Detail: err.Error()}
		}
		m.recordAccepted(newTip)
		return SyncReport{Adopted: true}
	}
}

// Push publishes the memory branch. Fast-forward only: rejections are
// reported, never forced. A successful push advances the accepted baseline:
// origin's tip is now our own history.
func (m *Memory) Push() error {
	if !m.HasOrigin() {
		return nil
	}
	wt := m.Worktree()
	if _, err := git(wt, "push", "--quiet", "origin", Branch); err != nil {
		return err
	}
	if tip, err := git(wt, "rev-parse", "HEAD"); err == nil {
		m.recordAccepted(tip)
	}
	return nil
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

// The lease: a single-writer heartbeat ref with a bounded TTL. The supervisor
// heartbeats before every run it dispatches, so the lease goes at most one run
// stale; the TTL is twice the longest run the framework supports, which is
// what makes "a live lease means a live instance" checkable rather than hoped
// for (design decision "The lease is renewed per run, not per tick").
const (
	leaseRef = "refs/openroutines/lease"
	LeaseTTL = 30 * time.Minute

	// MaxRunTimeout is the longest effective timeout the lease can cover.
	// `openroutines check` warns above it.
	MaxRunTimeout = LeaseTTL / 2
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
func (m *Memory) ReadLease() (*Lease, error) {
	if _, lerr := git(m.repoDir, "fetch", "--quiet", "origin", "+"+leaseRef+":"+leaseRef); lerr != nil {
		if strings.Contains(lerr.Error(), "couldn't find remote ref") {
			return nil, nil
		}
		return nil, lerr
	}
	sha, err := git(m.repoDir, "rev-parse", leaseRef)
	if err != nil {
		return nil, err
	}
	content, cerr := git(m.repoDir, "cat-file", "blob", leaseRef)
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
func (m *Memory) WriteLease(instanceID string, now time.Time, expectedSHA string) (string, error) {
	blob, err := gitStdin(m.repoDir, leaseContent(instanceID, now), "hash-object", "-w", "--stdin")
	if err != nil {
		return "", err
	}
	cas := "--force-with-lease=" + leaseRef + ":" + expectedSHA
	if _, err := git(m.repoDir, "push", "--quiet", cas, "origin", blob+":"+leaseRef); err != nil {
		return "", err
	}
	return blob, nil
}

// ReleaseLease removes this instance's lease from origin (best effort) --
// but only if origin's lease is still the one this instance last wrote.
// Unconditional deletion let a stale instance, shutting down after losing
// the lease, delete the new holder's live lease. ownedSHA "" means this
// instance never held the lease; there is nothing to release.
func (m *Memory) ReleaseLease(ownedSHA string) {
	if ownedSHA == "" {
		return
	}
	_, _ = git(m.repoDir, "push", "--quiet", "--force-with-lease="+leaseRef+":"+ownedSHA, "origin", ":"+leaseRef)
}

func gitStdin(dir, stdin string, args ...string) (string, error) {
	cmd := newGitCmd(dir, append(hermeticConfig, args...))
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// InstanceID identifies this supervisor process for the lease.
func InstanceID() string {
	host, _ := os.Hostname()
	return fmt.Sprintf("%s-%d", host, os.Getpid())
}
