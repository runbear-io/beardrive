package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
)

// ops builds a newest-first log like syncer.LogEntries returns.
func ops(specs ...[3]string) []journal.Op {
	var out []journal.Op
	base := time.Date(2026, 7, 28, 14, 0, 0, 0, time.UTC)
	for i := len(specs) - 1; i >= 0; i-- { // specs are oldest-first; log is newest-first
		s := specs[i]
		op := journal.Op{Kind: s[0], Path: s[1], Blob: s[2], Time: base.Add(time.Duration(i) * time.Minute)}
		if s[0] == journal.KindPut {
			op.Size = 10
			op.Lamport = int64(i + 1)
		} else {
			op.Lamport = int64(i + 1)
		}
		out = append(out, op)
	}
	return out
}

const (
	v1 = "a3f9c1e2000000000000000000000000000000000000000000000000000000aa"
	v2 = "b7d40000000000000000000000000000000000000000000000000000000000bb"
	v3 = "a3f900000000000000000000000000000000000000000000000000000000cccc"
)

// The previous version is the last content that differs from what the file
// holds now — not simply "the op before this one", which is wrong as soon as
// a run wrote the same path twice.
func TestPickPreviousVersion(t *testing.T) {
	log := ops(
		[3]string{journal.KindPut, "f.md", v1},
		[3]string{journal.KindPut, "f.md", v2},
		[3]string{journal.KindPut, "f.md", v2}, // same content written twice
	)
	vs := versionsOf(log, "f.md")
	got, err := pickVersion(vs, currentBlob(log, "f.md"), "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Blob != v1 {
		t.Fatalf("previous = %s, want v1 (%s)", got.Blob, v1)
	}
}

// Latest op is a delete: the file has no current content, so "previous" is
// simply its last content — which is what makes restoring a deleted file work.
func TestPickPreviousAfterDelete(t *testing.T) {
	log := ops(
		[3]string{journal.KindPut, "f.md", v1},
		[3]string{journal.KindPut, "f.md", v2},
		[3]string{journal.KindDelete, "f.md", ""},
	)
	got, err := pickVersion(versionsOf(log, "f.md"), currentBlob(log, "f.md"), "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Blob != v2 {
		t.Fatalf("previous after delete = %s, want v2", got.Blob)
	}
}

// The only version there has ever been is not restorable — say so instead of
// writing the same bytes back.
func TestPickPreviousWhenOnlyVersion(t *testing.T) {
	log := ops([3]string{journal.KindPut, "f.md", v1})
	if _, err := pickVersion(versionsOf(log, "f.md"), currentBlob(log, "f.md"), ""); err == nil {
		t.Fatal("want an error when there is no earlier version")
	}
}

func TestPickByShortSHA(t *testing.T) {
	log := ops(
		[3]string{journal.KindPut, "f.md", v1},
		[3]string{journal.KindPut, "f.md", v3}, // shares the "a3f9" prefix with v1
		[3]string{journal.KindPut, "f.md", v2},
	)
	vs, cur := versionsOf(log, "f.md"), currentBlob(log, "f.md")

	got, err := pickVersion(vs, cur, "a3f9c1")
	if err != nil || got.Blob != v1 {
		t.Fatalf("unique prefix → %v, %v", got.Blob, err)
	}
	if _, err := pickVersion(vs, cur, "a3f9"); err == nil || !strings.Contains(err.Error(), "more characters") {
		t.Fatalf("ambiguous prefix must refuse, got %v", err)
	}
	if _, err := pickVersion(vs, cur, "ffff"); err == nil {
		t.Fatal("unknown prefix must error")
	}
}

// LogEntries' path filter also matches directories and prefixes, so the
// command re-filters: restoring the wrong file would be the worst bug here.
func TestVersionsOfIsExactPath(t *testing.T) {
	log := ops(
		[3]string{journal.KindPut, "docs/f.md", v1},
		[3]string{journal.KindPut, "docs/f.md.bak", v2},
		[3]string{journal.KindPut, "docs", v3},
	)
	vs := versionsOf(log, "docs/f.md")
	if len(vs) != 1 || vs[0].Blob != v1 {
		t.Fatalf("versionsOf = %+v, want just docs/f.md", vs)
	}
	if len(versionsOf(log, "nope.md")) != 0 {
		t.Fatal("a path with no history must yield no versions")
	}
}

func TestPrintVersionsMarksCurrent(t *testing.T) {
	log := ops(
		[3]string{journal.KindPut, "f.md", v1},
		[3]string{journal.KindPut, "f.md", v2},
	)
	var out bytes.Buffer
	printVersions(&out, versionsOf(log, "f.md"), currentBlob(log, "f.md"))
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("listing = %d lines, want 2:\n%s", len(lines), out.String())
	}
	if !strings.HasPrefix(lines[0], "* "+v2[:8]) {
		t.Fatalf("current version not marked: %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "  "+v1[:8]) {
		t.Fatalf("older version line = %q", lines[1])
	}
}
