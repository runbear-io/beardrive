package webapp

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Project is one synced project hosted by this server. Its storage lives
// under <root>/<id>/ in the object store; the id is permanent, the name is a
// renameable label.
type Project struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Org         string    `json:"org,omitempty"` // owning organization
	Created     time.Time `json:"created"`
	Description string    `json:"description,omitempty"` // optional one-line subtitle
	Icon        string    `json:"icon,omitempty"`        // optional lucide icon name
	// Creator is the account that first created the project; it gets an
	// explicit admin grant at creation. Empty on projects that predate
	// per-project permissions — those are governed by org owners.
	Creator string `json:"creator,omitempty"`
	// Default is the level every org member gets without an explicit grant.
	// Empty means write: the historical behavior, so no row needs migrating.
	Default string `json:"default,omitempty"`
	// Perms are the explicit grants, lowercase email → level.
	Perms map[string]string `json:"perms,omitempty"`
}

// level is the project's effective default level for org members.
func (p Project) level() string {
	if p.Default == "" {
		return PermWrite
	}
	return p.Default
}

var projectIDRe = regexp.MustCompile(`^p-[0-9a-f]{8}$`)

// iconRe validates the *shape* of an icon name only. The list of icons a
// project may pick from lives in the frontend (shell.tsx's PROJECT_ICONS) —
// the server stores whatever kebab-case name it's given and the UI falls back
// to a placeholder for anything it doesn't know, so adding an icon never
// needs a server change.
var iconRe = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

const (
	maxNameLen = 120
	maxDescLen = 280
)

// ProjectDB is the server's project registry: an in-memory index over a
// MetaStore ProjectRepo. Reads are served from memory; every change is
// persisted as one record through the repo (file or SQL).
type ProjectDB struct {
	repo ProjectRepo

	mu   sync.Mutex
	byID map[string]Project
}

// NewProjectDB builds the registry over a repo, loading its current contents.
func NewProjectDB(repo ProjectRepo) (*ProjectDB, error) {
	db := &ProjectDB{repo: repo, byID: make(map[string]Project)}
	list, err := repo.Load()
	if err != nil {
		return nil, err
	}
	for _, p := range list {
		db.byID[p.ID] = p
	}
	return db, nil
}

// OpenProjectDB loads the file-backed registry at path (a missing file is an
// empty registry) — the zero-dependency default.
func OpenProjectDB(path string) (*ProjectDB, error) {
	return NewProjectDB(newFileProjectRepo(path))
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
	if err := db.repo.Put(p); err != nil {
		delete(db.byID, p.ID)
		return Project{}, false, err
	}
	return p, true, nil
}

// Update changes a project's editable metadata. Each field is a pointer so
// that "absent" (nil, leave alone) is distinguishable from "present and
// empty" (clear it) — the whole point of a partial update. One lock, one
// repo write, whatever the caller changed.
func (db *ProjectDB) Update(id string, name, description, icon *string) error {
	var newName, newDesc, newIcon string
	if name != nil {
		newName = trimText(*name, maxNameLen+1)
		if newName == "" {
			return fmt.Errorf("project name must not be empty")
		}
		if utf8.RuneCountInString(newName) > maxNameLen {
			return fmt.Errorf("project name must be at most %d characters", maxNameLen)
		}
	}
	if description != nil {
		newDesc = trimText(*description, maxDescLen+1)
		if utf8.RuneCountInString(newDesc) > maxDescLen {
			return fmt.Errorf("project description must be at most %d characters", maxDescLen)
		}
	}
	if icon != nil {
		newIcon = strings.TrimSpace(*icon)
		if newIcon != "" && !iconRe.MatchString(newIcon) {
			return fmt.Errorf("invalid icon name %q", newIcon)
		}
	}

	db.mu.Lock()
	defer db.mu.Unlock()
	p, ok := db.byID[id]
	if !ok {
		return fmt.Errorf("no such project %q", id)
	}
	if name != nil {
		for _, other := range db.byID {
			if other.ID != id && other.Name == newName && other.Org == p.Org {
				return fmt.Errorf("a project named %q already exists in this organization", newName)
			}
		}
		p.Name = newName
	}
	if description != nil {
		p.Description = newDesc
	}
	if icon != nil {
		p.Icon = newIcon
	}
	db.byID[id] = p
	return db.repo.Put(p)
}

