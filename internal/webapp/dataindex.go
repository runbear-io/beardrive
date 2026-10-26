package webapp

import (
	"bytes"
	"context"
	"crypto/md5"
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"modernc.org/sqlite"

	"github.com/runbear-io/beardrive/internal/journal"
)

/*
DataIndex is the hub's index of project data: one row per JSON file, and

	per Markdown file's frontmatter, in every project whose Data setting is on.

	Files stay the truth. A row is derived from two things the hub already has:
	the fold (which version of each path is current) and the blob (what that
	version says). Rows live in the hub's SQL database, not in process memory,
	so every hub process answers a query the same way and a process that has
	just started answers it without folding a journal or fetching a blob.

	Each row carries `win`, the replay-order key of the op that produced it
	(winKey). Every write is conditional on the stored key being older, so an
	applier holding an older fold — a slower process, a background pass that
	lost a race with the write that came after it — can never put back what a
	newer fold replaced. A deletion is a row too (deleted, keyed on the delete
	op) for the same reason: without it an older fold would put the file back.

	Queries run on their own small, read-only pool with a statement timeout, so
	a slow aggregate waits on its own connections and never on the ones sign-in
	and sync use.
*/
type DataIndex struct {
	w *sql.DB // the applier
	r *sql.DB // queries: read-only, few connections, bounded statements
	d dialect
}

const (
	// dataQueryConns bounds the query pool. Against the ~25 connections a
	// db-f1-micro allows, beside the metadata store's 10 and the applier's 2.
	dataQueryConns = 4
	// dataStatementTimeout bounds one query. A screen that waits longer than
	// this is broken anyway; the cap is what keeps it from taking the pool.
	dataStatementTimeout = 2 * time.Second
)

var registerSQLiteFuncs sync.Once

// OpenDataIndex opens (and migrates) the data index. driver and dsn are the
// metadata database's own ("sqlite" or "pgx"), or "sqlite" and a file path
// for a hub whose metadata is files.
func OpenDataIndex(driver, dsn string) (*DataIndex, error) {
	d := dialectSQLite
	readDSN := dsn
	switch driver {
	case "pgx", "postgres", "pgx/v5":
		driver, d = "pgx", dialectPostgres
	case "sqlite", "sqlite3":
		driver = "sqlite"
		registerSQLiteFuncs.Do(registerDataSQLiteFuncs)
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		// Waiting out the applier's write lock beats failing a read, and WAL
		// lets a read run beside a write at all.
		dsn += sep + "_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
		readDSN = dsn + "&_pragma=query_only(1)"
	default:
		return nil, fmt.Errorf("unsupported database driver %q for the data index", driver)
	}
	w, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, err
	}
	r, err := sql.Open(driver, readDSN)
	if err != nil {
		w.Close()
		return nil, err
	}
	w.SetMaxOpenConns(2)
	r.SetMaxOpenConns(dataQueryConns)
	if d == dialectPostgres {
		for _, db := range []*sql.DB{w, r} {
			db.SetConnMaxLifetime(30 * time.Minute)
			db.SetConnMaxIdleTime(5 * time.Minute)
		}
	}
	x := &DataIndex{w: w, r: r, d: d}
	if err := x.migrate(); err != nil {
		x.Close()
		return nil, err
	}
	return x, nil
}

// SetMaxConns bounds the index's two pools: the applier's and the queries'.
// The defaults (2 and 4) assume a database with room to spare; a deployment
// sharing a small one — a db-f1-micro allows about 25 connections for every
// pool in the process — sizes them to its own budget.
func (x *DataIndex) SetMaxConns(applier, queries int) {
	x.w.SetMaxOpenConns(applier)
	x.r.SetMaxOpenConns(queries)
}

func (x *DataIndex) Close() error {
	x.r.Close()
	return x.w.Close()
}

