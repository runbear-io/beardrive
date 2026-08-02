package webapp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/templates"
)

// Round 6 — the completeness floor. Three surfaces named in round 5's sweep
// and never reached by any round:
//
//  1. encodeCursor's UnixNano overflow past year 2262 (named in rounds 3, 4
//     and 5). Op.Time is a peer-controlled JSON field.
//  2. internal/templates' seeding path on the hub — a second write door that
//     does not call the guard every other write door calls.
//  3. account ids are 32 bits with no uniqueness check anywhere.
//
// Helpers are prefixed sec6 so they cannot collide with another agent's file
// in this package.

// sec6PushJournal writes a whole journal for device dev into project id
// through the public store API, as an ordinary member's syncing device does.
func sec6PushJournal(t *testing.T, h http.Handler, id, dev string, ops []map[string]any, c *http.Cookie) {
	t.Helper()
	var b strings.Builder
	for _, op := range ops {
		line, err := json.Marshal(op)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	req := httptest.NewRequest("PUT",
		"/api/p/"+id+"/store/object?key=journal/"+dev+".jsonl", strings.NewReader(b.String()))
	req.Header.Set("X-Bdrive-Device", dev)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("push journal %s: %d %s", dev, rec.Code, rec.Body)
	}
}

// sec6Op is one put op, with the wall-clock time the caller wants.
func sec6Op(dev string, seq int64, when time.Time, path string) map[string]any {
	return map[string]any{
		"seq": seq, "lamport": seq,
		"time":   when.UTC().Format(time.RFC3339Nano),
		"device": dev, "device_name": "Bob Laptop",
		"kind": "put", "path": path, "blob": strings.Repeat("a", 64), "size": 3,
	}
}

type sec6Page struct {
	Entries []HistoryEntry `json:"entries"`
	Next    string         `json:"next_cursor"`
}

