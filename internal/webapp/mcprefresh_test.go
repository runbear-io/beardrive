package webapp

import (
	"sync"
	"testing"
	"time"
)

/* A revoked MCP token has to stop working.

   MCPAuth loaded every grant once in NewMCPAuth and then never read the repo
   again — no versionGate, though sqlMCPRepo has implemented Version() all
   along and every other registry (ProjectDB, OrgDB, ShareDB, DeviceRegistry)
   uses it. Alone on one process that is invisible, because the same object
   does the revoking. It stops being invisible the moment anything else writes
   the table: `bdrive mcp revoke` from a second process, an admin action on
   another instance, or a hub that will soon be allowed to run more than one
   (docs/hub-load-prd.md Phase 3).

   The failure mode is the bad direction. A grant created elsewhere is merely
   a 401 the client retries; a grant REVOKED elsewhere stays honoured for the
   life of the process, which on a long-running hub is indefinitely. */

func TestMCPGrantRevokedElsewhereStopsWorking(t *testing.T) {
	const token = "mcp-token-under-test"
	repo := newFakeMCPRepo()
	repo.put(MCPGrant{
		ID: "g1", Account: "a@x.io", ClientID: "c1",
		Projects: []string{"p1"}, Created: time.Now(),
		TokenDigest: hashToken(token),
	})

	m, err := NewMCPAuth(repo, nil, func(string) []MCPProject { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Grant(token); !ok {
		t.Fatal("the grant was not honoured to begin with; this test proves nothing")
	}

	// Somebody else revokes it — another process, another instance, the CLI.
	repo.remove("g1")

	if g, ok := m.Grant(token); ok {
		t.Fatalf("a token revoked in the store is still honoured (grant %q, account %q); "+
			"nothing re-reads the repo, so this holds for the life of the process",
			g.ID, g.Account)
	}
}

// The other direction, so the gate is a re-read and not a cache wipe: a grant
// added elsewhere becomes usable without a restart.
func TestMCPGrantCreatedElsewhereIsHonoured(t *testing.T) {
	const token = "mcp-token-created-later"
	repo := newFakeMCPRepo()
	m, err := NewMCPAuth(repo, nil, func(string) []MCPProject { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Grant(token); ok {
		t.Fatal("a token nobody has issued was honoured")
	}

	repo.put(MCPGrant{
		ID: "g2", Account: "b@x.io", ClientID: "c1",
		Projects: []string{"p1"}, Created: time.Now(),
		TokenDigest: hashToken(token),
	})

	if _, ok := m.Grant(token); !ok {
		t.Fatal("a grant created in the store is not honoured; the registry never re-reads")
	}
}

// An expired grant is still refused, and the re-read must not resurrect it.
func TestMCPExpiredGrantStaysRefusedAcrossARefresh(t *testing.T) {
	const token = "mcp-token-expired"
	repo := newFakeMCPRepo()
	repo.put(MCPGrant{
		ID: "g3", Account: "c@x.io", ClientID: "c1",
		Created: time.Now().Add(-time.Hour), Expires: time.Now().Add(-time.Minute),
		TokenDigest: hashToken(token),
	})
	m, err := NewMCPAuth(repo, nil, func(string) []MCPProject { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Grant(token); ok {
		t.Fatal("an expired grant was honoured")
	}
	repo.bump() // force a re-read
	if _, ok := m.Grant(token); ok {
		t.Fatal("a re-read resurrected an expired grant")
	}
}

// --- a repo a second process can write to --------------------------------

type fakeMCPRepo struct {
	mu      sync.Mutex
	grants  map[string]MCPGrant
	clients map[string]MCPClient
	version int
}

func newFakeMCPRepo() *fakeMCPRepo {
	return &fakeMCPRepo{grants: map[string]MCPGrant{}, clients: map[string]MCPClient{}}
}

// put and remove stand in for another process writing the table: they move the
// version the way a real store's change token moves.
func (r *fakeMCPRepo) put(g MCPGrant) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.grants[g.ID] = g
	r.version++
}

func (r *fakeMCPRepo) remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.grants, id)
	r.version++
}

func (r *fakeMCPRepo) bump() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.version++
}

func (r *fakeMCPRepo) Version() (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return "v" + itoa(r.version), nil
}

func (r *fakeMCPRepo) LoadGrants() ([]MCPGrant, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]MCPGrant, 0, len(r.grants))
	for _, g := range r.grants {
		out = append(out, g)
	}
	return out, nil
}

func (r *fakeMCPRepo) PutGrant(g MCPGrant) error   { r.put(g); return nil }
func (r *fakeMCPRepo) DeleteGrant(id string) error { r.remove(id); return nil }
func (r *fakeMCPRepo) PutClient(c MCPClient) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clients[c.ID] = c
	r.version++
	return nil
}
func (r *fakeMCPRepo) LoadClients() ([]MCPClient, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]MCPClient, 0, len(r.clients))
	for _, c := range r.clients {
		out = append(out, c)
	}
	return out, nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
