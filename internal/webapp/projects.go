package webapp

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

// Project is one synced project hosted by this server. Its storage lives
// under <root>/<id>/ in the object store; the id is permanent, the name is a
// renameable label.
type Project struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Org     string    `json:"org,omitempty"` // owning organization
	Created time.Time `json:"created"`
}

var projectIDRe = regexp.MustCompile(`^p-[0-9a-f]{8}$`)

// ProjectDB is the server's project registry, persisted as a JSON file that
// is loaded on open and rewritten atomically on every change.
type ProjectDB struct {
	path string

	mu   sync.Mutex
	byID map[string]Project
}

// OpenProjectDB loads the registry at path; a missing file is an empty
// registry.
func OpenProjectDB(path string) (*ProjectDB, error) {
	db := &ProjectDB{path: path, byID: make(map[string]Project)}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return db, nil
		}
		return nil, err
	}
	var file struct {
		Projects []Project `json:"projects"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	for _, p := range file.Projects {
		db.byID[p.ID] = p
	}
	return db, nil
}

// save persists the registry. Callers hold mu.
func (db *ProjectDB) save() error {
	list := db.list()
	data, err := json.MarshalIndent(struct {
		Projects []Project `json:"projects"`
	}{list}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(db.path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(db.path), ".bdrive-tmp-*")
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
	return os.Rename(tmp.Name(), db.path)
}

// list returns projects sorted by name. Callers hold mu.
func (db *ProjectDB) list() []Project {
	out := make([]Project, 0, len(db.byID))
	for _, p := range db.byID {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (db *ProjectDB) List() []Project {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.list()
}

func (db *ProjectDB) Get(id string) (Project, bool) {
	db.mu.Lock()
	defer db.mu.Unlock()
	p, ok := db.byID[id]
	return p, ok
}

// GetOrCreate returns the project with the given name in the org, creating
// it (with a fresh id) if none exists. Names are matched exactly, scoped to
// the org: two organizations can each have a "wiki".
func (db *ProjectDB) GetOrCreate(name, org string) (Project, bool, error) {
	name = trimName(name)
	if name == "" {
		return Project{}, false, fmt.Errorf("project name must not be empty")
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	for _, p := range db.byID {
		if p.Name == name && p.Org == org {
			return p, false, nil
		}
	}
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return Project{}, false, err
	}
	p := Project{ID: "p-" + hex.EncodeToString(buf[:]), Name: name, Org: org, Created: time.Now().UTC()}
	db.byID[p.ID] = p
	if err := db.save(); err != nil {
		delete(db.byID, p.ID)
		return Project{}, false, err
	}
	return p, true, nil
}

// Rename changes a project's display name (its id and storage are permanent).
func (db *ProjectDB) Rename(id, name string) error {
	name = trimName(name)
	if name == "" {
		return fmt.Errorf("project name must not be empty")
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	p, ok := db.byID[id]
	if !ok {
		return fmt.Errorf("no such project %q", id)
	}
	for _, other := range db.byID {
		if other.ID != id && other.Name == name && other.Org == p.Org {
			return fmt.Errorf("a project named %q already exists in this organization", name)
		}
	}
	p.Name = name
	db.byID[id] = p
	return db.save()
}

// Delete removes a project from the registry. Its storage prefix (blobs,
// journals) is left in the object store — the id is retired, not scrubbed —
// so the caller decides whether to reclaim that space out of band.
func (db *ProjectDB) Delete(id string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if _, ok := db.byID[id]; !ok {
		return fmt.Errorf("no such project %q", id)
	}
	delete(db.byID, id)
	return db.save()
}

// SetOrg moves a project into an org (used by the startup migration).
func (db *ProjectDB) SetOrg(id, org string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	p, ok := db.byID[id]
	if !ok {
		return fmt.Errorf("no such project %q", id)
	}
	p.Org = org
	db.byID[id] = p
	return db.save()
}

func trimName(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' {
			continue
		}
		out = append(out, r)
	}
	for len(out) > 0 && out[0] == ' ' {
		out = out[1:]
	}
	for len(out) > 0 && out[len(out)-1] == ' ' {
		out = out[:len(out)-1]
	}
	if len(out) > 128 {
		out = out[:128]
	}
	return string(out)
}