func sec6History(t *testing.T, h http.Handler, id, query string, c *http.Cookie) sec6Page {
	t.Helper()
	rec := doAs(t, h, "GET", "/api/p/"+id+"/history?"+query, nil, c)
	if rec.Code != 200 {
		t.Fatalf("history?%s: %d %s", query, rec.Code, rec.Body)
	}
	var out sec6Page
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestSec_History_APeerCannotHideOlderChangesFromThePagingCursor
//
// History is the audit surface: it is where a project admin looks to find out
// what a member did. Paging is the only way past the first page, and the
// cursor is minted by encodeCursor, which stores the op's wall-clock time as
// `op.Time.UnixNano()`.
//
// time.Time.UnixNano() is documented as undefined for times outside
// [1678, 2262] — it silently wraps int64. Op.Time is arbitrary JSON in a
// member's own journal (row 15's known-open: journals are not validated at
// ingest). A time of 2300-01-01 encodes to -8032952073709551616, which
// decodeCursor reads back as 1715-06-13. The cursor no longer names the op it
// was minted from — it names a moment before every other entry in the feed —
// so handleHistory's skip loop ("advance while this entry is not older than
// the cursor") walks past EVERY remaining entry and hands back an empty page
// with no next_cursor.
//
// The reader sees a clean, complete-looking end of feed. Everything older is
// unreachable through the API, for every member of the project, from one
// ordinary journal push by one ordinary member.
//
// The secure behavior asserted: paging reaches every entry exactly once. That
// is the only property paging has, and it goes green the moment the cursor
// stops depending on an int64 that a peer can overflow.
func TestSec_History_APeerCannotHideOlderChangesFromThePagingCursor(t *testing.T) {
	h, _, c, p := permHub(t)

	// alice (project owner) pushes three ordinary changes.
	now := time.Now().UTC()
	sec6PushJournal(t, h, p.ID, "alice-desk", []map[string]any{
		sec6Op("alice-desk", 1, now.Add(-3*time.Hour), "notes/a.md"),
		sec6Op("alice-desk", 2, now.Add(-2*time.Hour), "notes/b.md"),
		sec6Op("alice-desk", 3, now.Add(-1*time.Hour), "notes/c.md"),
	}, c["alice"])

	// Control: with no hostile op present, paging one entry at a time reaches
	// all three. If this fails the harness is wrong, not the server.
	if got := sec6WalkPages(t, h, p.ID, c["alice"]); len(got) != 3 {
		t.Fatalf("control: paging n=1 over 3 ordinary ops reached %d entries %v, want 3", len(got), got)
	}

	// bob — a plain member with write permission, nothing more — pushes one
	// op into HIS OWN journal whose only unusual property is a date past 2262.
	sec6PushJournal(t, h, p.ID, "bob-laptop", []map[string]any{
		sec6Op("bob-laptop", 1, time.Date(2300, 1, 1, 0, 0, 0, 0, time.UTC), "notes/bob.md"),
	}, c["bob"])

	full := sec6History(t, h, p.ID, "n=100", c["alice"])
	if len(full.Entries) != 4 {
		t.Fatalf("unpaged feed has %d entries, want 4 (harness broken)", len(full.Entries))
	}

	got := sec6WalkPages(t, h, p.ID, c["alice"])
	if len(got) != 4 {
		t.Errorf("paging reached %d of %d history entries; a cursor minted from an op "+
			"dated past 2262 overflows UnixNano and skips the rest of the feed\nreached: %v",
			len(got), len(full.Entries), got)
	}
	seen := map[string]bool{}
	for _, path := range got {
		if seen[path] {
			t.Errorf("paging returned %q twice", path)
		}
		seen[path] = true
	}
	for _, e := range full.Entries {
		if !seen[e.Path] {
			t.Errorf("%q is in the unpaged feed but unreachable by paging", e.Path)
		}
	}
}

// sec6WalkPages pages the whole feed one entry at a time and returns the
// paths it reached, in order. It stops at 25 requests so a cursor that
// repeats itself fails the assertion instead of hanging the suite.
func sec6WalkPages(t *testing.T, h http.Handler, id string, c *http.Cookie) []string {
	t.Helper()
	var got []string
	q := "n=1"
	for i := 0; i < 25; i++ {
		page := sec6History(t, h, id, q, c)
		for _, e := range page.Entries {
			got = append(got, e.Path)
		}
		if page.Next == "" {
			return got
		}
		q = "n=1&cursor=" + page.Next
	}
	t.Errorf("paging did not terminate in 25 pages: the cursor is not advancing")
	return got
}

// TestSec_Seed_TemplateSeedingUsesTheSameGuardAsEveryOtherWriteDoor
//
// Every write door on the hub validates its destination path with
// cleanUploadPath before anything is journaled: handleUpload, handleRestore,
// handleRemove, the share minter. Its own comment says why — "a hub that
// journals one has already handed it to every device".
//
// seedTemplate (webapp/templates.go:48) is a second write door and it calls
// up.Upload directly with the template's path, with no guard at all. Today
// every Template comes from the go:embed'ed set, so the shipped structures
// are safe; the guard is missing all the same, and templates.Template is an
// exported struct with an exported Files field, so nothing but that embed
// stands between this door and a journaled `..` or `.git/hooks/pre-commit`.
// Round 4 found exactly this shape — the upload door and the sync door needed
// the same guard and only one had it.
//
// The secure behavior asserted: the seeding door refuses what the upload door
// refuses, and journals nothing when it does.
func TestSec_Seed_TemplateSeedingUsesTheSameGuardAsEveryOtherWriteDoor(t *testing.T) {
	hostile := []string{
		"../../../../etc/cron.d/pwned",
		".git/hooks/pre-commit",
		"/etc/passwd",
		".bdrive/config.json",
	}
	for _, bad := range hostile {
		t.Run(bad, func(t *testing.T) {
			srv, p, _ := newHub(t, true, nil)
			h := srv.Handler()

			// The delta: the same path through the upload door is refused.
			rec := do(t, h, "POST", "/api/p/"+p.ID+"/upload/init",
				map[string]any{"path": bad, "size": 1, "sha256": shaOf("x")})
			if rec.Code == 200 {
				t.Fatalf("upload door accepted %q (%d) — this test's premise is gone", bad, rec.Code)
			}

			tpl := templates.Template{Name: "hostile", Files: []templates.File{{Path: bad, Content: "x"}}}
			err := srv.seedTemplate(context.Background(), p.ID, tpl, User{Email: "alice@example.com"})
			if err == nil {
				t.Errorf("seedTemplate journaled %q; POST /upload/init refuses the same path with %d",
					bad, rec.Code)
			}

			// And nothing must have reached the journal even so.
			hist := do(t, h, "GET", "/api/p/"+p.ID+"/history?prefix=", nil)
			if strings.Contains(hist.Body.String(), strings.TrimPrefix(bad, "/")) {
				t.Errorf("history carries the refused path: %s", hist.Body)
			}
		})
	}
}

// TestSec_Account_AnIdCollisionMustNotDestroyALiveAccount
//
// authlocal.go:213-216 mints an account id as `"u-" + randHex(4)` — 32 bits —
// and installs it with `a.users[u.ID] = u`, with no check that the id is free.
// Neither backend has a uniqueness invariant either: fileAccountRepo.PutAccount
// is `r.users[u.ID] = u`, and sqlAccountRepo.PutAccount is an
// `ON CONFLICT(id) DO UPDATE SET email=…, pass=…` — the second account
// overwrites the first one's row, password hash included, permanently.
//
// The math, since it decides how this reads: a TARGETED collision needs ~2^32
// signups and is not reachable over HTTP. An UNTARGETED one is the birthday
// bound — 1.177 * 2^16 ≈ 77,000 accounts for even odds that some two accounts
// on one hub share an id, and ~9,300 accounts for a 1% chance. That is a
// managed hub's user table, not a thought experiment, and it needs no attacker
// at all.
//
// This test executes exactly the two statements createAccount executes when
// randHex hands back an id that is already taken, and then asks the service
// what it thinks. The secure behavior asserted is the invariant those two
// statements are missing: an id that names a live account is never reassigned,
// so a credential minted for one account never resolves to another.
func TestSec_Account_AnIdCollisionMustNotDestroyALiveAccount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	a, err := OpenBuiltinAuth(path, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	victim, err := a.signup("victim@corp.test", "Victim", "hunter2hunter2")
	if err != nil {
		t.Fatal(err)
	}
	vtok, err := a.issueToken(victim.ID, "victim-laptop")
	if err != nil {
		t.Fatal(err)
	}
	if u, ok := a.userForToken(vtok); !ok || u.Email != "victim@corp.test" {
		t.Fatalf("control: victim's own token resolves to %+v, ok=%v", u, ok)
	}

	// The collision: a later signup whose randHex(4) landed on the victim's id.
	// These are createAccount's own two lines, unguarded, with the id it could
	// legitimately mint.
	attacker := &authUser{
		ID: victim.ID, Email: "attacker@evil.test", Name: "Attacker",
		Pass: "$2a$10$notarealhash", Status: statusActive, Created: time.Now().UTC(),
	}
	// The store is the layer that has to refuse it: a row under a live id
	// carrying a different address is a collision, not an update. (As first
	// written this planted the clobber in a.users itself and then t.Fatal'd on
	// the very refusal it asks for, so neither outcome could pass — the
	// assertions below are unchanged and still fail on the code that shipped.)
	if err := a.store.PutAccount(attacker); err == nil {
		a.mu.Lock()
		a.users[attacker.ID] = attacker
		a.mu.Unlock()
		t.Errorf("the store accepted %q under %q's live account id %s",
			attacker.Email, "victim@corp.test", victim.ID)
	}

	if u, ok := a.userForToken(vtok); !ok || u.Email != "victim@corp.test" {
		t.Errorf("the victim's device token now authenticates as %q (ok=%v): "+
			"a 32-bit id collision transfers one account's live credentials onto another account's "+
			"identity and org memberships", u.Email, ok)
	}

	// And it is durable: reload the hub from the same file the way a restart
	// does, and ask whether the victim's account exists at all.
	b, err := OpenBuiltinAuth(path, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	found := b.findByEmail("victim@corp.test")
	b.mu.Unlock()
	if found == nil {
		t.Errorf("after a restart the victim's account is gone from disk entirely — "+
			"PutAccount overwrote the row (id %s), password hash included, with no way back", victim.ID)
	}
}
