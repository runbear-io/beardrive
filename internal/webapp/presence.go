package webapp

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
)

// Who else is here. Clients heartbeat the path they are looking at; the hub
// keeps that in memory and fans the roster out on the change stream that
// already exists (events.go).
//
// Deliberately NOT persisted, and never a MetaStore repo: presence is true for
// fifteen seconds and then it is a lie, so writing it down would only create
// something to serve staler than the thing it describes. A hub restart empties
// it and the next heartbeat rebuilds it.
//
// It is walled by the same proj(PermRead) as the stream itself, so a roster
// only ever reaches members of the project it describes. What it publishes is
// a display name and a path — the same pair History already shows every member
// of a project — and nothing else: no device id, no share token, no IP.

const (
	// presenceTTL is how long a heartbeat vouches for someone. Clients beat
	// well inside it, so a missed beat is a network hiccup and not a
	// disappearance.
	presenceTTL = 15 * time.Second
	// maxPresencePerProject bounds one project's roster. Far above any real
	// team; it exists so the map cannot be grown without end by a member with
	// a script.
	maxPresencePerProject = 256
	// presencePathMax bounds the path a client claims to be looking at. It is
	// echoed to every other member, so it is untrusted text like any other.
	presencePathMax = 1024
	// presenceAgentIdle is how long an opted-in view stays reportable to the
	// viewer's own agent (presence/me) after the last announce that carried
	// agent:true. Past it, an overnight tab is not "what I'm looking at".
	presenceAgentIdle = 30 * time.Minute
)

type presenceEntry struct {
	name string
	path string
	seen time.Time
	// agent/agentSeen: the viewer opted in to sharing this view with their
	// OWN agent (presence/me). Never serialized — person is built from name
	// and path only — so the roster fans out exactly what it always did.
	agent     bool
	agentSeen time.Time
}

// person is one roster row as clients receive it.
type person struct {
	Name string `json:"name"`
	Path string `json:"path,omitempty"`
}

type presenceHub struct {
	mu sync.Mutex
	// project id -> actor key -> entry. The actor key is an account email (or
	// a device id on an auth-less hub); it is a MAP KEY only and is never
	// serialized — see roster.
	at map[string]map[string]presenceEntry
}

func (s *Server) presence() *presenceHub {
	s.presOnce.Do(func() {
		s.pres = &presenceHub{at: map[string]map[string]presenceEntry{}}
	})
	return s.pres
}

// mark records that actor is looking at path, and reports the roster and
// whether it changed.
//
// Expiry is lazy — computed here rather than by a sweeper goroutine. Nobody
// needs to be told a roster shrank except the people still in it, and they are
// exactly the ones still heartbeating, so the next beat notices within its own
// interval. A ticker would buy nothing and would have to be shut down.
func (h *presenceHub) mark(project, actor, name, path string, now time.Time, stillHere func(string) bool) ([]person, bool, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	room := h.at[project]
	if room == nil {
		room = map[string]presenceEntry{}
		h.at[project] = room
	}
	prev, existed := room[actor]
	changed := !existed || prev.path != path || prev.name != name
	for k, e := range room {
		if k == actor || now.Sub(e.seen) <= presenceTTL {
			continue
		}
		// Still holding a change stream? Then they are here, whatever the
		// clock says. The TTL used to be the only evidence of liveness because
		// the only evidence WAS a heartbeat; the hub now holds the socket, and
		// a socket is better evidence than a timer. Without this the removal
		// of the beat would expire every reader 15 seconds after they arrived.
		if stillHere != nil && stillHere(k) {
			continue
		}
		delete(room, k)
		changed = true
	}
	if !existed && len(room) >= maxPresencePerProject {
		return rosterOf(room), false, false // full: this beat vouches for nobody
	}
	room[actor] = presenceEntry{name: name, path: path, seen: now}
	return rosterOf(room), changed, true
}

// setAgent records the opt-in flag on an actor's existing entry. Kept out of
// mark (and of its changed report) on purpose: a toggle flip or a refocus
// re-announce changes nothing any teammate sees, so it must publish no frame.
func (h *presenceHub) setAgent(project, actor string, agent bool, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	e, ok := h.at[project][actor]
	if !ok {
		return // mark refused it (room full): nothing to annotate
	}
	e.agent, e.agentSeen = agent, time.Time{}
	if agent {
		e.agentSeen = now
	}
	h.at[project][actor] = e
}

// self returns one actor's own entry.
func (h *presenceHub) self(project, actor string) (presenceEntry, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	e, ok := h.at[project][actor]
	return e, ok
}

// drop removes an actor immediately, for a client that says it is leaving.
func (h *presenceHub) drop(project, actor string) ([]person, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	room := h.at[project]
	if room == nil {
		return nil, false
	}
	if _, ok := room[actor]; !ok {
		return rosterOf(room), false
	}
	delete(room, actor)
	if len(room) == 0 {
		delete(h.at, project)
	}
	return rosterOf(room), true
}

