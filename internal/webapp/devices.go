package webapp

import (
	"log"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// DeviceInfo is what the server knows about one syncing device: self-reported
// name/OS (headers sent by the client), plus what the server itself observed
// (public IP of the last push, last activity, the signed-in account). History
// joins ops against this registry so ops stay small — but it reports only
// id/name/os (historyDevice, history.go): the IP is recorded here, not
// repeated to every project member on every change.
type DeviceInfo struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	OS   string `json:"os,omitempty"`
	User string `json:"user,omitempty"` // account email last seen using this device
	IP   string `json:"ip,omitempty"`   // as observed by the server
	// FirstSeen is when this account was first observed syncing under this id.
	// It is the only ownership fact the hub actually holds: a row is created by
	// the caller's own header, so "does this account have a row" is a question
	// the asking request already answered for itself, while "who was here
	// first" is something a later caller cannot manufacture.
	FirstSeen time.Time `json:"first_seen,omitzero"`
	LastSeen  time.Time `json:"last_seen"`
}

// deviceIDRe is the shape of a device identity. `bdrive` mints 12 hex chars,
// but the id is also a storage key component — journal/<id>.jsonl, the same
// set store.go's journalKeyRe accepts — so anything outside it cannot be a
// real syncing device. Enforcing it here matters because the id doubles as
// the read-heat actor column: free text would let a member hand /heat an
// account email (or any sentence they like) and have the hub serve it back to
// the whole project as a reader.
// One definition, two uses: store.go's journalKeyRe builds the journal key
// shape from it. They drifted — the key had no length cap while an id capped
// at 64 — so a journal key existed that no device row could ever own, the
// ownership gate could never engage on it, and History echoed the 200-char
// string to every member as the device that made each change.
const deviceIDPattern = `[A-Za-z0-9._-]{1,64}`

var deviceIDRe = regexp.MustCompile(`^` + deviceIDPattern + `$`)

func validDeviceID(id string) bool { return deviceIDRe.MatchString(id) }