// Rename changes a project's display name (its id and storage are permanent).
func (db *ProjectDB) Rename(id, name string) error {
	return db.Update(id, &name, nil, nil)
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
	return db.repo.Delete(id)
}

// SetCreator records who created a project (and is its first admin).
func (db *ProjectDB) SetCreator(id, email string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	p, ok := db.byID[id]
	if !ok {
		return fmt.Errorf("no such project %q", id)
	}
	p.Creator = normEmail(email)
	db.byID[id] = p
	return db.repo.Put(p)
}

// SetDefault sets the level org members get without an explicit grant.
func (db *ProjectDB) SetDefault(id, level string) error {
	if !validLevel(level) || level == PermAdmin {
		return fmt.Errorf("invalid default level %q", level)
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	p, ok := db.byID[id]
	if !ok {
		return fmt.Errorf("no such project %q", id)
	}
	p.Default = level
	db.byID[id] = p
	return db.repo.Put(p)
}

// SetPerm grants one account an explicit level on the project. Demoting the
// last explicit admin is refused, the same shape as OrgDB's last-owner rule:
// a project must keep someone who can administer it (org owners aside, who
// are implicitly admin and never appear in this list).
func (db *ProjectDB) SetPerm(id, email, level string) error {
	if !validLevel(level) {
		return fmt.Errorf("invalid level %q", level)
	}
	e := normEmail(email)
	if e == "" {
		return fmt.Errorf("email must not be empty")
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	p, ok := db.byID[id]
	if !ok {
		return fmt.Errorf("no such project %q", id)
	}
	if level != PermAdmin && p.Perms[e] == PermAdmin && adminCount(p) <= 1 {
		return fmt.Errorf("cannot demote the last project admin")
	}
	perms := make(map[string]string, len(p.Perms)+1)
	for k, v := range p.Perms {
		perms[k] = v
	}
	perms[e] = level
	p.Perms = perms
	db.byID[id] = p
	return db.repo.Put(p)
}

// ClearPerm drops an explicit grant, reverting the account to the default.
func (db *ProjectDB) ClearPerm(id, email string) error {
	e := normEmail(email)
	db.mu.Lock()
	defer db.mu.Unlock()
	p, ok := db.byID[id]
	if !ok {
		return fmt.Errorf("no such project %q", id)
	}
	if _, has := p.Perms[e]; !has {
		return fmt.Errorf("%s has no permission set on this project", email)
	}
	if p.Perms[e] == PermAdmin && adminCount(p) <= 1 {
		return fmt.Errorf("cannot remove the last project admin")
	}
	perms := make(map[string]string, len(p.Perms))
	for k, v := range p.Perms {
		if k != e {
			perms[k] = v
		}
	}
	p.Perms = perms
	db.byID[id] = p
	return db.repo.Put(p)
}

// adminCount counts explicit admin grants on a project.
func adminCount(p Project) int {
	n := 0
	for _, l := range p.Perms {
		if l == PermAdmin {
			n++
		}
	}
	return n
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
	return db.repo.Put(p)
}

// trimName normalizes a name on the *creation* path, where an over-long name
// is silently truncated rather than rejected (bdrive init must not fail on a
// long folder name). Update is stricter — see maxNameLen.
func trimName(s string) string { return trimText(s, 128) }

// trimText strips line breaks and outer spaces, then truncates to max runes.
func trimText(s string, max int) string {
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
	if len(out) > max {
		out = out[:max]
	}
	return string(out)
}
