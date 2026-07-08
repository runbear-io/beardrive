package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
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
	hc      *http.Client
}

var projectPathRe = regexp.MustCompile(`^/p/(p-[0-9a-f]{8})/?$`)

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
	return &httpBackend{base: base, project: m[1], hc: &http.Client{Timeout: 5 * time.Minute}}, nil
}

func (b *httpBackend) endpoint(name string, q url.Values) string {
	s := b.base + "/api/p/" + b.project + "/store/" + name
	if len(q) > 0 {
		s += "?" + q.Encode()
	}
	return s
}

// httpError turns a non-2xx response into an error carrying the server's
// message.
func httpError(resp *http.Response) error {
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return fmt.Errorf("server: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
}

func (b *httpBackend) List(ctx context.Context, prefix string) ([]Object, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		b.endpoint("list", url.Values{"prefix": {prefix}}), nil)
	if err != nil {
		return nil, err
	}
	resp, err := b.hc.Do(req)
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
	return out.Objects, nil
}

func (b *httpBackend) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		b.endpoint("object", url.Values{"key": {key}}), nil)
	if err != nil {
		return nil, err
	}
	resp, err := b.hc.Do(req)
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
	resp, err := b.hc.Do(req)
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
	resp, err := b.hc.Do(req)
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
		return fmt.Errorf("direct upload: %w", httpError(resp))
	}
	return nil
}

func (b *httpBackend) putViaServer(ctx context.Context, key string, r io.Reader, size int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		b.endpoint("object", url.Values{"key": {key}}), r)
	if err != nil {
		return err
	}
	req.ContentLength = size
	resp, err := b.hc.Do(req)
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
