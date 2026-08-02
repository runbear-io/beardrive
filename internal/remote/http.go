package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/runbear-io/beardrive/internal/config"
)

// httpBackend syncs one project through a bdrive web server instead of
// talking to an object store. The client device is storage-blind: it only
// knows the server URL and a project id (https://host:4173/p/<project-id>,
// written by `bdrive init`); the storage location and credentials live on
// the server. Blob uploads go directly to the object store through
// short-lived presigned URLs when the server can mint them, and are relayed
// through the server otherwise.
//
// The server exposes the project's store under /api/p/<id>/store/* (list,
// object, exists, sign). Key layout and semantics are identical to any other
// backend, so the whole sync machinery works unchanged.
type httpBackend struct {
	base    string // scheme://host[:port]
	project string
	token   string // device token from `bdrive login`; empty on open servers
	device  config.Device
	hc      *http.Client
}

// The id is whatever the hub minted (a UUID today, `p-xxxxxxxx` on older
// hubs), so this only checks the shape of a URL segment — the hub is the
// authority on which ids exist.
var projectPathRe = regexp.MustCompile(`^/p/([A-Za-z0-9._-]{4,64})/?$`)

func newHTTPBackend(raw string) (*httpBackend, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("server remote needs a URL like https://host:4173/p/<project-id>, got %q", raw)
	}
	m := projectPathRe.FindStringSubmatch(u.Path)
	if m == nil {
		return nil, fmt.Errorf("server remote %q has no project (want https://host:4173/p/<project-id>; run `bdrive init`)", raw)
	}
	base := (&url.URL{Scheme: u.Scheme, Host: u.Host}).String()
	dev, _ := config.LoadDevice()
	hc := &http.Client{Timeout: 5 * time.Minute, CheckRedirect: refuseOffOriginRedirect}
	return &httpBackend{base: base, project: m[1], token: deviceToken(base), device: dev, hc: hc}, nil
}

// deviceToken finds this device's credential for the server at base:
// BDRIVE_TOKEN wins (tests, CI), otherwise the token `bdrive login` stored in
// settings — but only for the server it was issued for.
//
// The remote URL comes from a folder's .bdrive/config.json, which travels with
// the folder: without the origin check, a folder someone shares with you
// chooses where your hub credential is sent, plaintext http included. The same
// binding covers `bdrive login <other-hub>`, after which every old mount would
// otherwise ship the new hub's token to the old host.
func deviceToken(base string) string {
	if t := os.Getenv("BDRIVE_TOKEN"); t != "" {
		return t
	}
	s, err := config.LoadSettings()
	if err != nil || s.Token == "" || !sameOrigin(base, s.Server) {
		return ""
	}
	return s.Token
}

// sameOrigin compares scheme+host, the only thing that decides who receives a
// bearer token. A bare host in settings is read as https, which is what
// `bdrive login` writes it as.
//
// The comparison is on the ORIGIN, not on the URL's spelling: the scheme's
// default port, the case of the host and an FQDN's trailing dot all name the
// same server. Comparing url.Host verbatim made "https://hub:443" a different
// server from "https://hub", so the token was silently dropped and every sync
// 401'd forever — and `bdrive login` could not fix it, because it writes the
// same string back. Fail-closed is still the direction: anything that does not
// parse, or has no host, matches nothing.
func sameOrigin(a, b string) bool {
	x, y := originOf(a), originOf(b)
	return x != "" && x == y
}

func originOf(raw string) string {
	if raw != "" && !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return ""
	}
	if strings.Contains(host, ":") { // IPv6 literal
		host = "[" + host + "]"
	}
	port := u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		host += ":" + port
	}
	return scheme + "://" + host
}

// refuseOffOriginRedirect stops a hub's 3xx from taking this device
// anywhere but the hub itself.
func refuseOffOriginRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return fmt.Errorf("stopped after 10 redirects")
	}
	if len(via) > 0 && !sameOrigin(req.URL.String(), via[0].URL.String()) {
		// Refused, not followed with a smaller payload. Every endpoint this
		// backend calls is the hub's own store API, where a 3xx is not part of
		// the contract — and following one handed a third-party host this
		// device's id, machine name and OS (and, before round 4, its token:
		// net/http only strips Authorization when the HOSTNAME changes, so a
		// port change, an https->http downgrade or a sibling subdomain kept
		// it).
		return fmt.Errorf("refusing a redirect off %s to %s", via[0].URL.Host, req.URL.Host)
	}
	return nil
}

// do sends the request with this device's credential attached, plus the
// identity headers the server's device registry records for history (name,
// OS; the server observes the IP itself).
func (b *httpBackend) do(req *http.Request) (*http.Response, error) {
	if b.token != "" {
		req.Header.Set("Authorization", "Bearer "+b.token)
	}
	if b.device.ID != "" {
		// A journal request already named its device (nameJournalDevice); the
		// name and OS describe this machine either way.
		if req.Header.Get("X-Bdrive-Device") == "" {
			req.Header.Set("X-Bdrive-Device", b.device.ID)
		}
		req.Header.Set("X-Bdrive-Device-Name", b.device.Name)
		req.Header.Set("X-Bdrive-Os", runtime.GOOS+"/"+runtime.GOARCH)
	}
	return b.hc.Do(req)
}

var journalKeyRe = regexp.MustCompile(`^journal/([A-Za-z0-9._-]+)\.jsonl$`)

