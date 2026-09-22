package webapp

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"strings"
	"sync"
)

/* Carrying change frames between hub processes.

   eventHub.publish fans a frame out to the subscribers THIS process is
   holding, which is the whole story while there is one process. Behind two,
   a write on instance A reaches nobody connected to instance B: their tree
   never updates, an open file never refreshes, and a device's change stream
   sits silent until its own poll notices. Cloud Run offers no cross-instance
   communication, so this has to be carried by something both processes can
   see (docs/hub-load-prd.md Stage 9).

   The seam is deliberately tiny. A relay moves BYTES for a project and knows
   nothing about what is in them: the frame is already JSON that publish
   marshalled, and every client on the far side handles it exactly as if it
   had been published locally.

   Two implementations here — nothing (the default, and what one instance
   wants) and Postgres LISTEN/NOTIFY, which adds no infrastructure because the
   managed hub already runs Postgres for its metadata. Redis becomes the right
   answer somewhere past a handful of instances or ~10k frames a second; the
   interface is the thing that makes that a swap rather than a rewrite. */

// eventRelay carries published frames to the hub's other processes.
//
// publish must not block: it sits on the sync push path, exactly like the
// local fan-out it accompanies, and a metadata store having a bad moment is
// not a reason to fail somebody's write.
type eventRelay interface {
	publish(project string, frame []byte) error
	// start begins delivering frames from OTHER processes. An implementation
	// must never deliver this process's own frames back — they have already
	// been fanned out locally, and a client seeing a change twice refetches
	// twice.
	start(deliver func(project string, frame []byte)) error
	Close() error
}

/* relayOrigin is how a process recognises its own frames coming back.

   Both transports here broadcast to every listener including the publisher —
   Postgres delivers a NOTIFY to a LISTENing connection in the same process,
   and a memory relay has no reason not to. Rather than each implementation
   inventing an echo rule, the origin is prefixed to the wire payload and
   stripped on the way out, so "did I send this?" is one comparison in one
   place.

   Random per process, not per Server: two Servers in one test process
   sharing a relay are exactly the two-instance case being tested, and giving
   them one origin would make each ignore the other. */
type relayOrigin string

func newRelayOrigin() relayOrigin {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return relayOrigin("origin-fallback")
	}
	return relayOrigin(hex.EncodeToString(b))
}

// encodeRelay packs a frame for the wire: origin, project, then the JSON.
// Tab-separated because neither an origin (hex) nor a project id contains one,
// and a length-prefixed format would buy nothing a Cut does not.
func encodeRelay(origin relayOrigin, project string, frame []byte) string {
	return string(origin) + "\t" + project + "\t" + string(frame)
}

// decodeRelay reverses it, and reports whether the frame came from somewhere
// else. A malformed payload is dropped rather than guessed at.
func decodeRelay(mine relayOrigin, payload string) (project string, frame []byte, ok bool) {
	origin, rest, found := strings.Cut(payload, "\t")
	if !found {
		return "", nil, false
	}
	project, body, found := strings.Cut(rest, "\t")
	if !found {
		return "", nil, false
	}
	if relayOrigin(origin) == mine {
		return "", nil, false // our own write, already fanned out locally
	}
	return project, []byte(body), true
}

/* resyncFrame is what a relay sends when a real frame will not fit.

   Every transport has a payload ceiling — Postgres NOTIFY stops at 8000
   bytes, and a change frame naming 64 long paths can exceed that. Truncating
   the path list would be a lie (a client would believe it had been told about
   every change), so the honest move is the one the local fan-out already
   makes for a subscriber that fell behind: say "resync" and let the client
   refetch. A client that missed one path and a client that missed fifty need
   the same thing. */
func resyncFrame() []byte {
	frame, _ := json.Marshal(changeEvent{Type: "resync"})
	return frame
}

// maxRelayFrame is the largest frame a relay will carry verbatim. Set by the
// tightest transport (Postgres NOTIFY at 8000 bytes) with room for the origin
// and project prefix, so a frame that travels on one relay travels on all of
// them and behaviour does not change with the backend.
const maxRelayFrame = 7 * 1024

// ---- memory relay ---------------------------------------------------------

/* memRelay connects processes that share an address space, which in practice
   means a test. It is not a shortcut around the real thing: the cross-instance
   PROPERTIES — a frame reaching a hub that did not publish it, and a hub not
   seeing its own twice — are the same ones the Postgres relay has to satisfy,
   and they are far cheaper to pin here than against a database. */
type memRelay struct {
	mu      sync.Mutex
	members []*memRelayMember
}

type memRelayMember struct {
	bus     *memRelay
	origin  relayOrigin
	deliver func(project string, frame []byte)
}

func newMemRelay() *memRelay { return &memRelay{} }

// join returns one process's handle on the shared bus.
func (b *memRelay) join() *memRelayMember {
	m := &memRelayMember{bus: b, origin: newRelayOrigin()}
	b.mu.Lock()
	b.members = append(b.members, m)
	b.mu.Unlock()
	return m
}

func (m *memRelayMember) publish(project string, frame []byte) error {
	if len(frame) > maxRelayFrame {
		frame = resyncFrame()
	}
	payload := encodeRelay(m.origin, project, frame)
	m.bus.mu.Lock()
	peers := append([]*memRelayMember(nil), m.bus.members...)
	m.bus.mu.Unlock()
	for _, p := range peers {
		p.receive(payload)
	}
	return nil
}

func (m *memRelayMember) receive(payload string) {
	if m.deliver == nil {
		return
	}
	if project, frame, ok := decodeRelay(m.origin, payload); ok {
		m.deliver(project, frame)
	}
}

func (m *memRelayMember) start(deliver func(project string, frame []byte)) error {
	m.deliver = deliver
	return nil
}

func (m *memRelayMember) Close() error {
	m.bus.mu.Lock()
	defer m.bus.mu.Unlock()
	for i, p := range m.bus.members {
		if p == m {
			m.bus.members = append(m.bus.members[:i], m.bus.members[i+1:]...)
			break
		}
	}
	return nil
}

// ---- wiring ---------------------------------------------------------------

// UseRelay attaches a cross-process relay to this hub's change stream. Safe to
// call before or after the fan-out has been built; a nil relay leaves the hub
// exactly as it is, which is what a single-instance deployment wants.
func (s *Server) UseRelay(r eventRelay) {
	if r == nil {
		return
	}
	h := s.events()
	h.mu.Lock()
	h.relay = r
	h.mu.Unlock()
	if err := r.start(func(project string, frame []byte) { h.fanout(project, frame) }); err != nil {
		log.Printf("beardrive: change relay could not start, this process will only "+
			"notify the clients it is holding: %v", err)
	}
}
