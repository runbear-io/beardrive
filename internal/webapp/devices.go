package webapp

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
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

// DeviceRegistry is the file-backed device table (devices.json), same
// load-at-open / atomic-rewrite discipline as the project registry.
type DeviceRegistry struct {
	path string

	mu      sync.Mutex
	byID    map[string]DeviceInfo
	lastSav map[string]time.Time // throttle disk writes per device
}

func OpenDeviceRegistry(path string) (*DeviceRegistry, error) {
	r := &DeviceRegistry{path: path, byID: make(map[string]DeviceInfo), lastSav: make(map[string]time.Time)}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return r, nil
		}
		return nil, err
	}
	var file struct {
		Devices []DeviceInfo `json:"devices"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	for _, d := range file.Devices {
		r.byID[d.ID] = d
	}
	return r, nil
}

func (r *DeviceRegistry) save() error {
	var file struct {
		Devices []DeviceInfo `json:"devices"`
	}
	for _, d := range r.byID {
		file.Devices = append(file.Devices, d)
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(r.path), ".bdrive-tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), r.path)
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
		if r.save() == nil {
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