// nameJournalDevice tells the hub which device a journal request is about.
// The hub holds one request to one device's journal — the one-writer
// invariant it can't otherwise check — and a session's device is not
// necessarily this process's identity file (the sync engine only ever writes
// its own journal, so the key is the authority here).
func nameJournalDevice(req *http.Request, key string) {
	if m := journalKeyRe.FindStringSubmatch(key); m != nil {
		req.Header.Set("X-Bdrive-Device", m[1])
	}
}

func (b *httpBackend) endpoint(name string, q url.Values) string {
	s := b.base + "/api/p/" + b.project + "/store/" + name
	if len(q) > 0 {
		s += "?" + q.Encode()
	}
	return s
}

// httpError turns a non-2xx response into an error carrying the server's
// message. A 403 additionally wraps ErrForbidden: only the hub's own
// endpoints go through here, so that status is always an authorization
// answer, never a storage hiccup.
func httpError(resp *http.Response) error {
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	err := fmt.Errorf("server: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	if resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("%w: %w", ErrForbidden, err)
	}
	return err
}

func (b *httpBackend) List(ctx context.Context, prefix string) ([]Object, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		b.endpoint("list", url.Values{"prefix": {prefix}}), nil)
	if err != nil {
		return nil, err
	}
	resp, err := b.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, httpError(resp)
	}
	var out struct {
		Objects []Object `json:"objects"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	// The hub names its own objects, and the device believes it: these keys
	// become local journal file names (syncer.pull) and tar member names
	// (`bdrive export`). Nothing downstream re-checks the shape, so a hostile
	// or compromised hub would be choosing paths on the victim's disk. Keys
	// that are not keys are dropped rather than fatal — one bad listing must
	// not stop the rest of the project syncing.
	kept := out.Objects[:0]
	for _, o := range out.Objects {
		if o.Key == "" || strings.HasPrefix(o.Key, "/") || o.Key != path.Clean(o.Key) ||
			o.Key == ".." || strings.HasPrefix(o.Key, "../") {
			continue
		}
		kept = append(kept, o)
	}
	return kept, nil
}

func (b *httpBackend) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		b.endpoint("object", url.Values{"key": {key}}), nil)
	if err != nil {
		return nil, err
	}
	resp, err := b.do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, httpError(resp)
	}
	return resp.Body, nil
}

func (b *httpBackend) Exists(ctx context.Context, key string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		b.endpoint("exists", url.Values{"key": {key}}), nil)
	if err != nil {
		return false, err
	}
	resp, err := b.do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, httpError(resp)
	}
	var out struct {
		Exists bool `json:"exists"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, err
	}
	return out.Exists, nil
}

// Put asks the server how to upload this key first: "direct" carries a
// presigned URL and the bytes bypass the server entirely; "server" relays
// them through it. The reader is only consumed once the destination is known.
func (b *httpBackend) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	plan, err := b.sign(ctx, key, size)
	if err != nil {
		return err
	}
	if plan.Mode == "direct" {
		if plan.Exists {
			return nil // content-addressed and already there
		}
		return b.putDirect(ctx, plan, r, size)
	}
	return b.putViaServer(ctx, key, r, size)
}

type putPlan struct {
	Mode    string            `json:"mode"`
	Exists  bool              `json:"exists"`
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers"`
}

func (b *httpBackend) sign(ctx context.Context, key string, size int64) (putPlan, error) {
	var plan putPlan
	body, err := json.Marshal(map[string]any{"key": key, "size": size})
	if err != nil {
		return plan, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.endpoint("sign", nil), bytes.NewReader(body))
	if err != nil {
		return plan, err
	}
	req.Header.Set("Content-Type", "application/json")
	nameJournalDevice(req, key)
	resp, err := b.do(req)
	if err != nil {
		return plan, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return plan, httpError(resp)
	}
	err = json.NewDecoder(resp.Body).Decode(&plan)
	return plan, err
}

func (b *httpBackend) putDirect(ctx context.Context, plan putPlan, r io.Reader, size int64) error {
	method := plan.Method
	if method == "" {
		method = http.MethodPut
	}
	req, err := http.NewRequestWithContext(ctx, method, plan.URL, r)
	if err != nil {
		return err
	}
	req.ContentLength = size
	for k, v := range plan.Headers {
		req.Header.Set(k, v)
	}
	resp, err := b.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Deliberately not httpError: this response comes from the object
		// store, not the hub, and its 403 means an expired presigned URL —
		// mapping it to ErrForbidden would park the device in permanent
		// read-only over a transient signing problem.
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("direct upload: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

func (b *httpBackend) putViaServer(ctx context.Context, key string, r io.Reader, size int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		b.endpoint("object", url.Values{"key": {key}}), r)
	if err != nil {
		return err
	}
	nameJournalDevice(req, key)
	req.ContentLength = size
	resp, err := b.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return httpError(resp)
	}
	return nil
}

// ReportReads sends the device's queued agent reads to the hub's read
// ledger, where they count as agent traffic (actor = this device).
func (b *httpBackend) ReportReads(ctx context.Context, reads []ReadEvent) error {
	body, err := json.Marshal(map[string]any{"reads": reads})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		b.base+"/api/p/"+b.project+"/reads", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return httpError(resp)
	}
	return nil
}

func (b *httpBackend) Close() error { return nil }
