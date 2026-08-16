package repository

import (
	"fmt"
	"strings"
	"time"
)

const leaseRef = "refs/openroutines/lease"

// Lease is the current holder's heartbeat and its compare-and-swap token.
type Lease struct {
	Holder string
	At     time.Time
	SHA    string
}

// ReadLease fetches the current repository writer lease from origin.
func (repo *Repository) ReadLease() (*Lease, error) {
	if !repo.Remote() {
		return nil, nil
	}
	if _, err := repo.Run("fetch", "--quiet", "origin", "+"+leaseRef+":"+leaseRef); err != nil {
		if strings.Contains(err.Error(), "couldn't find remote ref") {
			return nil, nil
		}
		return nil, err
	}
	sha, err := repo.Run("rev-parse", leaseRef)
	if err != nil {
		return nil, err
	}
	content, err := repo.Run("cat-file", "blob", leaseRef)
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(content)
	if len(fields) != 2 {
		return nil, fmt.Errorf("malformed lease %q", content)
	}
	at, err := parseLeaseTime(fields[1])
	if err != nil {
		return nil, err
	}
	return &Lease{Holder: fields[0], At: at, SHA: sha}, nil
}

// WriteLease atomically publishes a heartbeat if the expected lease still
// names origin's current value. An empty expected SHA requires no live lease.
func (repo *Repository) WriteLease(instanceID string, now time.Time, expectedSHA string) (string, error) {
	if !repo.Remote() {
		return "", nil
	}
	content := fmt.Sprintf("%s %s\n", instanceID, now.Format(time.RFC3339Nano))
	blob, err := repo.RunStdin(content, "hash-object", "-w", "--stdin")
	if err != nil {
		return "", err
	}
	cas := "--force-with-lease=" + leaseRef + ":" + expectedSHA
	if _, err := repo.Run("push", "--quiet", cas, "origin", blob+":"+leaseRef); err != nil {
		return "", err
	}
	return blob, nil
}

// ReleaseLease removes ownedSHA only while it remains origin's current lease.
func (repo *Repository) ReleaseLease(ownedSHA string) {
	if ownedSHA == "" || !repo.Remote() {
		return
	}
	_, _ = repo.Run("push", "--quiet", "--force-with-lease="+leaseRef+":"+ownedSHA, "origin", ":"+leaseRef)
}

func parseLeaseTime(value string) (time.Time, error) {
	if at, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return at, nil
	}
	var unix int64
	if _, err := fmt.Sscanf(value, "%d", &unix); err != nil {
		return time.Time{}, err
	}
	return time.Unix(unix, 0), nil
}
