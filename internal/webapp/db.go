package webapp

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

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
	Reads() ReadRepo
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

// ReadRepo persists read-telemetry buckets (see ReadStat). Unlike the other
// repos it is batch-oriented: reads are telemetry, and the ledger flushes many
// dirty buckets at once — one file rewrite / one SQL transaction per flush,
// not one write per bucket.
type ReadRepo interface {
	Load() ([]ReadStat, error)
	PutBatch(stats []ReadStat) error // upsert by (project, path, day, kind, actor)
	DeleteBatch(keys []ReadStatKey) error
}

// ---- what a metadata store will hold ------------------------------------

// storable refuses text that no metadata backend can hold faithfully, so all
// three agree on which requests succeed.
//
// The three disagreed on eighteen inputs. A NUL cannot go in a Postgres text
// column at all (SQLSTATE 22021), while sqlite keeps it and the file backend
// keeps it. Invalid UTF-8 is worse than a disagreement on the file backend —
// the default: encoding/json substitutes U+FFFD per bad byte and reports
// SUCCESS, so the running hub and its database hold different records, nothing
// is logged, and two inputs that differ in memory fold onto one key on disk.
//
// Refusing is the decision rather than storing verbatim, for two reasons. It
// is what the doors already enforce (printableOnly, hasControlChars,
// journal.SafePath), so there is one rule instead of three. And it means a hub
// cannot change what it accepts by changing its database — the property that
// let row 14 look clean for seven rounds while the backends diverged.
//
// It replaces the assertion in the retired TestSec_DB_NULBytesDoNotTruncateRecords
// ("a NUL must round-trip verbatim"), which Postgres cannot implement without
// moving this whole layer to bytea. See TestSec_DB_EveryBackendAgreesWhichTextIsStorable.
func storable(vals ...string) error {
	for _, v := range vals {
		if strings.IndexByte(v, 0) >= 0 {
			return fmt.Errorf("metadata text may not contain a NUL byte: %q", v)
		}
		if !utf8.ValidString(v) {
			return fmt.Errorf("metadata text must be valid UTF-8: %q", v)
		}
	}
	return nil
}

// storableMap checks a map's keys and values — the grant and membership maps,
// where the KEY is the account an authorization decision keys on.
func storableMap(m map[string]string) error {
	for k, v := range m {
		if err := storable(k, v); err != nil {
			return err
		}
	}
	return nil
}

func checkAccount(u *authUser) error {
	return storable(u.ID, u.Email, u.Name, u.Pass, u.Status)
}

func checkToken(t authToken) error { return storable(t.Hash, t.User, t.Device) }

func checkProject(p Project) error {
	if err := storable(p.ID, p.Name, p.Org, p.Description, p.Icon,
		p.Creator, p.Template, p.Default); err != nil {
		return err
	}
	return storableMap(p.Perms)
}

func checkOrg(o Org) error {
	if err := storable(o.ID, o.Name); err != nil {
		return err
	}
	return storableMap(o.Members)
}

func checkInvite(i OrgInvite) error { return storable(i.Token, i.Org, i.Creator) }

func checkShare(s Share) error { return storable(s.Token, s.Project, s.Path, s.Creator) }

func checkDevice(d DeviceInfo) error {
	return storable(d.ID, d.Name, d.OS, d.User, d.IP)
}

func checkReadStat(s ReadStat) error {
	return storable(s.Project, s.Path, s.Day, s.Kind, s.Actor)
}
