package webapp

import (
	"database/sql"
	"fmt"
	"strconv"
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
	ver      int // schema version the store recorded before this process migrated
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
		// device_rows replaces the original `devices` table, whose primary key
		// was the id alone: two accounts naming one device id collapsed into a
		// single row, so a restart handed the device to whoever wrote last and
		// the hub then refused its real owner. The key is (user_email, id),
		// matching DeviceRegistry. The old table is left in place — it is a
		// display cache that every device rebuilds on its next sync cycle, and
		// the rows worth keeping are copied over once below.
		`CREATE TABLE IF NOT EXISTS devices (
			id TEXT PRIMARY KEY, name TEXT NOT NULL DEFAULT '', os TEXT NOT NULL DEFAULT '',
			user_email TEXT NOT NULL DEFAULT '', ip TEXT NOT NULL DEFAULT '', last_seen TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE IF NOT EXISTS device_rows (
			user_email TEXT NOT NULL DEFAULT '', id TEXT NOT NULL,
			name TEXT NOT NULL DEFAULT '', os TEXT NOT NULL DEFAULT '',
			ip TEXT NOT NULL DEFAULT '', first_seen TEXT NOT NULL DEFAULT '',
			last_seen TEXT NOT NULL DEFAULT '', PRIMARY KEY (user_email, id))`,
		// The WHERE is not redundant: SQLite cannot parse ON CONFLICT directly
		// after an INSERT…SELECT (it reads as the SELECT's own ON clause), and
		// Postgres accepts it either way.
		`INSERT INTO device_rows (user_email,id,name,os,ip,first_seen,last_seen)
			SELECT user_email,id,name,os,ip,last_seen,last_seen FROM devices
			WHERE 1=1 ON CONFLICT (user_email,id) DO NOTHING`,
		`CREATE TABLE IF NOT EXISTS read_stats (
			project TEXT NOT NULL, path TEXT NOT NULL, day TEXT NOT NULL DEFAULT '',
			kind TEXT NOT NULL, actor TEXT NOT NULL,
			count INTEGER NOT NULL DEFAULT 0, last TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (project, path, day, kind, actor))`,
		`CREATE TABLE IF NOT EXISTS project_perms (
			project TEXT NOT NULL, email TEXT NOT NULL, level TEXT NOT NULL,
			PRIMARY KEY (project, email))`,
		`CREATE TABLE IF NOT EXISTS schema_meta (
			key TEXT PRIMARY KEY, value TEXT NOT NULL DEFAULT '')`,
	}
	for _, st := range stmts {
		if _, err := s.db.Exec(st); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	// The schema version this store last recorded, read before any ALTER: it is
	// what tells a first-ever migration apart from a rollback. See addColumns.
	var err error
	if s.ver, err = s.readSchemaVersion(); err != nil {
		return err
	}
	// Columns added after the tables shipped. CREATE TABLE IF NOT EXISTS does
	// nothing for an existing table, so these need a real (idempotent) ALTER.
	//
	// `default_level` is GUARDED. It defaults to '' when added, and '' is READ
	// AS `write` (Project.level — a deliberate no-migration choice, safe
	// forward and fail-OPEN backward). So re-adding it to a table that already
	// holds projects re-opens every `none` and `read` project to its whole
	// organization, silently, and a rollback, an older dump or a half-applied
	// migration is exactly how the column goes missing again.
	if err := s.addColumns("projects", map[string]string{
		"description":   `TEXT NOT NULL DEFAULT ''`,
		"icon":          `TEXT NOT NULL DEFAULT ''`,
		"creator":       `TEXT NOT NULL DEFAULT ''`,
		"default_level": `TEXT NOT NULL DEFAULT ''`,
		"template":      `TEXT NOT NULL DEFAULT ''`,
	}, map[string]string{
		"default_level": "it silently re-opens every restricted project to its whole organization",
	}); err != nil {
		return err
	}
	// A member row's join time. Rows written before it carry '' — the zero
	// time — which is what makes them the longest-standing members there are,
	// and that is the right answer for rows that predate the column.
	if err := s.addColumns("org_members", map[string]string{
		"joined": `TEXT NOT NULL DEFAULT ''`,
	}, map[string]string{}); err != nil {
		return err
	}
	return s.exec(`INSERT INTO schema_meta (key,value) VALUES (?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, "version", strconv.Itoa(schemaVersion))
}

// schemaVersion is what THIS binary's migration produces. Bump it when adding
// a column whose DEFAULT cannot be told apart from a real value — see the
// guarded set in migrate.
const schemaVersion = 1

// readSchemaVersion reads the version the store last recorded. 0 means a store
// written before versioning existed, which is the ordinary upgrade path and the
// one case where adding a guarded column is correct.
func (s *sqlMetaStore) readSchemaVersion() (int, error) {
	var v string
	err := s.db.QueryRow(s.q(`SELECT value FROM schema_meta WHERE key = ?`), "version").Scan(&v)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("migrate: read schema version: %w", err)
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("migrate: schema version %q is not a number", v)
	}
	return n, nil
}

// addColumns adds any of cols that the table doesn't already have. The live
// column set comes from an empty result set's metadata, which both drivers
// (modernc/sqlite and pgx) report the same way — no engine-specific catalog
// query, and safe to run on every start.
func (s *sqlMetaStore) addColumns(table string, cols, guarded map[string]string) error {
	rows, err := s.db.Query(`SELECT * FROM ` + table + ` LIMIT 0`)
	if err != nil {
		return fmt.Errorf("migrate %s: %w", table, err)
	}
	names, err := rows.Columns()
	rows.Close()
	if err != nil {
		return fmt.Errorf("migrate %s: %w", table, err)
	}
	have := make(map[string]bool, len(names))
	for _, n := range names {
		have[strings.ToLower(n)] = true
	}
	for col, spec := range cols {
		if have[col] {
			continue
		}
		// A guarded column's DEFAULT is indistinguishable from a real value, so
		// adding it rewrites the meaning of every row already in the table.
		// That is correct exactly once — the first migration, before this store
		// ever recorded a version — and on an empty table, where there is no
		// meaning to rewrite. Anywhere else it is a rollback or an older dump,
		// and it fails closed and loudly instead of widening access in silence.
		if why := guarded[col]; why != "" && s.ver > 0 {
			var rows int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&rows); err != nil {
				return fmt.Errorf("migrate %s: %w", table, err)
			}
			if rows > 0 {
				return fmt.Errorf("this database records schema version %d but %s.%s is missing "+
					"while the table holds %d row(s): that is a rollback, an older dump, or a "+
					"half-applied migration, and re-adding the column would hand every row its "+
					"DEFAULT — %s. Restore a dump taken at schema version %d or later, or add "+
					"the column back by hand with the values it should hold",
					s.ver, table, col, rows, why, s.ver)
			}
		}
		if _, err := s.db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + col + ` ` + spec); err != nil {
			return fmt.Errorf("migrate %s.%s: %w", table, col, err)
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
	if err := checkAccount(u); err != nil {
		return err
	}
	// The update arm is scoped to the SAME account: an id belongs to one
	// address for the life of the hub, so a row arriving under a live id with
	// a different email is a collision, not an update, and applying it hands
	// the victim's device tokens and memberships to the newcomer while their
	// password hash disappears. WHERE makes it a no-op; the rowcount check
	// turns that into an error the caller sees.
	res, err := r.s.db.Exec(r.s.q(`INSERT INTO accounts (id,email,name,pass,status,created) VALUES (?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET email=excluded.email, name=excluded.name, pass=excluded.pass,
		status=excluded.status, created=excluded.created
		WHERE lower(accounts.email) = lower(excluded.email)`),
		u.ID, u.Email, u.Name, u.Pass, u.Status, tenc(u.Created))
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("account id %s already belongs to another account", u.ID)
	}
	return nil
}

func (r *sqlAccountRepo) DeleteAccount(id string) error {
	return r.s.exec(`DELETE FROM accounts WHERE id = ?`, id)
}

func (r *sqlAccountRepo) PutToken(t authToken) error {
	if err := checkToken(t); err != nil {
		return err
	}
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
	rows, err := r.s.db.Query(
		`SELECT id, name, org, created, description, icon, creator, default_level, template FROM projects`)
	if err != nil {
		return nil, err
	}
	byID := map[string]*Project{}
	var order []string
	for rows.Next() {
		var p Project
		var created string
		if err := rows.Scan(&p.ID, &p.Name, &p.Org, &created,
			&p.Description, &p.Icon, &p.Creator, &p.Default, &p.Template); err != nil {
			rows.Close()
			return nil, err
		}
		p.Created = tdec(created)
		byID[p.ID] = &p
		order = append(order, p.ID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = r.s.db.Query(`SELECT project, email, level FROM project_perms`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var project, email, level string
		if err := rows.Scan(&project, &email, &level); err != nil {
			rows.Close()
			return nil, err
		}
		if p := byID[project]; p != nil {
			if p.Perms == nil {
				p.Perms = map[string]string{}
			}
			p.Perms[email] = level
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]Project, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out, nil
}

// Put writes the project and replaces its grants in one transaction — same
// shape as PutOrg over orgs/org_members.
func (r *sqlProjectRepo) Put(p Project) error {
	if err := checkProject(p); err != nil {
		return err
	}
	tx, err := r.s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(r.s.q(
		`INSERT INTO projects (id,name,org,created,description,icon,creator,default_level,template)
		VALUES (?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, org=excluded.org, created=excluded.created,
		description=excluded.description, icon=excluded.icon,
		creator=excluded.creator, default_level=excluded.default_level, template=excluded.template`),
		p.ID, p.Name, p.Org, tenc(p.Created), p.Description, p.Icon, p.Creator, p.Default, p.Template); err != nil {
		return err
	}
	if _, err := tx.Exec(r.s.q(`DELETE FROM project_perms WHERE project = ?`), p.ID); err != nil {
		return err
	}
	for email, level := range p.Perms {
		if _, err := tx.Exec(r.s.q(`INSERT INTO project_perms (project,email,level) VALUES (?,?,?)`),
			p.ID, email, level); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PutMeta writes the project's own columns and does not touch project_perms —
// see rowScopedProjectRepo for why a metadata write must not carry a grant set.
func (r *sqlProjectRepo) PutMeta(p Project) error {
	if err := checkProject(p); err != nil {
		return err
	}
	return r.s.exec(
		`INSERT INTO projects (id,name,org,created,description,icon,creator,default_level,template)
		VALUES (?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, org=excluded.org, created=excluded.created,
		description=excluded.description, icon=excluded.icon,
		creator=excluded.creator, default_level=excluded.default_level, template=excluded.template`,
		p.ID, p.Name, p.Org, tenc(p.Created), p.Description, p.Icon, p.Creator, p.Default, p.Template)
}

// PutPerm writes one grant row. An empty level removes it.
func (r *sqlProjectRepo) PutPerm(project, email, level string) error {
	if err := storable(project, email, level); err != nil {
		return err
	}
	if level == "" {
		return r.s.exec(`DELETE FROM project_perms WHERE project = ? AND email = ?`, project, email)
	}
	return r.s.exec(`INSERT INTO project_perms (project,email,level) VALUES (?,?,?)
		ON CONFLICT(project,email) DO UPDATE SET level=excluded.level`, project, email, level)
}

func (r *sqlProjectRepo) Delete(id string) error {
	tx, err := r.s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(r.s.q(`DELETE FROM project_perms WHERE project = ?`), id); err != nil {
		return err
	}
	if _, err := tx.Exec(r.s.q(`DELETE FROM projects WHERE id = ?`), id); err != nil {
		return err
	}
	return tx.Commit()
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
		o.Joined = map[string]time.Time{}
		orgs[o.ID] = &o
		order = append(order, o.ID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	rows, err = r.s.db.Query(`SELECT org, email, role, joined FROM org_members`)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var org, email, role, joined string
		if err := rows.Scan(&org, &email, &role, &joined); err != nil {
			rows.Close()
			return nil, nil, err
		}
		if o := orgs[org]; o != nil {
			o.Members[email] = role
			o.Joined[email] = tdec(joined)
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
	if err := checkOrg(o); err != nil {
		return err
	}
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
		if _, err := tx.Exec(r.s.q(`INSERT INTO org_members (org,email,role,joined) VALUES (?,?,?,?)`),
			o.ID, email, role, tenc(o.Joined[email])); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PutOrgMeta writes the org's own columns and does not touch org_members —
// see rowScopedOrgRepo for why a rename must not carry a member set.
func (r *sqlOrgRepo) PutOrgMeta(o Org) error {
	if err := checkOrg(o); err != nil {
		return err
	}
	return r.s.exec(`INSERT INTO orgs (id,name,created) VALUES (?,?,?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, created=excluded.created`,
		o.ID, o.Name, tenc(o.Created))
}

// PutMember writes one membership row. An empty role removes it.
func (r *sqlOrgRepo) PutMember(org, email, role string, joined time.Time) error {
	if err := storable(org, email, role); err != nil {
		return err
	}
	if role == "" {
		return r.s.exec(`DELETE FROM org_members WHERE org = ? AND email = ?`, org, email)
	}
	return r.s.exec(`INSERT INTO org_members (org,email,role,joined) VALUES (?,?,?,?)
		ON CONFLICT(org,email) DO UPDATE SET role=excluded.role, joined=excluded.joined`,
		org, email, role, tenc(joined))
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
	if err := checkInvite(i); err != nil {
		return err
	}
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
	if err := checkShare(s); err != nil {
		return err
	}
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
	rows, err := r.s.db.Query(`SELECT id, name, os, user_email, ip, first_seen, last_seen FROM device_rows`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeviceInfo
	for rows.Next() {
		var d DeviceInfo
		var firstSeen, lastSeen string
		if err := rows.Scan(&d.ID, &d.Name, &d.OS, &d.User, &d.IP, &firstSeen, &lastSeen); err != nil {
			return nil, err
		}
		d.FirstSeen, d.LastSeen = tdec(firstSeen), tdec(lastSeen)
		out = append(out, d)
	}
	return out, rows.Err()
}

func (r *sqlDeviceRepo) Put(d DeviceInfo) error {
	if err := checkDevice(d); err != nil {
		return err
	}
	return r.s.exec(`INSERT INTO device_rows (user_email,id,name,os,ip,first_seen,last_seen) VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(user_email,id) DO UPDATE SET name=excluded.name, os=excluded.os,
		ip=excluded.ip, last_seen=excluded.last_seen`,
		d.User, d.ID, d.Name, d.OS, d.IP, tenc(d.FirstSeen), tenc(d.LastSeen))
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
	for _, s := range stats {
		if err := checkReadStat(s); err != nil {
			return err
		}
	}
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