// printableOnly drops C0/C7F control characters from a client-supplied label.
func printableOnly(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// devKey is the registry's identity: a device id is client-asserted, so it is
// only meaningful inside the account that presented it. Two accounts naming
// the same id hold two separate rows and cannot overwrite or lock out each
// other — which is what makes "first caller owns it" unnecessary.
type devKey struct{ User, ID string }

// DeviceRegistry is the in-memory device table over a MetaStore DeviceRepo.
type DeviceRegistry struct {
	repo DeviceRepo

	mu     sync.Mutex
	byKey  map[devKey]DeviceInfo
	latest map[string]devKey // id → most recently observed row, for display joins
	// lastSav throttles disk writes per row.
	lastSav map[devKey]time.Time
	warned  bool // repo write failure logged once
}

// NewDeviceRegistry builds the registry over a repo, loading its contents.
func NewDeviceRegistry(repo DeviceRepo) (*DeviceRegistry, error) {
	r := &DeviceRegistry{
		repo:    repo,
		byKey:   make(map[devKey]DeviceInfo),
		latest:  make(map[string]devKey),
		lastSav: make(map[devKey]time.Time),
	}
	list, err := repo.Load()
	if err != nil {
		return nil, err
	}
	for _, d := range list {
		k := devKey{d.User, d.ID}
		r.byKey[k] = d
		r.latest[d.ID] = k
	}
	return r, nil
}

// OpenDeviceRegistry loads the file-backed registry at path.
func OpenDeviceRegistry(path string) (*DeviceRegistry, error) {
	return NewDeviceRegistry(newFileDeviceRepo(path))
}

// Observe merges what a request revealed about a device, into the row owned by
// the account that made it. Disk writes are throttled: identity changes
// persist immediately, bare last-seen bumps at most once a minute.
//
// The repo keeps one row per id (its primary key), so what survives a restart
// is the most recent observation. That is a display cache, not the authority:
// MayActAs reads the in-memory rows, and a device re-registers on its very
// next sync cycle.
func (r *DeviceRegistry) Observe(d DeviceInfo) {
	if r == nil || d.ID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	k := devKey{d.User, d.ID}
	if d.User == "" {
		// A caller claiming no account asserts no identity (auth-less hub, or
		// a partial refresh): it merges into whatever row already exists for
		// this id rather than opening a competing, ownerless one.
		if prev, ok := r.latest[d.ID]; ok {
			k = prev
		}
	}
	cur := r.byKey[k]
	changed := cur.ID == "" || cur.Name != d.Name || cur.OS != d.OS || cur.IP != d.IP
	if cur.FirstSeen.IsZero() {
		cur.FirstSeen = time.Now().UTC()
	}
	cur.ID = d.ID
	if d.Name != "" {
		cur.Name = d.Name
	}
	if d.OS != "" {
		cur.OS = d.OS
	}
	if d.User != "" {
		cur.User = d.User
	}
	if d.IP != "" {
		cur.IP = d.IP
	}
	cur.LastSeen = time.Now().UTC()
	r.byKey[k] = cur
	r.latest[d.ID] = k
	if changed || time.Since(r.lastSav[k]) > time.Minute {
		if err := r.repo.Put(cur); err == nil {
			r.lastSav[k] = time.Now()
		} else if !r.warned {
			// Silently discarded, this made a registry that reports a device
			// as observed while nothing about it ever reaches disk. Telemetry
			// still must not fail the request, so it logs once and the next
			// observation retries.
			r.warned = true
			log.Printf("beardrive: device registry write failed (will retry): %v", err)
		}
	}
}

// Get returns the most recently observed row for an id, whoever owns it. It is
// an unscoped display lookup: anything that serves the result to a project
// must use LookupIn instead, or it hands one org's device metadata to another.
func (r *DeviceRegistry) Get(id string) (DeviceInfo, bool) {
	if r == nil {
		return DeviceInfo{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	k, ok := r.latest[id]
	if !ok {
		return DeviceInfo{}, false
	}
	d, ok := r.byKey[k]
	return d, ok
}

// LookupIn returns the row for an id whose owner passes allowed — the join
// every per-project surface uses, so a device belonging to an account outside
// the project's org resolves to nothing instead of leaking its machine name
// and OS (and confirming that the id exists at all).
//
// When several accounts hold rows for one id, the FIRST claim wins. Picking
// the most recently observed row instead meant the second account to name an
// id decided what the whole project sees: one ordinary store request relabels
// a peer's device in History and in /heat?by=device, which is the forgery the
// per-account rekey was supposed to end.
func (r *DeviceRegistry) LookupIn(id string, allowed func(user string) bool) (DeviceInfo, bool) {
	if r == nil {
		return DeviceInfo{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var best DeviceInfo
	found := false
	for k, d := range r.byKey {
		if k.ID != id || !allowed(k.User) {
			continue
		}
		if !found || claimedBefore(d, best) {
			best, found = d, true
		}
	}
	return best, found
}

// claimedBefore orders two rows for the same id by when their account first
// appeared under it. A zero FirstSeen is a row written before the field
// existed, and sorts oldest — the safe direction: an established device keeps
// its identity against a newcomer.
func claimedBefore(a, b DeviceInfo) bool {
	if a.FirstSeen.IsZero() != b.FirstSeen.IsZero() {
		return a.FirstSeen.IsZero()
	}
	return a.FirstSeen.Before(b.FirstSeen)
}

// OwnerOf resolves who a device id belongs to, hub-wide: the account whose row
// for it was created first. It is the WRITE gate's resolver (ownJournal), and
// it differs from LookupIn — the display join — in the two ways that made
// round 4's binding fail:
//
//   - it is not scoped to an org. A claim that disappears when its owner is
//     offboarded hands her journal to whoever is left in the org, and History
//     keeps crediting her.
//   - a row with no owner (a pre-accounts devices.json, or an auth-less
//     observation) claims nothing. Round 4 read "no owner" as "no objection",
//     so on every upgraded hub the binding was off for exactly the established
//     devices.
//
// It returns the owning account and whether the hub has ever seen the id at
// all. The two differ, and the difference is a hole round 4 fell into: a row
// with no owner means a device exists whose account is unknown (known=true,
// owner=""), which is a reason to refuse, not to wave through.
func (r *DeviceRegistry) OwnerOf(id string) (owner string, known bool) {
	if r == nil {
		return "", false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var best DeviceInfo
	found := false
	for k, d := range r.byKey {
		if k.ID != id {
			continue
		}
		known = true
		if k.User == "" {
			continue
		}
		if !found || claimedBefore(d, best) {
			best, found = d, true
		}
	}
	return best.User, known
}

// MayActAs reports whether an account may name a device id as itself: yes if
// this account has been seen syncing under it, and yes if nobody else has —
// an id nobody has claimed is the ordinary case of a device whose telemetry
// arrives before its first push, and refusing it would mean read heat never
// starts. What it refuses is naming a device another account is syncing.
//
// This is the permissive question, and it is the right one for telemetry: a
// squatted id must still count its real owner's reads. Anything that WRITES on
// the strength of a device identity asks OwnerOf instead.
func (r *DeviceRegistry) MayActAs(user, id string) bool {
	if r == nil {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, mine := r.byKey[devKey{user, id}]; mine {
		return true
	}
	for k := range r.byKey {
		if k.ID == id && k.User != "" && k.User != user {
			return false
		}
	}
	return true
}

// ownsDevice reports whether the caller may act AS the device id it named.
//
// Two things have to hold. The id must be a device id at all (validDeviceID):
// it ends up in /heat as an actor, and free text there is an identity the
// caller invented. And no other account may already be syncing under it — the
// registry is populated by /store/* traffic, which is the only thing that
// proves a device exists, and this route never registers anything itself.
// Observing here is what made the round-2 check a one-request speed bump: the
// request it refused registered the refused id to the caller, so the second
// one passed.
//
// A hub with no registry cannot judge, and has no accounts to impersonate
// either (single-volume / auth-less), so it allows.
func (s *Server) ownsDevice(r *http.Request, id string) bool {
	if !validDeviceID(id) {
		return false
	}
	if s.Devices == nil {
		return true
	}
	return s.Devices.MayActAs(s.requestUser(r).Email, id)
}

// refreshDevice records traffic from a device the caller's account ALREADY
// owns, and records nothing at all otherwise.
//
// Creating the first row for an id is a CLAIM: OwnerOf is hub-wide and
// first-claim-wins, and it is what ownJournal consults. So a door that grants
// nothing must claim nothing — otherwise one GET naming an id nobody has
// synced under yet locks that device out of its own journal on every project
// on the hub, which ownJournal's own refusal calls a permanent lockout. Only
// an authorized journal write may claim (handleStorePut, after ownJournal).
func (s *Server) refreshDevice(r *http.Request) {
	if s.Devices == nil {
		return
	}
	owner, known := s.Devices.OwnerOf(r.Header.Get("X-Bdrive-Device"))
	if !known || normEmail(owner) == "" || normEmail(owner) != normEmail(s.requestUser(r).Email) {
		return
	}
	s.observeDevice(r)
}

// observeDevice records the device behind a store-API request, creating the
// row if it is new. Only an authorized journal write calls it: a device is
// something that syncs its own journal, and any other route registering an id
// would let a caller mint — or squat — a device identity out of a header.
func (s *Server) observeDevice(r *http.Request) {
	if s.Devices == nil {
		return
	}
	// The shape check lives here, at the trust boundary, not inside the
	// registry: what a client may assert about itself is a server decision,
	// while the registry (and the repos under it) must store whatever it is
	// handed faithfully.
	id := r.Header.Get("X-Bdrive-Device")
	if !validDeviceID(id) {
		return
	}
	// Name and OS are free text, and they end up in a metadata store where a
	// control character is a divergence between backends (Postgres refuses a
	// NUL in a text column; sqlite and the file backend keep it, so the same
	// device reads back differently depending on the hub's database).
	s.Devices.Observe(DeviceInfo{
		ID:   id,
		Name: printableOnly(r.Header.Get("X-Bdrive-Device-Name")),
		OS:   printableOnly(r.Header.Get("X-Bdrive-Os")),
		User: s.requestUser(r).Email,
		// clientIP, not the raw X-Forwarded-For: the recorded address is the
		// one the server actually saw unless the operator says a proxy fronts
		// this hub, so a client cannot choose what the registry says about it.
		IP: s.clientIP(r),
	})
}

// requestIP is the address the server actually saw on the connection. It
// ignores X-Forwarded-For on purpose: its caller (the CLI device grant) has no
// TrustProxy setting to consult, and an address the client chose, recorded as
// one the server observed, is worse than the proxy's own address recorded
// honestly. Anything that has a *Server uses s.clientIP.
func requestIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// deviceVisibleIn is the predicate LookupIn takes for one project: a device
// row may be joined into this project's surfaces only when the account that
// owns it belongs to the project's org. An unowned row (auth-less hub, or a
// pre-accounts observation) asserts no identity and stays visible; a hub with
// no directory has no orgs to cross in the first place.
func (s *Server) deviceVisibleIn(projectID string) func(string) bool {
	if s.Dir == nil {
		return func(string) bool { return true }
	}
	org := s.orgOf(projectID)
	return func(user string) bool {
		if user == "" {
			return true
		}
		return org != "" && s.Dir.Role(org, user) != ""
	}
}
