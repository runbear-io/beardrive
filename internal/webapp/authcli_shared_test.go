package webapp

import (
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

/* `bdrive login` has to survive landing on a different process each hop.

   The device flow is three requests and they are three separate connections:
   the CLI POSTs /api/auth/device/start, a HUMAN opens the approval link in a
   browser, and the CLI polls /api/auth/device/poll. Behind two instances
   there is no reason any two of those reach the same process — that is what a
   load balancer is for — and while pending grants lived in a map inside
   CLIAuth, the second hop simply did not know the sign-in existed.

   This is the Stage 8 success criterion in miniature (docs/hub-load-prd.md):
   mint on one instance, approve on another, poll back on the first. */

func twoCLIAuths(t *testing.T) (*CLIAuth, *CLIAuth) {
	t.Helper()
	shared := newFilePendingRepo(filepath.Join(t.TempDir(), "pending.json"))
	noSession := func(*http.Request) (User, bool) { return User{}, false }
	noIssue := func(http.ResponseWriter, *http.Request, string, string) {}

	a := NewCLIAuth(noSession, noIssue)
	a.UsePending(shared)
	b := NewCLIAuth(noSession, noIssue)
	b.UsePending(shared)
	return a, b
}

func TestDeviceLoginSpansTwoProcesses(t *testing.T) {
	a, b := twoCLIAuths(t)

	// 1. The CLI reaches instance A.
	id := a.newGrant(cliGrant{
		Kind: "device", Link: "link-abc", Device: "laptop", OS: "darwin", IP: "203.0.113.9",
	}, 10*time.Minute)
	if id == "" {
		t.Fatal("instance A refused to mint the grant")
	}

	// 2. The human's browser reaches instance B, which has never heard of it.
	gotID, ok := b.grantByLink("device", "link-abc")
	if !ok {
		t.Fatal("the approval link did not resolve on the instance the browser " +
			"happened to reach; behind two instances that is half of all sign-ins " +
			"showing the user an expired-link page for a link they just received")
	}
	if gotID != id {
		t.Fatalf("the link resolved to %q, want %q", gotID, id)
	}
	if !b.approveDevice(gotID, "u-123") {
		t.Fatal("instance B could not approve a grant instance A minted")
	}

	// 3. The CLI polls, and reaches A again.
	g, exists, won := a.takeGranted("device", id)
	if !exists {
		t.Fatal("the polling instance lost the grant it had minted")
	}
	if !won {
		t.Fatal("the poll did not see the approval that landed on another instance; " +
			"`bdrive login` would hang until it timed out")
	}
	if g.User != "u-123" {
		t.Fatalf("approval carried user %q, want u-123", g.User)
	}
}

/* A grant is single-use ACROSS processes, not just within one.

   takeGranted used to peek, decide, and take in a second critical section —
   so every poll in flight when a human approved got its own permanent token.
   That was fixed inside one process by consuming in the same step that
   decides. Two instances polling one grant is the same bug with a load
   balancer in the middle, and it stays closed only because the store's Take
   is atomic. */
func TestAnApprovedGrantIsWonByExactlyOneProcess(t *testing.T) {
	a, b := twoCLIAuths(t)

	id := a.newGrant(cliGrant{Kind: "device", Link: "link-race", IP: "203.0.113.9"}, 10*time.Minute)
	if id == "" {
		t.Fatal("mint refused")
	}
	if !a.approveDevice(id, "u-race") {
		t.Fatal("approve failed")
	}

	_, _, wonA := a.takeGranted("device", id)
	_, _, wonB := b.takeGranted("device", id)

	if wonA && wonB {
		t.Fatal("two processes each won the same approved sign-in; one human approval " +
			"would mint two independently valid, permanently revocable-by-nothing tokens")
	}
	if !wonA && !wonB {
		t.Fatal("nobody won an approved grant, so the sign-in cannot complete at all")
	}
}

// The browser-callback flow is two hops rather than three, and has the same
// requirement: the code is minted where the CLI asked and redeemed wherever
// the browser's callback lands.
func TestBrowserCodeIsRedeemableOnAnotherProcess(t *testing.T) {
	a, b := twoCLIAuths(t)

	id := a.newGrant(cliGrant{Kind: "code", Challenge: "abc", User: "u-9", Granted: true}, time.Minute)
	if id == "" {
		t.Fatal("mint refused")
	}
	g, ok := b.take("code", id)
	if !ok {
		t.Fatal("a browser-flow code could not be redeemed on another instance")
	}
	if g.Challenge != "abc" || g.User != "u-9" {
		t.Fatalf("code round-trip lost fields: %+v", g)
	}
	if _, ok := a.take("code", id); ok {
		t.Fatal("a single-use code was redeemable twice across processes")
	}
}

// Without a shared store the default is unchanged, which is what a
// single-instance hub should keep doing — and is what makes the tests above
// mean something.
func TestWithoutASharedStoreAGrantStaysInItsProcess(t *testing.T) {
	noSession := func(*http.Request) (User, bool) { return User{}, false }
	noIssue := func(http.ResponseWriter, *http.Request, string, string) {}
	a := NewCLIAuth(noSession, noIssue)
	b := NewCLIAuth(noSession, noIssue)

	id := a.newGrant(cliGrant{Kind: "device", Link: "link-solo"}, time.Minute)
	if id == "" {
		t.Fatal("mint refused")
	}
	if _, ok := b.grantByLink("device", "link-solo"); ok {
		t.Fatal("a grant reached another CLIAuth with no store between them")
	}
	if _, ok := a.grantByLink("device", "link-solo"); !ok {
		t.Fatal("a grant did not resolve in the process that minted it")
	}
}
