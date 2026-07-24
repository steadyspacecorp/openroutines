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
func HasOrigin(repoDir string) bool {
	_, err := git(repoDir, "remote", "get-url", "origin")
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
func AcceptedTip(repoDir string) string {
	if _, err := git(repoDir, "fetch", "--quiet", "origin", "+"+acceptedRef+":"+acceptedRef); err != nil {
		return ""
	}
	tip, _ := git(repoDir, "rev-parse", "--verify", "--quiet", acceptedRef)
	return tip
}

// recordAccepted publishes tip as the new accepted baseline (best effort --
// the next sync simply re-verifies from the previous baseline).
func recordAccepted(repoDir, tip string) {
	current, _ := git(repoDir, "rev-parse", "--verify", "--quiet", acceptedRef)
	if current == tip {
		return
	}
	if _, err := git(repoDir, "push", "--quiet", "origin", "+"+tip+":"+acceptedRef); err == nil {
		_, _ = git(repoDir, "update-ref", acceptedRef, tip)
	}
}

// Sync reconciles the local memory branch with origin, defensively
// (DESIGN.md "Memory"): fast-forward when behind; rebase local commits when
// diverged (append-only files rebase cleanly); refuse rewritten remote
// history and conflicts -- never resolve silently. The rewrite baseline is
// the durable accepted ref, so refusal holds across repeated syncs and
// process restarts alike.
func Sync(repoDir string) SyncReport {
	wt := WorktreePath(repoDir)
	if !HasOrigin(repoDir) {
		return SyncReport{NoOrigin: true}
	}
	if _, err := git(repoDir, "ls-remote", "--exit-code", "origin", "refs/heads/"+Branch); err != nil {
		if strings.Contains(err.Error(), "exit status 2") {
			return SyncReport{RemoteMissing: true} // first push will create it
		}
		return SyncReport{Unreachable: true, Detail: err.Error()}
	}

	// The baseline for rewrite detection: the durably recorded accepted tip,
	// falling back to the remote-tracking ref for repos that predate it.
	oldTip := AcceptedTip(repoDir)
	if oldTip == "" {
		oldTip, _ = git(repoDir, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+Branch)
	}
	if _, err := git(repoDir, "fetch", "--quiet", "origin", Branch); err != nil {
		return SyncReport{Unreachable: true, Detail: err.Error()}
	}
	newTip, err := git(repoDir, "rev-parse", "refs/remotes/origin/"+Branch)
	if err != nil {
		return SyncReport{Unreachable: true, Detail: err.Error()}
	}
	if oldTip != "" && oldTip != newTip && !isAncestor(repoDir, oldTip, newTip) {
		return SyncReport{Rewritten: true, Detail: fmt.Sprintf("origin/%s rewritten: %s no longer reachable from %s", Branch, short(oldTip), short(newTip))}
	}

	local, err := git(wt, "rev-parse", "HEAD")
	if err != nil {
		return SyncReport{Detail: err.Error()}
	}
	switch {
	case local == newTip:
		recordAccepted(repoDir, newTip)
		return SyncReport{}
	case isAncestor(repoDir, local, newTip):
		// Behind: fast-forward only.
		if _, err := git(wt, "merge", "--ff-only", "--quiet", newTip); err != nil {
			return SyncReport{Conflict: true, Detail: err.Error()}
		}
		recordAccepted(repoDir, newTip)
		return SyncReport{Adopted: true}
	case isAncestor(repoDir, newTip, local):
		return SyncReport{} // ahead: the next push carries it
	default:
		// Diverged (human curation raced local commits): rebase ours on top.
		if _, err := git(wt, "rebase", "--quiet", newTip); err != nil {
			_, _ = git(wt, "rebase", "--abort")
			return SyncReport{Conflict: true, Detail: err.Error()}
		}
		recordAccepted(repoDir, newTip)
		return SyncReport{Adopted: true}
	}
}

// Push publishes the memory branch. Fast-forward only: rejections are
// reported, never forced. A successful push advances the accepted baseline:
// origin's tip is now our own history.
func Push(repoDir string) error {
	if !HasOrigin(repoDir) {
		return nil
	}
	wt := WorktreePath(repoDir)
	if _, err := git(wt, "push", "--quiet", "origin", Branch); err != nil {
		return err
	}
	if tip, err := git(wt, "rev-parse", "HEAD"); err == nil {
		recordAccepted(repoDir, tip)
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

const (
	leaseRef = "refs/openroutines/lease"
	LeaseTTL = 5 * time.Minute
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
func ReadLease(repoDir string) (*Lease, error) {
	if _, lerr := git(repoDir, "fetch", "--quiet", "origin", "+"+leaseRef+":"+leaseRef); lerr != nil {
		if strings.Contains(lerr.Error(), "couldn't find remote ref") {
			return nil, nil
		}
		return nil, lerr
	}
	sha, err := git(repoDir, "rev-parse", leaseRef)
	if err != nil {
		return nil, err
	}
	content, cerr := git(repoDir, "cat-file", "blob", leaseRef)
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
func WriteLease(repoDir, instanceID string, now time.Time, expectedSHA string) (string, error) {
	blob, err := gitStdin(repoDir, leaseContent(instanceID, now), "hash-object", "-w", "--stdin")
	if err != nil {
		return "", err
	}
	cas := "--force-with-lease=" + leaseRef + ":" + expectedSHA
	if _, err := git(repoDir, "push", "--quiet", cas, "origin", blob+":"+leaseRef); err != nil {
		return "", err
	}
	return blob, nil
}

// ReleaseLease removes this instance's lease from origin (best effort) --
// but only if origin's lease is still the one this instance last wrote.
// Unconditional deletion let a stale instance, shutting down after losing
// the lease, delete the new holder's live lease. ownedSHA "" means this
// instance never held the lease; there is nothing to release.
func ReleaseLease(repoDir, ownedSHA string) {
	if ownedSHA == "" {
		return
	}
	_, _ = git(repoDir, "push", "--quiet", "--force-with-lease="+leaseRef+":"+ownedSHA, "origin", ":"+leaseRef)
}

func gitStdin(dir, stdin string, args ...string) (string, error) {
	base := []string{
		"-c", "core.hooksPath=/dev/null",
		"-c", "protocol.file.allow=user",
	}
	cmd := newGitCmd(dir, append(base, args...))
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
