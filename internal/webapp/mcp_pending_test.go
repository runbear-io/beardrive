package webapp

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

/* An MCP consent has to be exchangeable by a process that did not mint it.

   The browser POSTs consent to whichever instance the load balancer picked,
   and the MCP client then exchanges the code over a completely separate
   connection — so behind more than one instance those two hops land on
   different processes roughly half the time. While the codes lived in a map
   inside MCPAuth, the second hop simply did not know the code existed, and the
   sign-in failed with "unknown or expired code" for no reason a user could
   act on. It also meant a hub restart cancelled an in-flight consent.

   The full OAuth round trip through one process is covered by mcp_test.go.
   What this adds is the property that round trip cannot show: the code is in
   SHARED storage, and a second MCPAuth over the same repos can complete the
   exchange the first one started. */

func TestMCPConsentIsExchangeableByAnotherProcess(t *testing.T) {
	dir := t.TempDir()
	shared := newFilePendingRepo(filepath.Join(dir, "pending.json"))
	grants := newFileMCPRepo(filepath.Join(dir, "mcp.json"))

	// The instance the browser happened to reach.
	a, err := NewMCPAuth(grants, shared, nil, func(string) []MCPProject { return nil })
	if err != nil {
		t.Fatal(err)
	}
	// A different instance, same storage. Nothing is shared in memory.
	b, err := NewMCPAuth(grants, shared, nil, func(string) []MCPProject { return nil })
	if err != nil {
		t.Fatal(err)
	}

	const code = "mcpa_cross_process"
	payload, err := json.Marshal(mcpCode{
		GrantID: "g-cross", ClientID: "c1", Redirect: "http://127.0.0.1:9999/cb",
		Expires: time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	// What a's authorize handler records.
	if err := a.pending.Put(PendingGrant{
		Kind: pendingMCPCode, Key: code, Payload: payload,
		Expires: time.Now().Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	// What b's token handler does with it.
	row, ok, err := b.pending.Take(pendingMCPCode, code)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("the instance that did not mint the consent could not find it; " +
			"behind two instances this is every other MCP sign-in failing with " +
			"\"unknown or expired code\"")
	}
	var got mcpCode
	if err := json.Unmarshal(row.Payload, &got); err != nil {
		t.Fatalf("a consent written by one process is unreadable by another: %v", err)
	}
	if got.GrantID != "g-cross" || got.ClientID != "c1" {
		t.Fatalf("consent round-trip lost fields: %+v", got)
	}

	// And it is still single-use across the boundary, which is the half that
	// matters for security: two clients racing one code must not both win.
	if _, ok, _ := a.pending.Take(pendingMCPCode, code); ok {
		t.Fatal("a consent consumed on one instance was still available on another")
	}
}

// A grant that a consent created and nobody ever exchanged must not accumulate
// forever. The old sweep walked the in-memory codes; keying on the grant's own
// age is what survives the codes moving out of the process.
func TestMCPUnclaimedGrantIsPruned(t *testing.T) {
	dir := t.TempDir()
	shared := newFilePendingRepo(filepath.Join(dir, "pending.json"))
	grants := newFileMCPRepo(filepath.Join(dir, "mcp.json"))

	// Old and never exchanged: no token was ever issued against it.
	if err := grants.PutGrant(MCPGrant{
		ID: "stale", Account: "a@x.io", ClientID: "c1",
		Created: time.Now().Add(-10 * mcpCodeTTL),
	}); err != nil {
		t.Fatal(err)
	}
	// Old, but in daily use — a code can expire long after its grant was
	// exchanged, and dropping this one would sign a working client out.
	if err := grants.PutGrant(MCPGrant{
		ID: "live", Account: "a@x.io", ClientID: "c1",
		Created: time.Now().Add(-10 * mcpCodeTTL), TokenDigest: hashToken("still-working"),
	}); err != nil {
		t.Fatal(err)
	}

	m, err := NewMCPAuth(grants, shared, nil, func(string) []MCPProject { return nil })
	if err != nil {
		t.Fatal(err)
	}
	m.pruneOrphanGrants()

	if _, ok := m.Grant("still-working"); !ok {
		t.Fatal("pruning unclaimed grants revoked one that had a token; that signs a working client out")
	}
	left, err := grants.LoadGrants()
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range left {
		if g.ID == "stale" {
			t.Fatal("a grant no consent ever claimed was left in the store")
		}
	}
}
