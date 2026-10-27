package webapp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
)

// An agent editing through MCP and a person saving in the browser write the
// same file at once. Each writer reads, adds its own line, and writes back
// held to what it read, retrying when told it is stale. Every line must
// survive: a writer that overwrote a version it never saw would erase one.
//
// MCP used to hold a lock of its own around a check of its own and then write
// without a precondition, so a browser save landing between its read and its
// write was replaced wholesale.
func TestMCPEditAndBrowserSaveCannotLoseAnUpdate(t *testing.T) {
	f := newMCPHub(t)
	token := f.connect("alice", f.wiki.ID)
	file := "/api/p/" + f.wiki.ID + "/file?path=race.md"
	put := "/api/p/" + f.wiki.ID + "/upload/content?path=race.md"
	browser := &http.Client{}
	send := func(method, url string, body io.Reader, hdr http.Header) (*http.Response, []byte, error) {
		req, _ := http.NewRequest(method, f.ts.URL+url, body)
		req.AddCookie(f.cookies["alice"])
		for k, v := range hdr {
			req.Header[k] = v
		}
		resp, err := browser.Do(req)
		if err != nil {
			return nil, nil, err
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		return resp, b, err
	}
	if resp, b, err := send("PUT", put, strings.NewReader("END\n"), nil); err != nil || resp.StatusCode != 200 {
		t.Fatalf("seed: %v %s", err, b)
	}

	const writers, each = 3, 6
	var wg sync.WaitGroup
	errs := make(chan error, 2*writers)
	for w := 0; w < writers; w++ {
		cs := f.session(token)
		wg.Add(2)
		go func() { // the agent
			defer wg.Done()
			for i := 0; i < each; i++ {
				line := fmt.Sprintf("mcp-%d-%d", w, i)
				for {
					res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "edit", Arguments: map[string]any{
						"path": "/" + f.wiki.ID + "/race.md", "old_string": "END", "new_string": line + "\nEND",
					}})
					if err != nil {
						errs <- err
						return
					}
					text := toolText(res)
					if !res.IsError {
						break
					}
					if !strings.Contains(text, "stale") {
						errs <- fmt.Errorf("edit: %s", text)
						return
					}
				}
			}
		}()
		go func() { // the person
			defer wg.Done()
			for i := 0; i < each; i++ {
				line := fmt.Sprintf("web-%d-%d", w, i)
				for {
					resp, body, err := send("GET", file, nil, nil)
					if err != nil || resp.StatusCode != 200 {
						errs <- fmt.Errorf("read: %v %s", err, body)
						return
					}
					next := strings.Replace(string(body), "END", line+"\nEND", 1)
					resp, out, err := send("PUT", put, strings.NewReader(next),
						http.Header{"If-Match": {resp.Header.Get("ETag")}})
					if err != nil {
						errs <- err
						return
					}
					if resp.StatusCode == 200 {
						break
					}
					if resp.StatusCode != http.StatusConflict {
						errs <- fmt.Errorf("save: %d %s", resp.StatusCode, out)
						return
					}
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	_, final, err := send("GET", file, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for w := 0; w < writers; w++ {
		for i := 0; i < each; i++ {
			for _, line := range []string{fmt.Sprintf("mcp-%d-%d", w, i), fmt.Sprintf("web-%d-%d", w, i)} {
				if !strings.Contains(string(final), line+"\n") {
					t.Fatalf("%s was acknowledged and then lost — a lost update. Final file:\n%s", line, final)
				}
			}
		}
	}
}

func toolText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// casRoot is a file:// root that can compare and swap, the way GCS can, with
// a hook that runs in the window between our read of a version and our
// conditional write — standing in for another hub process.
type casRoot struct {
	remote.Backend
	mu            sync.Mutex
	beforePutIf   func(key string) // before a conditional journal write
	beforeBlobPut func(key string) // before a conditional blob write
	conflicts     int
}

func versionOf(b []byte) remote.Version {
	sum := sha256.Sum256(b)
	return remote.Version(hex.EncodeToString(sum[:8]))
}

func (c *casRoot) read(ctx context.Context, key string) ([]byte, remote.Version, error) {
	rc, err := c.Backend.Get(ctx, key)
	if err != nil {
		if remote.IsNotExist(err) {
			return nil, remote.VersionAbsent, nil
		}
		return nil, remote.VersionAbsent, err
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	return b, versionOf(b), err
}

func (c *casRoot) GetVersioned(ctx context.Context, key string) (io.ReadCloser, remote.Version, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, v, err := c.read(ctx, key)
	if err != nil {
		return nil, remote.VersionAbsent, err
	}
	if v == remote.VersionAbsent {
		return nil, remote.VersionAbsent, errNotExist
	}
	return io.NopCloser(bytes.NewReader(b)), v, nil
}

func (c *casRoot) PutIf(ctx context.Context, key string, r io.Reader, _ int64, expect remote.Version) (remote.Version, error) {
	hook := &c.beforePutIf
	if strings.Contains(key, "blobs/") {
		hook = &c.beforeBlobPut
	}
	if f := *hook; f != nil {
		*hook = nil
		f(key)
	}
	body, err := io.ReadAll(r)
	if err != nil {
		return remote.VersionAbsent, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, cur, err := c.read(ctx, key)
	if err != nil {
		return remote.VersionAbsent, err
	}
	if cur != expect {
		c.conflicts++
		return remote.VersionAbsent, remote.ErrVersionMismatch
	}
	if err := c.Backend.Put(ctx, key, bytes.NewReader(body), int64(len(body))); err != nil {
		return remote.VersionAbsent, err
	}
	return versionOf(body), nil
}

// theOtherProcessAppends writes one op to the hub's own journal the way a
// second hub process would: same device, next seq, a later lamport.
func theOtherProcessAppends(t *testing.T, c *casRoot, key, path, content string) {
	t.Helper()
	ctx := context.Background()
	c.Backend.Put(ctx, strings.TrimSuffix(key, "journal/"+webDevice.ID+".jsonl")+"blobs/"+shaOf(content),
		strings.NewReader(content), int64(len(content)))
	existing, _, err := c.read(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	ops, _ := journal.Parse(existing)
	var seq, lam int64
	for _, op := range ops {
		seq, lam = max(seq, op.Seq), max(lam, op.Lamport)
	}
	line, _ := journal.Marshal([]journal.Op{{
		Seq: seq + 1, Lamport: lam + 1, Time: time.Now().UTC(), Device: webDevice.ID,
		Kind: journal.KindPut, Path: path, Blob: shaOf(content), Size: int64(len(content)),
	}})
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Backend.Put(ctx, key, bytes.NewReader(append(existing, line...)), int64(len(existing)+len(line)))
}

func casHub(t *testing.T) (*casRoot, http.Handler, Project) {
	t.Helper()
	var root *casRoot
	srv, p, _ := newHub(t, true, func(be remote.Backend) remote.Backend {
		root = &casRoot{Backend: be}
		return root
	})
	return root, srv.Handler(), p
}

func putIfMatch(h http.Handler, project, path, content, base string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("PUT", "/api/p/"+project+"/upload/content?path="+path, strings.NewReader(content))
	if base != "" {
		req.Header.Set("If-Match", `"`+base+`"`)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func hubOps(t *testing.T, c *casRoot, project string) []journal.Op {
	t.Helper()
	b, _, err := c.read(context.Background(), project+"/journal/"+webDevice.ID+".jsonl")
	if err != nil {
		t.Fatal(err)
	}
	ops, _ := journal.Parse(b)
	return ops
}

// Behind more than one hub process, the base check a handler makes against
// its own snapshot cannot see a write the other process has just appended.
// The append's compare-and-swap does: the other write moves the journal, the
// conditional write fails, and the retry finds that the version this write
// was built on is no longer the one there.
func TestABaseIsRecheckedInsideTheCompareAndSwapLoop(t *testing.T) {
	root, h, p := casHub(t)
	if rec := putIfMatch(h, p.ID, "note.md", "v1", ""); rec.Code != 200 {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body)
	}
	root.beforePutIf = func(key string) { theOtherProcessAppends(t, root, key, "note.md", "theirs") }

	rec := putIfMatch(h, p.ID, "note.md", "ours", shaOf("v1"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("a write built on v1 landed over the other process's version: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		SHA string `json:"sha"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out.SHA != shaOf("theirs") {
		t.Fatalf("conflict names %q, want the version that replaced the base", out.SHA)
	}
	if root.conflicts == 0 {
		t.Fatal("the conditional write never failed, so the retry was not exercised and this proves nothing")
	}
	for _, op := range hubOps(t, root, p.ID) {
		if op.Blob == shaOf("ours") {
			t.Fatal("the refused write was appended anyway")
		}
	}
}

// A retry stamps its ops from the journal as it then stands. It used to
// re-append the first attempt's bytes, reusing the seq the other process had
// just taken: two ops sharing (device, seq), which is meant to name one op.
func TestARetriedAppendTakesTheNextSeq(t *testing.T) {
	root, h, p := casHub(t)
	if rec := putIfMatch(h, p.ID, "a.md", "one", ""); rec.Code != 200 {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body)
	}
	root.beforePutIf = func(key string) { theOtherProcessAppends(t, root, key, "other.md", "theirs") }
	if rec := putIfMatch(h, p.ID, "b.md", "two", ""); rec.Code != 200 {
		t.Fatalf("write: %d %s", rec.Code, rec.Body)
	}
	seen := map[int64]string{}
	for _, op := range hubOps(t, root, p.ID) {
		if prev, ok := seen[op.Seq]; ok {
			t.Fatalf("seq %d names two ops (%s and %s)", op.Seq, prev, op.Path)
		}
		seen[op.Seq] = op.Path
	}
	if len(seen) != 3 {
		t.Fatalf("journal holds %d ops, want 3 (seed, theirs, ours): %v", len(seen), seen)
	}
}

// The hub's next seq counts only its own journal. An op's Device field is
// JSON its pusher chose: a member's op naming the hub with seq MaxInt64 used
// to wrap every later hub op onto one (device, seq) slot.
func TestTheHubSeqIgnoresOpsForgedInAnotherJournal(t *testing.T) {
	root, h, p := casHub(t)
	line, _ := journal.Marshal([]journal.Op{{
		Seq: math.MaxInt64, Lamport: 1, Time: time.Now().UTC(), Device: webDevice.ID,
		Kind: journal.KindPut, Path: "forged.md", Blob: shaOf("x"), Size: 1,
	}})
	if err := root.Backend.Put(context.Background(), p.ID+"/journal/laptop.jsonl", bytes.NewReader(line), int64(len(line))); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.md", "b.md"} {
		if rec := putIfMatch(h, p.ID, name, name, ""); rec.Code != 200 {
			t.Fatalf("write %s: %d %s", name, rec.Code, rec.Body)
		}
	}
	seen := map[int64]string{}
	for _, op := range hubOps(t, root, p.ID) {
		if op.Seq <= 0 || seen[op.Seq] != "" {
			t.Fatalf("hub op %s took seq %d (seen: %v)", op.Path, op.Seq, seen)
		}
		seen[op.Seq] = op.Path
	}
}

// A write refused for a stale base had already stored its blob. It is billed
// then, once: a retry of the same bytes finds the blob there and adds nothing.
func TestARefusedWriteStillBillsTheBlobItStored(t *testing.T) {
	var root *casRoot
	srv, p, _ := newHub(t, true, func(be remote.Backend) remote.Backend {
		root = &casRoot{Backend: be}
		return root
	})
	q := &recQuota{}
	srv.Quota = q
	h := srv.Handler()
	if rec := putIfMatch(h, p.ID, "note.md", "v1", ""); rec.Code != 200 {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body)
	}
	billed := func() (n int64) {
		q.mu.Lock()
		defer q.mu.Unlock()
		for _, u := range q.usage {
			n += u.bytes
		}
		return n
	}
	before := billed()
	root.beforePutIf = func(key string) { theOtherProcessAppends(t, root, key, "note.md", "theirs") }
	if rec := putIfMatch(h, p.ID, "note.md", "ours", shaOf("v1")); rec.Code != http.StatusConflict {
		t.Fatalf("stale write: %d %s", rec.Code, rec.Body)
	}
	if got := billed() - before; got != int64(len("ours")) {
		t.Fatalf("a refused write that stored its blob billed %d bytes, want %d", got, len("ours"))
	}
	if rec := putIfMatch(h, p.ID, "note.md", "ours", shaOf("theirs")); rec.Code != 200 {
		t.Fatalf("retry: %d %s", rec.Code, rec.Body)
	}
	if got := billed() - before; got != int64(len("ours")) {
		t.Fatalf("the retry billed again: %d bytes in all, want %d", got, len("ours"))
	}
}

// Two hub processes upload the same new bytes. The one that stores the blob is
// billed for it. The other found it stored and is billed nothing, even when
// its own op is then refused (which used to bill it again).
func TestOnlyTheWriteThatStoresABlobIsBilledForIt(t *testing.T) {
	var root *casRoot
	srv, p, _ := newHub(t, true, func(be remote.Backend) remote.Backend {
		root = &casRoot{Backend: be}
		return root
	})
	q := &recQuota{}
	srv.Quota = q
	h := srv.Handler()
	if rec := putIfMatch(h, p.ID, "note.md", "v1", ""); rec.Code != 200 {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body)
	}
	billed := func() (n int64) {
		q.mu.Lock()
		defer q.mu.Unlock()
		for _, u := range q.usage {
			n += u.bytes
		}
		return n
	}
	before := billed()
	ctx := context.Background()
	root.beforeBlobPut = func(key string) { // the other process stored the same bytes first
		root.Backend.Put(ctx, key, strings.NewReader("ours"), 4)
	}
	root.beforePutIf = func(key string) { theOtherProcessAppends(t, root, key, "note.md", "theirs") }
	if rec := putIfMatch(h, p.ID, "note.md", "ours", shaOf("v1")); rec.Code != http.StatusConflict {
		t.Fatalf("stale write: %d %s", rec.Code, rec.Body)
	}
	if got := billed() - before; got != 0 {
		t.Fatalf("a write that found its blob already stored was billed %d bytes", got)
	}
}

// No request writes the hub's own journal, the project-admin recovery path
// included: every op the hub stamps takes its seq from that journal.
func TestTheHubJournalIsWrittenOnlyByTheHub(t *testing.T) {
	h, srv, cookies, p := permHub(t)
	forged := `{"kind":"put","path":"x.md","seq":9223372036854775807,"lamport":1,"device":"` + srv.Device.ID + `"}` + "\n"
	req := httptest.NewRequest("PUT", "/api/p/"+p.ID+"/store/object?key=journal/"+srv.Device.ID+".jsonl", strings.NewReader(forged))
	req.Header.Set("X-Bdrive-Device", srv.Device.ID)
	req.AddCookie(cookies["alice"]) // the org owner: a project admin
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("an admin wrote the hub's journal through the store: %d %s", rec.Code, rec.Body)
	}
}

// A seq with no room after it, put in the hub's journal from outside the hub,
// stops the hub's writes rather than wrapping every later op onto one slot.
func TestTheHubRefusesASeqWithNoRoomAfterIt(t *testing.T) {
	root, h, p := casHub(t)
	line, _ := journal.Marshal([]journal.Op{{
		Seq: math.MaxInt64, Lamport: 1, Time: time.Now().UTC(), Device: webDevice.ID,
		Kind: journal.KindPut, Path: "forged.md", Blob: shaOf("x"), Size: 1,
	}})
	key := p.ID + "/journal/" + webDevice.ID + ".jsonl"
	if err := root.Backend.Put(context.Background(), key, bytes.NewReader(line), int64(len(line))); err != nil {
		t.Fatal(err)
	}
	if rec := putIfMatch(h, p.ID, "a.md", "a", ""); rec.Code == 200 {
		t.Fatal("the hub stamped an op after seq MaxInt64")
	}
	for _, op := range hubOps(t, root, p.ID) {
		if op.Seq <= 0 {
			t.Fatalf("hub op %s took seq %d", op.Path, op.Seq)
		}
	}
}
