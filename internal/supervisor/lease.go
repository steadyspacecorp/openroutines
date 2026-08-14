package supervisor

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

const leaseTTL = 30 * time.Minute

func instanceID() string {
	host, _ := os.Hostname()
	return fmt.Sprintf("%s-%d", host, os.Getpid())
}

type leaseKeeper struct {
	instanceID string
	mu         sync.Mutex
	sha        string
	ttl        time.Duration
	renewed    time.Time
	warned     bool
}

// Enforces "exactly one instance": a fresh foreign lease means
// another supervisor is alive, so this one waits. The write is a
// compare-and-swap on the lease ref -- two racing instances cannot both win.
func (s *Supervisor) acquireLease(ctx context.Context) error {
	for {
		lease, err := s.repo.ReadLease()
		if err != nil {
			return fmt.Errorf("lease: %w", err)
		}
		expected := ""
		eligible := lease == nil
		if lease != nil {
			expected = lease.SHA
			eligible = lease.Holder == s.lease.instanceID || time.Since(lease.At) > s.lease.ttl
		}
		if eligible {
			now := time.Now()
			sha, werr := s.repo.WriteLease(s.lease.instanceID, now, expected)
			if werr == nil {
				s.lease.mu.Lock()
				s.holdLease(sha, now)
				s.lease.mu.Unlock()
				return nil
			}
			slog.Info("lease race lost -- re-evaluating")
			continue
		}
		slog.Warn("another instance holds the lease -- waiting", "holder", lease.Holder, "heartbeat", lease.At)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(30 * time.Second):
		}
	}
}

func (s *Supervisor) renewLease() bool {
	s.lease.mu.Lock()
	defer s.lease.mu.Unlock()
	return s.renewLeaseLocked()
}

func (s *Supervisor) tryRenewLease() bool {
	s.lease.mu.Lock()
	defer s.lease.mu.Unlock()
	if time.Since(s.lease.renewed) < s.lease.ttl/4 {
		return true
	}
	return s.renewLeaseLocked()
}

func (s *Supervisor) leaseExpired() bool {
	s.lease.mu.Lock()
	defer s.lease.mu.Unlock()
	return time.Since(s.lease.renewed) > s.lease.ttl
}

func (s *Supervisor) renewLeaseLocked() bool {
	now := time.Now()
	if sha, err := s.repo.WriteLease(s.lease.instanceID, now, s.lease.sha); err == nil {
		s.holdLease(sha, now)
		return true
	}
	lease, err := s.repo.ReadLease()
	if err == nil && lease != nil && lease.Holder != s.lease.instanceID && time.Since(lease.At) <= s.lease.ttl {
		return s.leaseLost(fmt.Sprintf("lease held by %s (last heartbeat %s ago, expires in %s) -- pausing dispatch",
			lease.Holder, time.Since(lease.At).Round(time.Second), (s.lease.ttl - time.Since(lease.At)).Round(time.Second)))
	}
	expected := ""
	if lease != nil {
		expected = lease.SHA
	}
	if sha, werr := s.repo.WriteLease(s.lease.instanceID, now, expected); werr == nil {
		s.holdLease(sha, now)
		return true
	}
	return s.leaseLost("lease renewal failed -- pausing dispatch until origin accepts a heartbeat")
}

func (s *Supervisor) keepLeaseAlive(ctx context.Context, cancelRun context.CancelFunc, log *slog.Logger) (stop func()) {
	quit := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(s.lease.ttl / 4)
		defer ticker.Stop()
		for {
			select {
			case <-quit:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if s.tryRenewLease() {
					continue
				}
				if s.foreignLeaseLive() || s.leaseExpired() {
					log.Error("lease lost mid-run -- canceling the attempt")
					cancelRun()
					return
				}
			}
		}
	}()
	return func() {
		close(quit)
		<-done
	}
}

func (s *Supervisor) foreignLeaseLive() bool {
	lease, err := s.repo.ReadLease()
	return err == nil && lease != nil && lease.Holder != s.lease.instanceID && time.Since(lease.At) <= s.lease.ttl
}

func (s *Supervisor) holdLease(sha string, at time.Time) {
	if s.lease.warned {
		s.lease.warned = false
		slog.Error("RECOVERED", "kind", "lease", "reason", "lease heartbeat recovered -- dispatch resumed")
	}
	s.lease.sha = sha
	s.lease.renewed = at
}

func (s *Supervisor) leaseLost(msg string) bool {
	if !s.lease.warned {
		s.lease.warned = true
		slog.Error("BLOCKED", "kind", "lease", "reason", msg)
	}
	return false
}
