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
// joins ops against this registry, so IPs are real — as the server saw them —
// and ops stay small.
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
