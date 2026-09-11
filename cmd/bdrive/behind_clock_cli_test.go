package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runbear-io/beardrive/internal/journal"
)

// `*` marks the version the file HOLDS, so exactly one row can carry it.
// Restoring puts old bytes back under a NEW op, so a restored file legitimately
// has several ops sharing the current blob — and every one of them used to be
// starred. Two `*` rows read as two competing heads, which is what sent
// BEA-196's diagnosis to "duplicate head records per path".
func TestListMarksExactlyOneCurrentRow(t *testing.T) {
	log := ops(
		[3]string{journal.KindPut, "f.md", v1},
		[3]string{journal.KindPut, "f.md", v2},
		[3]string{journal.KindPut, "f.md", v1}, // a restore of v1: same blob, new op
	)
	var out bytes.Buffer
	printVersions(&out, versionsOf(log, "f.md"), currentBlob(log, "f.md"))
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("listing = %d lines, want 3:\n%s", len(lines), out.String())
	}
	starred := 0
	for _, l := range lines {
		if strings.HasPrefix(l, "* ") {
			starred++
		}
	}
	if starred != 1 {
		t.Fatalf("%d rows marked current, want exactly 1:\n%s", starred, out.String())
	}
	// And it must be the NEWEST op holding those bytes, not an older one.
	if !strings.HasPrefix(lines[0], "* "+v1[:8]) {
		t.Fatalf("the marked row is not the newest current one:\n%s", out.String())
	}
}

// The success line is what an agent trusts, so it may never name a version the
// file does not hold. restore writes the right bytes and then runs a full
// cycle; if anything in that cycle resolves the path elsewhere, this has to
// exit non-zero rather than print success over it.
func TestRestoreReportsWhatIsActuallyOnDisk(t *testing.T) {
	folder := staleProject(t, map[string]string{"doc.md": "version one\n"}, nil)
	abs := filepath.Join(folder, "doc.md")
	if err := os.WriteFile(abs, []byte("version two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sync := syncCmd()
	sync.SetOut(&bytes.Buffer{})
	sync.SetErr(&bytes.Buffer{})
	sync.SetArgs([]string{folder})
	if err := sync.Execute(); err != nil {
		t.Fatal(err)
	}

	run := func(args ...string) (string, error) {
		c := restoreCmd()
		var out bytes.Buffer
		c.SetOut(&out)
		c.SetErr(&out)
		c.SetArgs(args)
		err := c.Execute() // before reading the buffer: args are evaluated left to right
		return out.String(), err
	}
	listing, err := run(abs, "--list")
	if err != nil {
		t.Fatalf("--list: %v\n%s", err, listing)
	}
	lines := strings.Split(strings.TrimSpace(listing), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected two versions:\n%s", listing)
	}
	// The older row is the one to restore; its short sha is the first field.
	older := strings.Fields(strings.TrimPrefix(lines[1], "  "))[0]

	out, err := run(abs, older)
	if err != nil {
		t.Fatalf("restore: %v\n%s", err, out)
	}
	body, rerr := os.ReadFile(abs)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(body) != "version one\n" {
		t.Fatalf("restore exited 0 but the file holds %q", body)
	}
	if !strings.Contains(out, "restored") {
		t.Fatalf("no success line: %q", out)
	}
	// Exactly one row marked current, on real data this time.
	listing, err = run(abs, "--list")
	if err != nil {
		t.Fatal(err)
	}
	starred := 0
	for _, l := range strings.Split(strings.TrimSpace(listing), "\n") {
		if strings.HasPrefix(l, "* ") {
			starred++
		}
	}
	if starred != 1 {
		t.Fatalf("%d rows marked current after a restore, want 1:\n%s", starred, listing)
	}
}

// `status` is the command someone runs when sync looks wrong, and throughout
// BEA-196 it printed a healthy project: `pending` is len(myOps)-PushedOps, so
// a successful push zeroes it by construction, and Drift compares disk against
// the cache that materialize just made agree. Both were correct; neither could
// say "the ops this device writes are losing replay". A clock below the
// journals in its own volume dir is what that state IS, and status can read it
// without running a cycle.
func TestStatusNamesABehindClock(t *testing.T) {
	folder := staleProject(t, map[string]string{"doc.md": "one\n"}, nil)

	out, err := seccliRun(t, statusCmd(), []string{folder})
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	if strings.Contains(out, "sync clock is behind") {
		t.Fatalf("a healthy project must not be warned about:\n%s", out)
	}

	// Rewind only sync.json, leaving the journals alone — the shape the report
	// was filed from.
	sess, _, err := openSession(context.Background(), folder, false)
	if err != nil {
		t.Fatal(err)
	}
	st, err := sess.Store.LoadSync()
	if err != nil {
		t.Fatal(err)
	}
	hi := int64(0)
	all, err := sess.Store.AllOps()
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range all {
		if op.Lamport > hi {
			hi = op.Lamport
		}
	}
	if hi == 0 {
		t.Fatal("setup: the project has no ops")
	}
	st.Lamport = 0
	if err := sess.Store.SaveSync(st); err != nil {
		t.Fatal(err)
	}
	closeSession(sess)

	out, err = seccliRun(t, statusCmd(), []string{folder})
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	if !strings.Contains(out, "sync clock is behind") {
		t.Fatalf("a stuck device still reports healthy:\n%s", out)
	}
	if !strings.Contains(out, "bdrive sync") {
		t.Fatalf("the warning does not name the command that fixes it:\n%s", out)
	}
}