func (x *DataIndex) migrate() error {
	path, win, doc := `TEXT NOT NULL`, `BLOB NOT NULL`, `TEXT`
	if x.d == dialectPostgres {
		// C collation so the primary key orders paths byte-wise: prefix
		// ranges then use it, and ordering agrees with SQLite and with Go.
		path, win, doc = `TEXT COLLATE "C" NOT NULL`, `BYTEA NOT NULL`, `JSONB`
	}
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS data_items (
			project    TEXT NOT NULL,
			path       ` + path + `,
			win        ` + win + `,
			sha        TEXT NOT NULL DEFAULT '',
			changed_at TEXT NOT NULL DEFAULT '',
			changed_by TEXT NOT NULL DEFAULT '',
			doc        ` + doc + `,
			error      TEXT NOT NULL DEFAULT '',
			deleted    INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (project, path))`,
		`CREATE TABLE IF NOT EXISTS data_projects (
			project    TEXT PRIMARY KEY,
			complete   INTEGER NOT NULL DEFAULT 0,
			applied_at TEXT NOT NULL DEFAULT '')`,
	} {
		if _, err := x.w.Exec(stmt); err != nil {
			return fmt.Errorf("migrate data index: %w", err)
		}
	}
	return nil
}

// q rebinds ?-placeholders to $1,$2,… for Postgres.
func (x *DataIndex) q(query string) string {
	if x.d != dialectPostgres {
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

// winKey encodes the fields journal.Less reads so that comparing two keys
// byte-wise orders them exactly as Less orders the ops: signed integers with
// the sign bit flipped, big-endian; a time as seconds then nanoseconds;
// strings with NUL escaped and a terminator that sorts below every byte.
func winKey(op journal.Op) []byte {
	b := make([]byte, 0, 96+len(op.Device)+len(op.Path)+len(op.Blob))
	i64 := func(v int64) { b = binary.BigEndian.AppendUint64(b, uint64(v)^(1<<63)) }
	str := func(s string) {
		for i := 0; i < len(s); i++ {
			if s[i] == 0 {
				b = append(b, 0, 0xff)
			} else {
				b = append(b, s[i])
			}
		}
		b = append(b, 0, 0)
	}
	i64(op.Lamport)
	t := op.Time.UTC()
	i64(t.Unix())
	b = binary.BigEndian.AppendUint32(b, uint32(t.Nanosecond()))
	str(op.Device)
	i64(op.Seq)
	str(op.Kind)
	str(op.Path)
	str(op.Blob)
	i64(op.Size)
	b = binary.BigEndian.AppendUint32(b, op.Mode)
	return b
}

// dataRow is one row the applier writes.
type dataRow struct {
	path      string
	win       []byte
	sha       string
	changedAt time.Time
	changedBy string
	doc       []byte // JSON; nil for an error or a deletion
	err       string
	deleted   bool
}

// dataTime is how a time is stored: fixed-width UTC text, so that comparing
// the text compares the instants in every dialect.
func dataTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000000Z") }

// known is what the index holds for a project: path → (win, deleted).
func (x *DataIndex) known(ctx context.Context, project string) (map[string]dataKnown, error) {
	rows, err := x.w.QueryContext(ctx, x.q(`SELECT path, win, deleted FROM data_items WHERE project = ?`), project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]dataKnown{}
	for rows.Next() {
		var path string
		var win []byte
		var deleted int
		if err := rows.Scan(&path, &win, &deleted); err != nil {
			return nil, err
		}
		out[path] = dataKnown{win: string(win), deleted: deleted != 0}
	}
	return out, rows.Err()
}

type dataKnown struct {
	win     string
	deleted bool
}

// write applies rows, each only over an older one. Chunked so the initial
// build of a large project is not one transaction.
func (x *DataIndex) write(ctx context.Context, project string, rows []dataRow) error {
	docParam := "?"
	if x.d == dialectPostgres {
		docParam = "CAST(? AS JSONB)"
	}
	stmt := x.q(`INSERT INTO data_items (project, path, win, sha, changed_at, changed_by, doc, error, deleted)
		VALUES (?, ?, ?, ?, ?, ?, ` + docParam + `, ?, ?)
		ON CONFLICT (project, path) DO UPDATE SET
			win = excluded.win, sha = excluded.sha, changed_at = excluded.changed_at,
			changed_by = excluded.changed_by, doc = excluded.doc, error = excluded.error,
			deleted = excluded.deleted
		WHERE data_items.win < excluded.win`)
	const chunk = 500
	for len(rows) > 0 {
		n := min(chunk, len(rows))
		tx, err := x.w.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		for _, r := range rows[:n] {
			var doc any
			if r.doc != nil {
				doc = string(r.doc)
			}
			if _, err := tx.ExecContext(ctx, stmt, project, r.path, r.win, r.sha, dataTime(r.changedAt),
				r.changedBy, doc, r.err, boolInt(r.deleted)); err != nil {
				tx.Rollback()
				return fmt.Errorf("index %s: %w", r.path, err)
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		rows = rows[n:]
	}
	return nil
}

// forget drops a row that left the fold with no delete op to key a
// tombstone on — only while it still holds the key the caller saw.
func (x *DataIndex) forget(ctx context.Context, project, path string, win []byte) error {
	_, err := x.w.ExecContext(ctx, x.q(`DELETE FROM data_items WHERE project = ? AND path = ? AND win = ?`),
		project, path, win)
	return err
}

// dropProject removes a project's whole index, when its Data setting goes off
// or the project goes away.
func (x *DataIndex) dropProject(ctx context.Context, project string) error {
	for _, t := range []string{"data_items", "data_projects"} {
		if _, err := x.w.ExecContext(ctx, x.q(`DELETE FROM `+t+` WHERE project = ?`), project); err != nil {
			return err
		}
	}
	return nil
}

// setState records a project's index state. complete is only ever raised by a
// pass with nothing left to retry; starting a fresh build lowers it.
func (x *DataIndex) setState(ctx context.Context, project string, complete bool, at time.Time) error {
	_, err := x.w.ExecContext(ctx, x.q(`INSERT INTO data_projects (project, complete, applied_at) VALUES (?, ?, ?)
		ON CONFLICT (project) DO UPDATE SET complete = excluded.complete, applied_at = excluded.applied_at`),
		project, boolInt(complete), dataTime(at))
	return err
}

// readTx is a consistent, read-only view for one query: its ETag and its rows
// come from the same snapshot of the index.
func (x *DataIndex) readTx(ctx context.Context) (*sql.Tx, error) {
	opts := &sql.TxOptions{ReadOnly: true}
	if x.d == dialectPostgres {
		opts.Isolation = sql.LevelRepeatableRead
	}
	tx, err := x.r.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	if x.d == dialectPostgres {
		// UTC so a timestamp with no offset means the same instant here as in
		// week() on SQLite, where it is read as UTC.
		for _, set := range []string{
			fmt.Sprintf(`SET LOCAL statement_timeout = %d`, dataStatementTimeout.Milliseconds()),
			`SET LOCAL TimeZone = 'UTC'`,
		} {
			if _, err := tx.ExecContext(ctx, set); err != nil {
				tx.Rollback()
				return nil, err
			}
		}
	}
	return tx, nil
}

// state is a project's index state as a query reports it.
func (x *DataIndex) state(ctx context.Context, tx *sql.Tx, project string) (complete bool, asOf string, err error) {
	var c int
	err = tx.QueryRowContext(ctx, x.q(`SELECT complete, applied_at FROM data_projects WHERE project = ?`), project).
		Scan(&c, &asOf)
	if err == sql.ErrNoRows {
		return false, "", nil
	}
	return c != 0, asOf, err
}

// rowsDigest hashes the (path, win) of every row a query reads, in path order,
// and counts the ones that failed to index. It is the part of a query's ETag
// that moves when the data does — over the rows THIS caller can see, so a
// change behind a folder rule never moves it.
func (x *DataIndex) rowsDigest(ctx context.Context, tx *sql.Tx, where string, args []any) (string, int, error) {
	if x.d == dialectPostgres {
		var digest string
		var errs int
		err := tx.QueryRowContext(ctx, `SELECT
			md5(coalesce(string_agg(f.path || ':' || encode(f.win, 'hex'), ',' ORDER BY f.path), '')),
			count(*) FILTER (WHERE f.error <> '')
			FROM data_items AS f WHERE `+where, args...).Scan(&digest, &errs)
		return digest, errs, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT f.path, f.win, f.error <> '' FROM data_items AS f WHERE `+where+
		` ORDER BY f.path`, args...)
	if err != nil {
		return "", 0, err
	}
	defer rows.Close()
	var b bytes.Buffer
	errs := 0
	for rows.Next() {
		var path string
		var win []byte
		var failed bool
		if err := rows.Scan(&path, &win, &failed); err != nil {
			return "", 0, err
		}
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(path)
		b.WriteByte(':')
		b.WriteString(hex.EncodeToString(win))
		if failed {
			errs++
		}
	}
	sum := md5.Sum(b.Bytes())
	return hex.EncodeToString(sum[:]), errs, rows.Err()
}

