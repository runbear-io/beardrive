package webapp

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/runbear-io/beardrive/internal/remote"
)

/* Which process writes a file while it is being co-edited.

   max_instance_count = 1 was never one process: Cloud Run keeps the old
   revision's instance serving its open websockets through every rollout,
   and overshoots the cap under load. Two hubs each holding a document for
   the same file — seeded from storage, each writing it back — is therefore
   the ordinary state, not a failure, and the one writer has to be chosen
   somewhere both can see. That is the project's own storage, with the
   compare-and-swap the journal append already depends on.

   One lease per room, held by the process that writes its snapshots and
   renewed while the room lives. With a relay (CollabConfig) every process
   hosts the room and the lease only picks the writer; without one the
   process that does not hold it refuses the room, and the browser's own
   retry carries it to the one that does. A backend without compare-and-swap
   (file://) is one process by definition and always holds it. */

// leaseTTL is how long a lease outlives a holder that stopped renewing —
// a process SIGKILLed with the room open. A var so a test need not wait it
// out.
var leaseTTL = 30 * time.Second

type leaseRecord struct {
	Holder string    `json:"holder"`
	Until  time.Time `json:"until"`
}

type roomLease struct {
	version remote.Version
	until   time.Time
}

func leaseKey(path string) string { return "lease/" + path }

// nodeID names this process on a lease. Not the device id: every instance
// of the managed hub shares one, derived from the storage root.
func (s *Server) nodeID() string {
	s.nodeOnce.Do(func() {
		var b [8]byte
		_, _ = rand.Read(b[:])
		s.node = hex.EncodeToString(b[:])
	})
	return s.node
}

// holdLease reports whether this process writes the room, taking the lease
// if it is free or expired and renewing it once half its life has passed.
func (s *Server) holdLease(ctx context.Context, room string) bool {
	project, path, ok := strings.Cut(room, "/")
	if !ok {
		return false
	}
	be, ok := s.projectBackend(project)
	if !ok {
		return true
	}
	cond, ok := be.(remote.ConditionalPutter)
	if !ok {
		return true
	}
	r := s.rooms()
	// ponytail: one lock across every room's lease round-trip; per-room locks if hundreds of live rooms contend
	r.leaseMu.Lock()
	defer r.leaseMu.Unlock()
	now := time.Now()
	held := r.leases[room]
	if held != nil && now.Before(held.until.Add(-leaseTTL/2)) {
		return true
	}
	expect := remote.VersionAbsent
	if held != nil {
		expect = held.version
	} else {
		rc, v, err := cond.GetVersioned(ctx, leaseKey(path))
		switch {
		case err == nil:
			var rec leaseRecord
			_ = json.NewDecoder(rc).Decode(&rec) // unreadable reads as expired
			rc.Close()
			if rec.Holder != s.nodeID() && now.Before(rec.Until) {
				return false
			}
			expect = v
		case !remote.IsNotExist(err):
			log.Printf("bdrive: reading lease for %s: %v", room, err)
			return false
		}
	}
	body, _ := json.Marshal(leaseRecord{Holder: s.nodeID(), Until: now.Add(leaseTTL)})
	v, err := cond.PutIf(ctx, leaseKey(path), bytes.NewReader(body), int64(len(body)), expect)
	if err != nil {
		delete(r.leases, room)
		if !errors.Is(err, remote.ErrVersionMismatch) {
			log.Printf("bdrive: taking lease for %s: %v", room, err)
		}
		return false
	}
	r.leases[room] = &roomLease{version: v, until: now.Add(leaseTTL)}
	return true
}

// releaseLease gives a room's lease up as the room ends, so the next process
// to open the file does not wait out the TTL. Best effort: an expired lease
// is the same thing, later.
func (s *Server) releaseLease(room string) {
	project, path, _ := strings.Cut(room, "/")
	r := s.rooms()
	r.leaseMu.Lock()
	defer r.leaseMu.Unlock()
	held := r.leases[room]
	if held == nil {
		return
	}
	delete(r.leases, room)
	be, ok := s.projectBackend(project)
	if !ok {
		return
	}
	cond, ok := be.(remote.ConditionalPutter)
	if !ok {
		return
	}
	body, _ := json.Marshal(leaseRecord{Holder: s.nodeID()})
	_, _ = cond.PutIf(context.Background(), leaseKey(path), bytes.NewReader(body), int64(len(body)), held.version)
}

// keepLease renews a room's lease for as long as the room lives — a quiet
// room must not lose its writer to a joiner on another process — and, on a
// process that does not hold it but could write, keeps asking, which is how
// the writer's job passes on when the holder's room ends.
func (s *Server) keepLease(room string) {
	r := s.rooms()
	r.mu.Lock()
	if r.renewals[room] != nil {
		r.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	r.renewals[room] = stop
	r.mu.Unlock()
	go func() {
		t := time.NewTicker(leaseTTL / 3)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				_, who, ok := r.get(room)
				if ok && (who.Email != "" || s.holdsLease(room)) {
					s.holdLease(context.Background(), room)
				}
			}
		}
	}()
}

func (s *Server) holdsLease(room string) bool {
	r := s.rooms()
	r.leaseMu.Lock()
	defer r.leaseMu.Unlock()
	return r.leases[room] != nil
}
