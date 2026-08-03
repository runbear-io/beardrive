package syncer

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runbear-io/beardrive/internal/journal"
)

// The scan door and the hub's ingest door must agree about which paths exist.
//
// They did not. walkFolder applied config.ReservedPath and Filter.SkipUp; the
// hub's journalOps applied journal.SafePath AND config.ReservedPath. A file
// whose name the scan accepted and the hub refused was therefore blobbed and
// journaled locally, and then — because push PUTs the whole journal object —
// every subsequent push from that device 400'd on the same op, forever.
// Renaming the file did not help: the delete op names the same path.
//
// walk.go's own comment already stated the rule it was breaking: "the outbound
// half has to match the inbound one or a file lands on the hub that no peer
// will ever materialize." It just did not name every predicate the inbound
// half applies.
//
// This is the client-side twin of the scan-vs-ingest disagreement round 7
// closed on the hub. The assertion is deliberately a PROPERTY over both
// predicates rather than a list of characters, so a future change to either
// door cannot reopen the gap by adding a rule to only one of them.
func TestSec_Path_ScanRefusesEveryPathIngestRefuses(t *testing.T) {
	paths := []string{
		// Refused by SafePath, so the scan must refuse them too. Written as
		// escapes: a literal BOM is not legal in a Go source file, which is
		// itself a small reminder of why these do not belong in a filename.
		"note\u200bs.md",        // ZWSP
		"in\u202evoice.md",      // RLO
		"a\ufeffb.md",           // BOM
		"line\u2028sep.md",      // Zl
		"csi\u009bhere.md",      // C1
		"tag\U000e0041block.md", // tag block
		"nul\x00byte.md",        // C0
		"del\x7fhere.md",        // DEL
		// Accepted by SafePath, so the scan must carry them: these are real
		// filenames, not payloads. ZWNJ is required Persian orthography and
		// ZWJ builds most emoji.
		"\u0645\u06cc\u200c\u0631\u0648\u0645.md",
		"team\u200d.md",
		"ordinary.md",
		"\u05e7\u05d5\u05d1\u05e5.md", // Hebrew, strong RTL, no controls
	}

	dir := t.TempDir()
	for _, p := range paths {
		// Only skip names the FILESYSTEM cannot hold. Everything else is
		// created for real — a ZWSP, an RLO, a BOM, U+2028, a C1 and the tag
		// block are all legal bytes in a unix filename, which is the whole
		// problem. An earlier draft of this test skipped every !SafePath name
		// and was therefore vacuous: with nothing hostile on disk, deleting
		// the guard under test changed nothing and the test still passed.
		if strings.ContainsAny(p, "\x00/") {
			continue
		}
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	filter, err := loadFilter(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	if err := walkFolder(dir, filter, func(_, rel string, _ fs.DirEntry, v verdict) error {
		if v == vSync {
			seen[rel] = true
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// The property: anything the scan hands onward must be a path the hub
	// will accept. A violation here is a device that journals an op no hub
	// will take and cannot push again.
	for rel := range seen {
		if !journal.SafePath(rel) {
			t.Errorf("the scan produced %q, which journal.SafePath refuses — "+
				"the hub's /store/* door 400s the whole journal body on this op, "+
				"and push re-sends that body every cycle, so this device can "+
				"never push again", rel)
		}
	}

	// And the control, which is the half that makes the property meaningful:
	// ordinary and non-Latin filenames must still reach the hub. A scan that
	// refused everything would satisfy the loop above and be useless.
	for _, want := range []string{
		"ordinary.md",
		"\u0645\u06cc\u200c\u0631\u0648\u0645.md",
		"\u05e7\u05d5\u05d1\u05e5.md",
	} {
		if !seen[want] {
			t.Errorf("the scan dropped %q, a legitimate filename — refusing real "+
				"orthography is not hardening, it tells those users their files "+
				"cannot sync", want)
		}
	}
}
