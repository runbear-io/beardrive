package webapp

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A hub with database: sqlite and Data on keeps its metadata and its data
// index in one file, and a deployment may open more pools on it. A write on one
// waits out another's lock instead of failing SQLITE_BUSY at once, and a
// transaction that reads before it writes is not refused because another
// writer committed in between.
func TestSQLitePoolsSharingAFileWaitForEachOther(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	meta, err := OpenSQLStore("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer meta.Close()
	x, err := OpenDataIndex("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer x.Close()
	ctx := context.Background()
	holdIndexLock := func(d time.Duration) {
		t.Helper()
		tx, err := x.w.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO data_projects (project, complete, applied_at) VALUES ('p', 0, '')
			ON CONFLICT (project) DO UPDATE SET complete = 0`); err != nil {
			t.Fatal(err)
		}
		go func() { time.Sleep(d); tx.Commit() }()
	}

	holdIndexLock(300 * time.Millisecond)
	start := time.Now()
	if err := meta.Apps().PutApp(App{ID: "a1", Org: "o", Name: "x", Created: time.Now()}); err != nil {
		t.Fatalf("a metadata write beside an index write: %v", err)
	}
	if time.Since(start) < 200*time.Millisecond {
		t.Fatal("the metadata write did not meet the index's lock, so this proves nothing")
	}

	// Take reads, then deletes. Deferred, its read would land before the
	// index commits and its delete after, and SQLite refuses that delete.
	if err := meta.Pending().Put(PendingGrant{Kind: "cli", Key: "k", Payload: []byte("x"), Expires: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	holdIndexLock(300 * time.Millisecond)
	if _, ok, err := meta.Pending().Take("cli", "k"); err != nil || !ok {
		t.Fatalf("a read-then-write transaction beside an index write: ok=%v %v", ok, err)
	}
}

// SQLiteDSN adds what is missing and leaves what an operator set.
func TestSQLiteDSNKeepsTheOperatorsSettings(t *testing.T) {
	got := SQLiteDSN("/srv/hub.db?_pragma=busy_timeout(20000)")
	if strings.Count(got, "busy_timeout") != 1 || !strings.Contains(got, "busy_timeout(20000)") ||
		!strings.Contains(got, "journal_mode(WAL)") || !strings.Contains(got, "_txlock=immediate") {
		t.Fatalf("SQLiteDSN = %s", got)
	}
	db, err := sql.Open("sqlite", SQLiteDSN(filepath.Join(t.TempDir(), "x.db")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var mode string
	var timeout int
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q %v", mode, err)
	}
	if err := db.QueryRow(`PRAGMA busy_timeout`).Scan(&timeout); err != nil || timeout != 5000 {
		t.Fatalf("busy_timeout = %d %v", timeout, err)
	}
}
