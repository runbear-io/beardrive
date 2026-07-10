package webapp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// The file backend: each repository is one JSON file, cached in memory and
// rewritten atomically (temp + rename) on every change — the exact on-disk
// format and discipline the registries used before the store abstraction, so a
// running hub upgrades with no migration.

// writeFileAtomic writes data to path via a temp file + rename. Files land at
// 0600 (os.CreateTemp's default); dirMode controls the parent directory.
func writeFileAtomic(path string, data []byte, dirMode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".bdrive-tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func readJSONFile(path string, into any) (found bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if err := json.Unmarshal(data, into); err != nil {
		return false, fmt.Errorf("parse %s: %w", path, err)
	}
	return true, nil
}

// fileMetaStore bundles the five file repositories rooted in one directory,
// keeping the historical filenames so an existing hub's data loads unchanged.
type fileMetaStore struct {
	accounts *fileAccountRepo
	projects *fileProjectRepo
	orgs     *fileOrgRepo
	shares   *fileShareRepo
	devices  *fileDeviceRepo
}

// OpenFileStore builds the file backend over dir, using the historical
// filenames (auth.json, projects.json, orgs.json, shares.json, devices.json).
func OpenFileStore(dir string) (MetaStore, error) {
	return &fileMetaStore{
		accounts: newFileAccountRepo(filepath.Join(dir, "auth.json")),
		projects: newFileProjectRepo(filepath.Join(dir, "projects.json")),
		orgs:     newFileOrgRepo(filepath.Join(dir, "orgs.json")),
		shares:   newFileShareRepo(filepath.Join(dir, "shares.json")),
		devices:  newFileDeviceRepo(filepath.Join(dir, "devices.json")),
	}, nil
}

func (s *fileMetaStore) Accounts() AccountRepo { return s.accounts }
func (s *fileMetaStore) Projects() ProjectRepo { return s.projects }
func (s *fileMetaStore) Orgs() OrgRepo         { return s.orgs }
func (s *fileMetaStore) Shares() ShareRepo     { return s.shares }
func (s *fileMetaStore) Devices() DeviceRepo   { return s.devices }
func (s *fileMetaStore) Close() error          { return nil }

// ---- accounts (auth.json: users + tokens + policy) ----

type authFileShape struct {
	Users  []*authUser `json:"users"`
	Tokens []authToken `json:"tokens"`
	Policy *authPolicy `json:"policy,omitempty"`
}

type fileAccountRepo struct {
	path string

	mu     sync.Mutex
	users  map[string]*authUser
	tokens map[string]authToken
	policy *authPolicy
}

func newFileAccountRepo(path string) *fileAccountRepo {
	return &fileAccountRepo{path: path, users: map[string]*authUser{}, tokens: map[string]authToken{}}
}

func (r *fileAccountRepo) Load() ([]*authUser, []authToken, *authPolicy, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var f authFileShape
	if _, err := readJSONFile(r.path, &f); err != nil {
		return nil, nil, nil, err
	}
	r.users = map[string]*authUser{}
	r.tokens = map[string]authToken{}
	for _, u := range f.Users {
		r.users[u.ID] = u
	}
	for _, t := range f.Tokens {
		r.tokens[t.Hash] = t
	}
	r.policy = f.Policy
	return f.Users, f.Tokens, f.Policy, nil
}

// write persists users, tokens, and policy. Callers hold mu.
func (r *fileAccountRepo) write() error {
	var f authFileShape
	for _, u := range r.users {
		f.Users = append(f.Users, u)
	}
	for _, t := range r.tokens {
		f.Tokens = append(f.Tokens, t)
	}
	f.Policy = r.policy
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(r.path, append(data, '\n'), 0o700) // holds password hashes
}

func (r *fileAccountRepo) PutAccount(u *authUser) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.users[u.ID] = u
	return r.write()
}

func (r *fileAccountRepo) DeleteAccount(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.users, id)
	return r.write()
}

func (r *fileAccountRepo) PutToken(t authToken) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tokens[t.Hash] = t
	return r.write()
}

func (r *fileAccountRepo) DeleteToken(hash string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tokens, hash)
	return r.write()
}

func (r *fileAccountRepo) PutPolicy(p authPolicy) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.policy = &p
	return r.write()
}

// ---- projects (projects.json) ----

type fileProjectRepo struct {
	path string
	mu   sync.Mutex
	byID map[string]Project
}

func newFileProjectRepo(path string) *fileProjectRepo {
	return &fileProjectRepo{path: path, byID: map[string]Project{}}
}

func (r *fileProjectRepo) Load() ([]Project, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var f struct {
		Projects []Project `json:"projects"`
	}
	if _, err := readJSONFile(r.path, &f); err != nil {
		return nil, err
	}
	r.byID = map[string]Project{}
	for _, p := range f.Projects {
		r.byID[p.ID] = p
	}
	return f.Projects, nil
}

