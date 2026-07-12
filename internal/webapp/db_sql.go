package webapp

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	// Pure-Go drivers only, so static (CGO-free) builds keep working.
	_ "github.com/jackc/pgx/v5/stdlib" // "pgx" — Postgres / Supabase
	_ "modernc.org/sqlite"             // "sqlite" — embedded local DB
)

// The SQL backend: one *sql.DB shared by the repos, targeting SQLite (local)
// or Postgres/Supabase (production) through the same portable schema. Each
// record is a real row; multi-step writes (an org and its members) run in a
// transaction. Timestamps are stored as RFC3339 text and booleans as 0/1 so
// the same statements work on both engines.

type dialect int

const (
	dialectSQLite dialect = iota
	dialectPostgres
)

type sqlMetaStore struct {
	db *sql.DB
	d  dialect

	accounts *sqlAccountRepo
	projects *sqlProjectRepo
	orgs     *sqlOrgRepo
	shares   *sqlShareRepo
	devices  *sqlDeviceRepo
	reads    *sqlReadRepo
}

// OpenSQLStore opens (and migrates) a SQL metadata store. driver is "sqlite"
// or "pgx" (Postgres/Supabase); dsn is the connection string / file path.
func OpenSQLStore(driver, dsn string) (MetaStore, error) {
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", driver, err)
	}
	d := dialectSQLite
	switch driver {
	case "pgx", "postgres", "pgx/v5":
		d = dialectPostgres
	case "sqlite", "sqlite3":
		d = dialectSQLite
	default:
		db.Close()
		return nil, fmt.Errorf("unsupported database driver %q (use sqlite or pgx)", driver)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect %s: %w", driver, err)
	}
	s := &sqlMetaStore{db: db, d: d}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	s.accounts = &sqlAccountRepo{s}
	s.projects = &sqlProjectRepo{s}
	s.orgs = &sqlOrgRepo{s}
	s.shares = &sqlShareRepo{s}
	s.devices = &sqlDeviceRepo{s}
	s.reads = &sqlReadRepo{s}
	return s, nil
}

func (s *sqlMetaStore) Accounts() AccountRepo { return s.accounts }
func (s *sqlMetaStore) Projects() ProjectRepo { return s.projects }
func (s *sqlMetaStore) Orgs() OrgRepo         { return s.orgs }
func (s *sqlMetaStore) Shares() ShareRepo     { return s.shares }
func (s *sqlMetaStore) Devices() DeviceRepo   { return s.devices }
func (s *sqlMetaStore) Reads() ReadRepo       { return s.reads }
func (s *sqlMetaStore) Close() error          { return s.db.Close() }