// markPresence records that actor is looking at path and publishes the roster
// if it moved. The Server-level wrapper exists because liveness is no longer a
// property of the presence map alone: see stillHere.
func (s *Server) markPresence(project, actor, name, path string, now time.Time) ([]person, bool, bool) {
	// Liveness comes from the fan-out: anyone holding a stream on this project
	// is here. Passed as a predicate rather than a snapshot so the presence
	// lock is never held across the event hub's.
	people, changed, ok := s.presence().mark(project, actor, name, path, now,
		func(a string) bool { return s.events().hasActor(project, a) })
	if changed {
		s.events().publish(project, changeEvent{Type: "presence", People: people})
	}
	return people, changed, ok
}

// streamGone is called when a change stream closes. If that was the actor's
// last stream on this project, they are no longer here — which is the whole
// replacement for the heartbeat's TTL.
func (s *Server) streamGone(project, actor string) {
	if actor == "" || s.events().hasActor(project, actor) {
		return // another tab or device of the same person is still holding one
	}
	if people, changed := s.presence().drop(project, actor); changed {
		s.events().publish(project, changeEvent{Type: "presence", People: people})
	}
}

// rosterOf renders a room. The actor key never appears in the result: it is an
// email, and the roster goes to every member of the project.
func rosterOf(room map[string]presenceEntry) []person {
	out := make([]person, 0, len(room))
	for _, e := range room {
		out = append(out, person{Name: e.name, Path: e.path})
	}
	// Sorted so an unchanged roster serializes identically and the frontend's
	// structural sharing sees no update: Go map order is deliberately random.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Path < out[j].Path
	})
	return out
}

// presenceIdentity is WHO a request is, for the roster: a stable actor key and
// the display name everyone else sees. An auth-less hub (the plain-folder
// viewer) has no account, so it falls back to the device — and to nothing, in
// which case there is no one to report and the caller does nothing rather than
// inventing a "someone".
func presenceIdentity(s *Server, r *http.Request) (actor, name string) {
	u := s.requestUser(r)
	actor, name = u.Email, u.Name
	if actor == "" {
		actor = deviceID(r)
		name = actor
	}
	if name == "" {
		name = u.Email // History shows the address too when there is no name
	}
	return actor, name
}

// presenceActor is presenceIdentity's key half, for callers that only need to
// know which roster row a connection belongs to.
func presenceActor(s *Server, r *http.Request) string {
	actor, _ := presenceIdentity(s, r)
	return actor
}

// handlePresence serves POST {prefix}presence — one heartbeat.
func (s *Server) handlePresence(v *volume, w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path  string `json:"path"`
		Leave bool   `json:"leave,omitempty"`
		Agent bool   `json:"agent,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	actor, name := presenceIdentity(s, r)
	if actor == "" {
		writeJSON(w, map[string]any{"ok": true, "people": []person{}})
		return
	}
	project := projectID(r)

	if req.Leave {
		people, changed := s.presence().drop(project, actor)
		if changed {
			s.events().publish(project, changeEvent{Type: "presence", People: people})
		}
		writeJSON(w, map[string]any{"ok": true, "people": people})
		return
	}

	// A claimed path is untrusted text echoed to every other member. It is
	// display-only — nothing is looked up by it — but it still goes through
	// the same rule every other path in this codebase does, rather than a
	// fourth private copy of "looks fine to me".
	path := req.Path
	if len(path) > presencePathMax || (path != "" && !journal.SafePath(path)) {
		path = ""
	}
	now := time.Now()
	people, _, _ := s.markPresence(project, actor, name, path, now)
	// Every announce states the flag; a missing one means off, so nothing is
	// carried over from an earlier opt-in.
	s.presence().setAgent(project, actor, req.Agent, now)
	writeJSON(w, map[string]any{"ok": true, "people": people})
}

// handlePresenceMe serves GET {prefix}presence/me — the doc the CALLER has
// open in the hub, for their own agent's hook context (bdrive sync --hook).
//
// Account-only: presenceIdentity falls back to the X-Bdrive-Device header on an
// auth-less hub, and anyone can set a header, so a caller without an account
// is answered with nothing. The key is the caller's own email, so no request
// can name someone else's entry. Answers {} unless the viewer opted in, the
// view is under presenceAgentIdle old, and the viewer is still here — expiry in
// mark is lazy (it runs on someone else's announce), so liveness is re-checked
// on read rather than trusted from the map.
func (s *Server) handlePresenceMe(v *volume, w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.presenceMe(projectID(r), s.requestUser(r).Email, time.Now()))
}

func (s *Server) presenceMe(project, email string, now time.Time) map[string]string {
	if email == "" {
		return map[string]string{}
	}
	e, ok := s.presence().self(project, email)
	if !ok || !e.agent || e.path == "" || now.Sub(e.agentSeen) > presenceAgentIdle {
		return map[string]string{}
	}
	if now.Sub(e.seen) > presenceTTL && !s.events().hasActor(project, email) {
		return map[string]string{}
	}
	return map[string]string{"path": e.path, "seen": e.agentSeen.UTC().Format(time.RFC3339)}
}