func (r *fileProjectRepo) write() error {
	list := make([]Project, 0, len(r.byID))
	for _, p := range r.byID {
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	data, err := json.MarshalIndent(struct {
		Projects []Project `json:"projects"`
	}{list}, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(r.path, append(data, '\n'), 0o755)
}

func (r *fileProjectRepo) Put(p Project) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byID[p.ID] = p
	return r.write()
}

func (r *fileProjectRepo) Delete(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byID, id)
	return r.write()
}

// ---- orgs (orgs.json: orgs + invites) ----

type fileOrgRepo struct {
	path    string
	mu      sync.Mutex
	byID    map[string]Org
	invites map[string]OrgInvite
}

func newFileOrgRepo(path string) *fileOrgRepo {
	return &fileOrgRepo{path: path, byID: map[string]Org{}, invites: map[string]OrgInvite{}}
}

func (r *fileOrgRepo) Load() ([]Org, []OrgInvite, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var f struct {
		Orgs    []Org       `json:"orgs"`
		Invites []OrgInvite `json:"invites"`
	}
	if _, err := readJSONFile(r.path, &f); err != nil {
		return nil, nil, err
	}
	r.byID = map[string]Org{}
	r.invites = map[string]OrgInvite{}
	for _, o := range f.Orgs {
		r.byID[o.ID] = o
	}
	for _, i := range f.Invites {
		r.invites[i.Token] = i
	}
	return f.Orgs, f.Invites, nil
}

func (r *fileOrgRepo) write() error {
	var f struct {
		Orgs    []Org       `json:"orgs"`
		Invites []OrgInvite `json:"invites"`
	}
	for _, o := range r.byID {
		f.Orgs = append(f.Orgs, o)
	}
	sort.Slice(f.Orgs, func(i, j int) bool { return f.Orgs[i].ID < f.Orgs[j].ID })
	for _, i := range r.invites {
		if !i.expired() {
			f.Invites = append(f.Invites, i)
		}
	}
	sort.Slice(f.Invites, func(i, j int) bool { return f.Invites[i].Token < f.Invites[j].Token })
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(r.path, append(data, '\n'), 0o755)
}

func (r *fileOrgRepo) PutOrg(o Org) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byID[o.ID] = o
	return r.write()
}

func (r *fileOrgRepo) DeleteOrg(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byID, id)
	return r.write()
}

func (r *fileOrgRepo) PutInvite(i OrgInvite) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.invites[i.Token] = i
	return r.write()
}

func (r *fileOrgRepo) DeleteInvite(token string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.invites, token)
	return r.write()
}

// ---- shares (shares.json) ----

type fileShareRepo struct {
	path    string
	mu      sync.Mutex
	byToken map[string]Share
}

func newFileShareRepo(path string) *fileShareRepo {
	return &fileShareRepo{path: path, byToken: map[string]Share{}}
}

func (r *fileShareRepo) Load() ([]Share, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var f struct {
		Shares []Share `json:"shares"`
	}
	if _, err := readJSONFile(r.path, &f); err != nil {
		return nil, err
	}
	r.byToken = map[string]Share{}
	for _, s := range f.Shares {
		r.byToken[s.Token] = s
	}
	return f.Shares, nil
}

func (r *fileShareRepo) write() error {
	var f struct {
		Shares []Share `json:"shares"`
	}
	for _, s := range r.byToken {
		f.Shares = append(f.Shares, s)
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(r.path, append(data, '\n'), 0o755)
}

func (r *fileShareRepo) Put(s Share) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byToken[s.Token] = s
	return r.write()
}

func (r *fileShareRepo) Delete(token string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byToken, token)
	return r.write()
}

// ---- devices (devices.json) ----

type fileDeviceRepo struct {
	path string
	mu   sync.Mutex
	byID map[string]DeviceInfo
}

func newFileDeviceRepo(path string) *fileDeviceRepo {
	return &fileDeviceRepo{path: path, byID: map[string]DeviceInfo{}}
}

func (r *fileDeviceRepo) Load() ([]DeviceInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var f struct {
		Devices []DeviceInfo `json:"devices"`
	}
	if _, err := readJSONFile(r.path, &f); err != nil {
		return nil, err
	}
	r.byID = map[string]DeviceInfo{}
	for _, d := range f.Devices {
		r.byID[d.ID] = d
	}
	return f.Devices, nil
}

func (r *fileDeviceRepo) write() error {
	var f struct {
		Devices []DeviceInfo `json:"devices"`
	}
	for _, d := range r.byID {
		f.Devices = append(f.Devices, d)
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(r.path, append(data, '\n'), 0o755)
}

func (r *fileDeviceRepo) Put(d DeviceInfo) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byID[d.ID] = d
	return r.write()
}
