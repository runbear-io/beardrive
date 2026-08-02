package webapp

import (
	"context"
	"time"

	"github.com/runbear-io/beardrive/internal/remote"
)

// Storage grants and what they cost.
//
// A presigned upload never passes through the hub, so the hub has exactly two
// moments to account for it: when it hands out the URL, and when it next looks
// at storage. Doing only the first over-charges (20 JSON posts booked 20 GiB
// that never arrived, with no refund path); doing only the second under-checks
// (a caller mints unlimited concurrent grants and blows past the plan's cap
// before any of them commits). So a grant is a RESERVATION:
//
//   - it counts against the cap the moment it is granted — reservedBytes is
//     added to every CheckWrite, so the allowance cannot be oversubscribed;
//   - it is CHARGED only when the bytes are actually in storage, which the hub
//     confirms the next time it talks to that project (reconcileGrants);
//   - it is RELEASED for free when it expires with nothing there, which is
//     what "charged for bytes that arrive" means.
//
// ponytail: reservations live in this process, so a restart forgets the
// outstanding ones — the bytes are still charged the first time anything reads
// or writes the project, because the confirmation is a storage lookup, not a
// memory of the grant. Persisting them buys only the cap check during the
// minutes after a restart.
type grant struct {
	project string
	org     string
	key     string
	size    int64
	expires time.Time
}

// reserve records a presigned grant.
func (s *Server) reserve(project, org, key string, size int64, ttl time.Duration) {
	s.resMu.Lock()
	defer s.resMu.Unlock()
	s.dropExpiredLocked()
	s.grants = append(s.grants, grant{project, org, key, size, time.Now().Add(ttl)})
}

// reservedBytes is what this org has outstanding but unconfirmed.
func (s *Server) reservedBytes(org string) int64 {
	s.resMu.Lock()
	defer s.resMu.Unlock()
	s.dropExpiredLocked()
	var n int64
	for _, g := range s.grants {
		if g.org == org {
			n += g.size
		}
	}
	return n
}

// claimGrant takes over a grant's accounting, reporting whether there was one
// left to take. The caller charges those bytes itself (a commit that read the
// stored size) or has already charged them (a relayed put) — either way the
// reconciler must not charge them a second time.
func (s *Server) claimGrant(project, key string) bool {
	s.resMu.Lock()
	defer s.resMu.Unlock()
	before := len(s.grants)
	s.dropLocked(func(g grant) bool { return g.project == project && g.key == key })
	return len(s.grants) != before
}

// reconcileGrants charges the grants of one project whose bytes have arrived
// and releases the ones that expired without arriving. It is called wherever
// the hub already has this project's storage in hand; with nothing
// outstanding — the ordinary case — it costs nothing at all.
func (s *Server) reconcileGrants(ctx context.Context, project string, be remote.Backend) {
	s.resMu.Lock()
	s.dropExpiredLocked()
	var mine []grant
	for _, g := range s.grants {
		if g.project == project {
			mine = append(mine, g)
		}
	}
	s.resMu.Unlock()
	for _, g := range mine {
		exists, err := be.Exists(ctx, g.key)
		if err != nil || !exists {
			continue // still in flight, or storage is unhappy: ask again later
		}
		s.resMu.Lock()
		before := len(s.grants)
		s.dropLocked(func(o grant) bool { return o.project == g.project && o.key == g.key })
		s.resMu.Unlock()
		if before != len(s.grants) {
			// Charged once, by whichever request got here first.
			s.quota().RecordUsage(g.org, g.size)
		}
	}
}

// dropExpiredLocked releases grants whose URL is no longer usable. Nothing was
// ever charged for them, so releasing is just forgetting.
func (s *Server) dropExpiredLocked() {
	now := time.Now()
	s.dropLocked(func(g grant) bool { return now.After(g.expires) })
}

func (s *Server) dropLocked(match func(grant) bool) {
	kept := s.grants[:0]
	for _, g := range s.grants {
		if !match(g) {
			kept = append(kept, g)
		}
	}
	s.grants = kept
}
