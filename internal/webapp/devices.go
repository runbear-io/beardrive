package webapp

import (
	"net"
	"net/http"
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
	ID       string    `json:"id"`
	Name     string    `json:"name,omitempty"`
	OS       string    `json:"os,omitempty"`
	User     string    `json:"user,omitempty"` // account email last seen using this device
	IP       string    `json:"ip,omitempty"`   // as observed by the server
	LastSeen time.Time `json:"last_seen"`
}

// DeviceRegistry is the in-memory device table over a MetaStore DeviceRepo.
type DeviceRegistry struct {
	repo DeviceRepo

	mu      sync.Mutex
	byID    map[string]DeviceInfo
	lastSav map[string]time.Time // throttle writes per device
}

// NewDeviceRegistry builds the registry over a repo, loading its contents.
func NewDeviceRegistry(repo DeviceRepo) (*DeviceRegistry, error) {
	r := &DeviceRegistry{repo: repo, byID: make(map[string]DeviceInfo), lastSav: make(map[string]time.Time)}
	list, err := repo.Load()
	if err != nil {
		return nil, err
	}
	for _, d := range list {
		r.byID[d.ID] = d
	}
	return r, nil
}

// OpenDeviceRegistry loads the file-backed registry at path.
func OpenDeviceRegistry(path string) (*DeviceRegistry, error) {
	return NewDeviceRegistry(newFileDeviceRepo(path))
}

// Observe merges what a request revealed about a device. Disk writes are
// throttled: identity changes persist immediately, bare last-seen bumps at
// most once a minute.
func (r *DeviceRegistry) Observe(d DeviceInfo) {
	if r == nil || d.ID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := r.byID[d.ID]
	// The registry is hub-wide but device ids are client-asserted, so the
	// first account to be seen using a device owns the row: anyone else
	// naming that id (any project, any org) is claiming a device they do not
	// own, and History joins this table — a forged name would surface in the
	// victim org's change feed. Refuse silently: the caller's request is
	// about something else, and telemetry never fails it. A caller claiming
	// no account at all (auth-less hub) asserts no identity, so it merges as
	// before — it can only refresh what it observed, never re-own the row.
	if cur.User != "" && d.User != "" && d.User != cur.User {
		return
	}
	changed := cur.ID == "" || cur.Name != d.Name || cur.OS != d.OS || cur.User != d.User || cur.IP != d.IP
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
	r.byID[d.ID] = cur
	if changed || time.Since(r.lastSav[d.ID]) > time.Minute {
		if r.repo.Put(cur) == nil {
			r.lastSav[d.ID] = time.Now()
		}
	}
}

func (r *DeviceRegistry) Get(id string) (DeviceInfo, bool) {
	if r == nil {
		return DeviceInfo{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.byID[id]
	return d, ok
}

// requestIP extracts the client address the server actually saw.
func requestIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ownsDevice reports whether the caller may act AS the device id it named.
// The id is a request header, so anyone can name any device: it is only
// theirs if the registry already knows it (from this device's own sync
// traffic, which is what registers a device) and the row is unowned or owned
// by this account. Call it BEFORE observeDevice — observing an unknown id
// creates the row, which would make every id "known".
//
// A hub with no registry cannot judge, and has no accounts to impersonate
// either (single-volume / auth-less), so it allows.
func (s *Server) ownsDevice(r *http.Request, id string) bool {
	if s.Devices == nil {
		return true
	}
	d, ok := s.Devices.Get(id)
	if !ok {
		return false
	}
	return d.User == "" || d.User == s.requestUser(r).Email
}

// observeDevice records the device behind a store-API request.
func (s *Server) observeDevice(r *http.Request) {
	if s.Devices == nil {
		return
	}
	id := r.Header.Get("X-Bdrive-Device")
	if id == "" {
		return
	}
	s.Devices.Observe(DeviceInfo{
		ID:   id,
		Name: r.Header.Get("X-Bdrive-Device-Name"),
		OS:   r.Header.Get("X-Bdrive-Os"),
		User: s.requestUser(r).Email,
		IP:   requestIP(r),
	})
}