// registerDataSQLiteFuncs gives SQLite the functions Postgres has built in and
// it does not, with the same answers: a path segment by position (split_part),
// Unicode lower(), and the ISO week of a timestamp in a time zone.
func registerDataSQLiteFuncs() {
	sqlite.MustRegisterDeterministicScalarFunction("bd_seg", 2, func(_ *sqlite.FunctionContext, a []driver.Value) (driver.Value, error) {
		s, _ := a[0].(string)
		n, _ := a[1].(int64)
		return pathSegment(s, int(n)), nil
	})
	sqlite.MustRegisterDeterministicScalarFunction("bd_lower", 1, func(_ *sqlite.FunctionContext, a []driver.Value) (driver.Value, error) {
		s, ok := a[0].(string)
		if !ok {
			return nil, nil
		}
		return strings.ToLower(s), nil
	})
	sqlite.MustRegisterDeterministicScalarFunction("bd_week", 2, func(_ *sqlite.FunctionContext, a []driver.Value) (driver.Value, error) {
		s, _ := a[0].(string)
		tz, _ := a[1].(string)
		w, ok := isoWeek(s, tz)
		if !ok {
			return nil, nil
		}
		return w, nil
	})
}

// pathSegment is split_part(s, '/', n): 1-based, negative from the end, ""
// past either end.
func pathSegment(s string, n int) string {
	segs := strings.Split(s, "/")
	switch {
	case n > 0 && n <= len(segs):
		return segs[n-1]
	case n < 0 && -n <= len(segs):
		return segs[len(segs)+n]
	}
	return ""
}

// isoWeek is week(): the ISO year-week ("2026-W43") of an ISO 8601 date or
// timestamp, in a time zone. A string that is not one has no week. A time with
// no offset is UTC, as it is in the Postgres session (readTx).
func isoWeek(s, tz string) (string, bool) {
	if !isoTimeRe.MatchString(s) {
		return "", false
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return "", false
	}
	layout := "2006-01-02T15:04:05.999999999Z07:00"
	switch {
	case len(s) == 10:
		layout = "2006-01-02"
	case !strings.HasSuffix(s, "Z") && !strings.ContainsAny(s[19:], "+-"):
		layout = "2006-01-02T15:04:05.999999999"
	}
	t, err := time.Parse(layout, s)
	if err != nil {
		return "", false
	}
	y, w := t.In(loc).ISOWeek()
	return fmt.Sprintf("%04d-W%02d", y, w), true
}
