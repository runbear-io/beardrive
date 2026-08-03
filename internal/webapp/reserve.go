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

// reserve records a presigned grant unconditionally. Every request path goes
// through reserveIfFits instead, which is the same thing plus the cap check in
// the same critical section.
func (s *Server) reserve(project, org, key string, size int64, ttl time.Duration) {
	s.resMu.Lock()
	defer s.resMu.Unlock()
	s.dropStaleLocked()
	s.grants = append(s.grants, grant{project, org, key, size, time.Now().Add(ttl)})
}

// reserveIfFits asks the quota provider about this write plus everything this
// org already has outstanding, and books the grant only if the outstanding
// total has not grown in the meantime. Read-check-act with the ledger unlocked
// in between let every concurrent caller see the same zero reservation, so N
// grants that each fit the allowance were all signed against an allowance that
// fits one — round 2's seat-check race, on the new ledger. Compare-and-set
// closes that without holding the lock across CheckWrite: the provider is a
// third-party seam (a network hop to a billing service), and resMu is hub-wide,
// so waiting on it there stalls every other project's sync cycle.
func (s *Server) reserveIfFits(project, org, key string, size int64, ttl time.Duration) error {
	for {
		s.resMu.Lock()
		s.dropStaleLocked()
		outstanding := s.outstandingLocked(org)
		s.resMu.Unlock()

		if err := s.quota().CheckWrite(org, size+outstanding); err != nil {
			return err
		}

		s.resMu.Lock()
		s.dropStaleLocked()
		if s.outstandingLocked(org) > outstanding {
			// Someone booked while we were asking, so the answer we hold was
			// about a smaller total: ask again. This terminates — the retry
			// happens only because another caller made progress, and a growing
			// total is what makes CheckWrite refuse.
			s.resMu.Unlock()
			continue
		}
		s.grants = append(s.grants, grant{project, org, key, size, time.Now().Add(ttl)})
		s.resMu.Unlock()
		return nil
	}
}

// reservedBytes is what this org has outstanding but unconfirmed. An expired
// grant no longer holds capacity (its URL cannot be used) even though the
// ledger still remembers it until storage has been asked.
func (s *Server) reservedBytes(org string) int64 {
	s.resMu.Lock()
	defer s.resMu.Unlock()
	s.dropStaleLocked()
	return s.outstandingLocked(org)
}

func (s *Server) outstandingLocked(org string) int64 {
	now := time.Now()
	var n int64
	for _, g := range s.grants {
		if g.org == org && !now.After(g.expires) {
			n += g.size
		}
	}
	return n
}

// claimGrant takes over a grant's accounting, reporting whether there was one
// left to take. The caller charges those bytes itself (a commit that read the
// stored size) or has already charged them (a relayed put) — either way the
// reconciler must not charge them a second time. It is also how a grant that
// was reserved but never actually handed out is given back.
func (s *Server) claimGrant(project, key string) bool {
	s.resMu.Lock()
	defer s.resMu.Unlock()
	before := len(s.grants)
	s.dropLocked(func(g grant) bool { return g.project == project && g.key == key })
	return len(s.grants) != before
}

// reconcileGrants charges the grants of one project whose bytes have arrived
// and retires the ones that expired without arriving. It is called wherever
// the hub already has this project's storage in hand; with nothing
// outstanding — the ordinary case — it costs nothing at all.
func (s *Server) reconcileGrants(ctx context.Context, project string, be remote.Backend) {
	s.resMu.Lock()
	s.dropStaleLocked()
	var mine []grant
	for _, g := range s.grants {
		if g.project == project {
			mine = append(mine, g)
		}
	}
	s.resMu.Unlock()
	for _, g := range mine {
		exists, err := be.Exists(ctx, g.key)
		if err != nil {
			continue // storage is unhappy: ask again later
		}
		if !exists && !time.Now().After(g.expires) {
			continue // still in flight
		}
		// Either the bytes are there (charge them) or the URL is dead and
		// nothing arrived (release for free). Both retire the grant, and the
		// decision is read under the same lock that made it: comparing the
		// length after unlocking was a data race on the billing ledger, and a
		// concurrent reserve that restored the length silently dropped the
		// charge.
		s.resMu.Lock()
		before := len(s.grants)
		s.dropLocked(func(o grant) bool { return o.project == g.project && o.key == g.key })
		retired := before != len(s.grants)
		s.resMu.Unlock()
		if retired && exists {
			// Charged once, by whichever request got here first.
			s.quota().RecordUsage(g.org, g.size)
		}
	}
}

// dropStaleLocked forgets grants nothing will ever reconcile. Expiry alone is
// NOT the trigger: an uploader that pushes through its presigned URL and then
// goes quiet leaves bytes in storage that only the grant remembers, so
// dropping on expiry made them free forever. A grant is retired by
// reconcileGrants, which asks storage first; this is only the backstop for a
// project nothing touches again, so the ledger cannot grow without bound.
const grantReconcileGrace = 24 * time.Hour

func (s *Server) dropStaleLocked() {
	cutoff := time.Now().Add(-grantReconcileGrace)
	s.dropLocked(func(g grant) bool { return g.expires.Before(cutoff) })
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