// q rebinds ?-placeholders to $1,$2,… for Postgres; SQLite keeps ?.
func (s *sqlMetaStore) q(query string) string {
	if s.d != dialectPostgres {
		return query
	}
	var b strings.Builder
	n := 0
	for _, r := range query {
		if r == '?' {
			n++
			fmt.Fprintf(&b, "$%d", n)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (s *sqlMetaStore) exec(query string, args ...any) error {
	_, err := s.db.Exec(s.q(query), args...)
	return err
}

// tenc / tdec store times as RFC3339 text (empty string for the zero time),
// avoiding per-driver timestamp scanning differences.
func tenc(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func tdec(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s *sqlMetaStore) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS accounts (
			id TEXT PRIMARY KEY, email TEXT NOT NULL, name TEXT NOT NULL,
			pass TEXT NOT NULL, status TEXT NOT NULL DEFAULT '', created TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE IF NOT EXISTS tokens (
			hash TEXT PRIMARY KEY, user_id TEXT NOT NULL, device TEXT NOT NULL DEFAULT '',
			created TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE IF NOT EXISTS auth_policy (
			id INTEGER PRIMARY KEY, require_verification INTEGER NOT NULL DEFAULT 0,
			require_approval INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS projects (
			id TEXT PRIMARY KEY, name TEXT NOT NULL, org TEXT NOT NULL DEFAULT '',
			created TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE IF NOT EXISTS orgs (
			id TEXT PRIMARY KEY, name TEXT NOT NULL, created TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE IF NOT EXISTS org_members (
			org TEXT NOT NULL, email TEXT NOT NULL, role TEXT NOT NULL, PRIMARY KEY (org, email))`,
		`CREATE TABLE IF NOT EXISTS invites (
			token TEXT PRIMARY KEY, org TEXT NOT NULL, creator TEXT NOT NULL DEFAULT '',
			created TEXT NOT NULL DEFAULT '', expires TEXT NOT NULL DEFAULT '', uses INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS shares (
			token TEXT PRIMARY KEY, project TEXT NOT NULL, path TEXT NOT NULL,
			creator TEXT NOT NULL DEFAULT '', created TEXT NOT NULL DEFAULT '', expires TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE IF NOT EXISTS devices (
			id TEXT PRIMARY KEY, name TEXT NOT NULL DEFAULT '', os TEXT NOT NULL DEFAULT '',
			user_email TEXT NOT NULL DEFAULT '', ip TEXT NOT NULL DEFAULT '', last_seen TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE IF NOT EXISTS read_stats (
			project TEXT NOT NULL, path TEXT NOT NULL, day TEXT NOT NULL DEFAULT '',
			kind TEXT NOT NULL, actor TEXT NOT NULL,
			count INTEGER NOT NULL DEFAULT 0, last TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (project, path, day, kind, actor))`,
	}
	for _, st := range stmts {
		if _, err := s.db.Exec(st); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	return nil
}

// ---- accounts ----

type sqlAccountRepo struct{ s *sqlMetaStore }

func (r *sqlAccountRepo) Load() ([]*authUser, []authToken, *authPolicy, error) {
	var users []*authUser
	rows, err := r.s.db.Query(`SELECT id, email, name, pass, status, created FROM accounts`)
	if err != nil {
		return nil, nil, nil, err
	}
	for rows.Next() {
		var u authUser
		var created string
		if err := rows.Scan(&u.ID, &u.Email, &u.Name, &u.Pass, &u.Status, &created); err != nil {
			rows.Close()
			return nil, nil, nil, err
		}
		u.Created = tdec(created)
		users = append(users, &u)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, nil, err
	}

	var tokens []authToken
	rows, err = r.s.db.Query(`SELECT hash, user_id, device, created FROM tokens`)
	if err != nil {
		return nil, nil, nil, err
	}
	for rows.Next() {
		var t authToken
		var created string
		if err := rows.Scan(&t.Hash, &t.User, &t.Device, &created); err != nil {
			rows.Close()
			return nil, nil, nil, err
		}
		t.Created = tdec(created)
		tokens = append(tokens, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, nil, err
	}

	var policy *authPolicy
	var rv, ra int
	err = r.s.db.QueryRow(`SELECT require_verification, require_approval FROM auth_policy WHERE id = 1`).Scan(&rv, &ra)
	if err == nil {
		policy = &authPolicy{RequireVerification: rv != 0, RequireApproval: ra != 0}
	} else if err != sql.ErrNoRows {
		return nil, nil, nil, err
	}
	return users, tokens, policy, nil
}

func (r *sqlAccountRepo) PutAccount(u *authUser) error {
	return r.s.exec(`INSERT INTO accounts (id,email,name,pass,status,created) VALUES (?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET email=excluded.email, name=excluded.name, pass=excluded.pass,
		status=excluded.status, created=excluded.created`,
		u.ID, u.Email, u.Name, u.Pass, u.Status, tenc(u.Created))
}

func (r *sqlAccountRepo) DeleteAccount(id string) error {
	return r.s.exec(`DELETE FROM accounts WHERE id = ?`, id)
}

func (r *sqlAccountRepo) PutToken(t authToken) error {
	return r.s.exec(`INSERT INTO tokens (hash,user_id,device,created) VALUES (?,?,?,?)
		ON CONFLICT(hash) DO UPDATE SET user_id=excluded.user_id, device=excluded.device, created=excluded.created`,
		t.Hash, t.User, t.Device, tenc(t.Created))
}

func (r *sqlAccountRepo) DeleteToken(hash string) error {
	return r.s.exec(`DELETE FROM tokens WHERE hash = ?`, hash)
}

func (r *sqlAccountRepo) PutPolicy(p authPolicy) error {
	return r.s.exec(`INSERT INTO auth_policy (id,require_verification,require_approval) VALUES (1,?,?)
		ON CONFLICT(id) DO UPDATE SET require_verification=excluded.require_verification,
		require_approval=excluded.require_approval`,
		b2i(p.RequireVerification), b2i(p.RequireApproval))
}

// ---- projects ----

type sqlProjectRepo struct{ s *sqlMetaStore }

func (r *sqlProjectRepo) Load() ([]Project, error) {
	rows, err := r.s.db.Query(`SELECT id, name, org, created FROM projects`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		var p Project
		var created string
		if err := rows.Scan(&p.ID, &p.Name, &p.Org, &created); err != nil {
			return nil, err
		}
		p.Created = tdec(created)
		out = append(out, p)
	}
	return out, rows.Err()
}

func (r *sqlProjectRepo) Put(p Project) error {
	return r.s.exec(`INSERT INTO projects (id,name,org,created) VALUES (?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, org=excluded.org, created=excluded.created`,
		p.ID, p.Name, p.Org, tenc(p.Created))
}

func (r *sqlProjectRepo) Delete(id string) error {
	return r.s.exec(`DELETE FROM projects WHERE id = ?`, id)
}

// ---- orgs (+ members, + invites) ----

type sqlOrgRepo struct{ s *sqlMetaStore }

func (r *sqlOrgRepo) Load() ([]Org, []OrgInvite, error) {
	orgs := map[string]*Org{}
	var order []string
	rows, err := r.s.db.Query(`SELECT id, name, created FROM orgs`)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var o Org
		var created string
		if err := rows.Scan(&o.ID, &o.Name, &created); err != nil {
			rows.Close()
			return nil, nil, err
		}
		o.Created = tdec(created)
		o.Members = map[string]string{}
		orgs[o.ID] = &o
		order = append(order, o.ID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	rows, err = r.s.db.Query(`SELECT org, email, role FROM org_members`)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var org, email, role string
		if err := rows.Scan(&org, &email, &role); err != nil {
			rows.Close()
			return nil, nil, err
		}
		if o := orgs[org]; o != nil {
			o.Members[email] = role
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	outOrgs := make([]Org, 0, len(order))
	for _, id := range order {
		outOrgs = append(outOrgs, *orgs[id])
	}

	var invites []OrgInvite
	rows, err = r.s.db.Query(`SELECT token, org, creator, created, expires, uses FROM invites`)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var i OrgInvite
		var created, expires string
		if err := rows.Scan(&i.Token, &i.Org, &i.Creator, &created, &expires, &i.Uses); err != nil {
			rows.Close()
			return nil, nil, err
		}
		i.Created, i.Expires = tdec(created), tdec(expires)
		invites = append(invites, i)
	}
	rows.Close()
	return outOrgs, invites, rows.Err()
}

func (r *sqlOrgRepo) PutOrg(o Org) error {
	tx, err := r.s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(r.s.q(`INSERT INTO orgs (id,name,created) VALUES (?,?,?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, created=excluded.created`),
		o.ID, o.Name, tenc(o.Created)); err != nil {
		return err
	}
	if _, err := tx.Exec(r.s.q(`DELETE FROM org_members WHERE org = ?`), o.ID); err != nil {
		return err
	}
	for email, role := range o.Members {
		if _, err := tx.Exec(r.s.q(`INSERT INTO org_members (org,email,role) VALUES (?,?,?)`),
			o.ID, email, role); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (r *sqlOrgRepo) DeleteOrg(id string) error {
	tx, err := r.s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(r.s.q(`DELETE FROM org_members WHERE org = ?`), id); err != nil {
		return err
	}
	if _, err := tx.Exec(r.s.q(`DELETE FROM orgs WHERE id = ?`), id); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *sqlOrgRepo) PutInvite(i OrgInvite) error {
	return r.s.exec(`INSERT INTO invites (token,org,creator,created,expires,uses) VALUES (?,?,?,?,?,?)
		ON CONFLICT(token) DO UPDATE SET org=excluded.org, creator=excluded.creator,
		created=excluded.created, expires=excluded.expires, uses=excluded.uses`,
		i.Token, i.Org, i.Creator, tenc(i.Created), tenc(i.Expires), i.Uses)
}

func (r *sqlOrgRepo) DeleteInvite(token string) error {
	return r.s.exec(`DELETE FROM invites WHERE token = ?`, token)
}

// ---- shares ----

type sqlShareRepo struct{ s *sqlMetaStore }

func (r *sqlShareRepo) Load() ([]Share, error) {
	rows, err := r.s.db.Query(`SELECT token, project, path, creator, created, expires FROM shares`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Share
	for rows.Next() {
		var s Share
		var created, expires string
		if err := rows.Scan(&s.Token, &s.Project, &s.Path, &s.Creator, &created, &expires); err != nil {
			return nil, err
		}
		s.Created, s.Expires = tdec(created), tdec(expires)
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r *sqlShareRepo) Put(s Share) error {
	return r.s.exec(`INSERT INTO shares (token,project,path,creator,created,expires) VALUES (?,?,?,?,?,?)
		ON CONFLICT(token) DO UPDATE SET project=excluded.project, path=excluded.path,
		creator=excluded.creator, created=excluded.created, expires=excluded.expires`,
		s.Token, s.Project, s.Path, s.Creator, tenc(s.Created), tenc(s.Expires))
}

func (r *sqlShareRepo) Delete(token string) error {
	return r.s.exec(`DELETE FROM shares WHERE token = ?`, token)
}

// ---- devices ----

type sqlDeviceRepo struct{ s *sqlMetaStore }

func (r *sqlDeviceRepo) Load() ([]DeviceInfo, error) {
	rows, err := r.s.db.Query(`SELECT id, name, os, user_email, ip, last_seen FROM devices`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeviceInfo
	for rows.Next() {
		var d DeviceInfo
		var lastSeen string
		if err := rows.Scan(&d.ID, &d.Name, &d.OS, &d.User, &d.IP, &lastSeen); err != nil {
			return nil, err
		}
		d.LastSeen = tdec(lastSeen)
		out = append(out, d)
	}
	return out, rows.Err()
}

func (r *sqlDeviceRepo) Put(d DeviceInfo) error {
	return r.s.exec(`INSERT INTO devices (id,name,os,user_email,ip,last_seen) VALUES (?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, os=excluded.os, user_email=excluded.user_email,
		ip=excluded.ip, last_seen=excluded.last_seen`,
		d.ID, d.Name, d.OS, d.User, d.IP, tenc(d.LastSeen))
}

// ---- reads ----

type sqlReadRepo struct{ s *sqlMetaStore }

func (r *sqlReadRepo) Load() ([]ReadStat, error) {
	rows, err := r.s.db.Query(`SELECT project, path, day, kind, actor, count, last FROM read_stats`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReadStat
	for rows.Next() {
		var st ReadStat
		var last string
		if err := rows.Scan(&st.Project, &st.Path, &st.Day, &st.Kind, &st.Actor, &st.Count, &last); err != nil {
			return nil, err
		}
		st.Last = tdec(last)
		out = append(out, st)
	}
	return out, rows.Err()
}

func (r *sqlReadRepo) PutBatch(stats []ReadStat) error {
	tx, err := r.s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, st := range stats {
		if _, err := tx.Exec(r.s.q(`INSERT INTO read_stats (project,path,day,kind,actor,count,last)
			VALUES (?,?,?,?,?,?,?)
			ON CONFLICT(project,path,day,kind,actor) DO UPDATE SET count=excluded.count, last=excluded.last`),
			st.Project, st.Path, st.Day, st.Kind, st.Actor, st.Count, tenc(st.Last)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (r *sqlReadRepo) DeleteBatch(keys []ReadStatKey) error {
	tx, err := r.s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, k := range keys {
		if _, err := tx.Exec(r.s.q(`DELETE FROM read_stats
			WHERE project = ? AND path = ? AND day = ? AND kind = ? AND actor = ?`),
			k.Project, k.Path, k.Day, k.Kind, k.Actor); err != nil {
			return err
		}
	}
	return tx.Commit()
}
