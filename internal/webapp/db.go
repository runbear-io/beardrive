package webapp

// MetaStore is the hub's metadata persistence, split into one typed repository
// per entity. It holds ONLY the control plane — accounts, tokens, projects,
// orgs, invites, shares, devices. File content and the append-only journals
// live in the object store and never touch this; ephemeral state (one-time
// login and device codes, rate-limit buckets) stays in memory.
//
// A deployment chooses the backend: `file` (JSON on disk, the zero-dependency
// default) or `sql` (SQLite locally, Postgres/Supabase in production). The
// service structs (BuiltinAuth, OrgDB, …) keep their in-memory maps, mutexes,
// and business logic and persist each change through these repos — so reads
// stay in memory and writes are a single record apiece, which every backend
// implements as one real row.
type MetaStore interface {
	Accounts() AccountRepo
	Projects() ProjectRepo
	Orgs() OrgRepo
	Shares() ShareRepo
	Devices() DeviceRepo
	Close() error
}

// AccountRepo persists accounts, device tokens, and the (singleton) signup
// policy. Load returns everything at open; every other method is one record.
type AccountRepo interface {
	Load() (users []*authUser, tokens []authToken, policy *authPolicy, err error)
	PutAccount(u *authUser) error
	DeleteAccount(id string) error
	PutToken(t authToken) error
	DeleteToken(hash string) error
	PutPolicy(p authPolicy) error
}

type ProjectRepo interface {
	Load() ([]Project, error)
	Put(p Project) error
	Delete(id string) error
}

type OrgRepo interface {
	Load() (orgs []Org, invites []OrgInvite, err error)
	PutOrg(o Org) error
	DeleteOrg(id string) error
	PutInvite(i OrgInvite) error
	DeleteInvite(token string) error
}

type ShareRepo interface {
	Load() ([]Share, error)
	Put(s Share) error
	Delete(token string) error
}

type DeviceRepo interface {
	Load() ([]DeviceInfo, error)
	Put(d DeviceInfo) error
}
