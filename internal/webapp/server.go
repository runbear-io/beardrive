// Package webapp serves the bdrive web server: a browsable web view of
// synced files (file tree reconstructed from the journals, rendered
// markdown, downloads), browser uploads, and — in hub mode — the sync API
// that lets storage-blind client devices sync whole projects through this
// server.
//
// Two modes:
//
//   - single-volume: Source is set (a DirSource for a plain folder, or a
//     RemoteSource in tests); the classic viewer.
//   - hub: Root + Projects are set; the server hosts many projects, each a
//     volume stored under <root>/<project-id>/ in the object store, managed
//     by a file-backed project registry.
//
// The client — browser or syncing device — is deliberately told nothing
// about the storage: no remote URL, bucket, or credentials ever appear in an
// API response.
package webapp

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"mime"
	"net/http"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
)

//go:embed static
var staticFiles embed.FS

// Source supplies the file set and content of one volume. Implementations:
// RemoteSource (a beardrive remote) and DirSource (a plain local folder).
type Source interface {
	Files(ctx context.Context) (map[string]FileInfo, error)
	Open(ctx context.Context, path string, fi FileInfo) (io.ReadCloser, error)
}

// Server renders volumes as a website and, in hub mode, brokers sync for
// client devices.
type Server struct {
	// Single-volume mode: serve exactly this source.
	Source Source
	Volume string // display only

	// Hub mode (when Root is set): many projects on one storage root.
	Root     remote.Backend
	Projects *ProjectDB

	// Device identifies this server in ops it journals for browser uploads.
	Device  Identity
	Refresh time.Duration
	Upload  UploadConfig
	// Auth, when set, gates the whole API behind sign-in. Nil means the
	// historical trusted-network behavior: no accounts, everyone welcome.
	Auth AuthProvider
	// Devices, when set, records what the server observes about syncing
	// devices (name, OS, public IP, last activity) for history.
	Devices *DeviceRegistry
	// Shares, when set, enables public share links (/s/<token>).
	Shares *ShareDB

	volOnce sync.Once
	vol     *volume

	volsMu sync.Mutex
	vols   map[string]*volume // hub mode: per-project, keyed by project id
}

// UploadConfig controls whether and how clients may write.
type UploadConfig struct {
	Enabled bool
	// TTL bounds the lifetime of presigned direct-upload URLs.
	TTL time.Duration
}

// DefaultUploadTTL is used when UploadConfig.TTL is unset: long enough for a
// slow upload, short enough that a leaked URL goes stale quickly.
const DefaultUploadTTL = 15 * time.Minute

func (c UploadConfig) ttl() time.Duration {
	if c.TTL > 0 {
		return c.TTL
	}
	return DefaultUploadTTL
}

// FileInfo is the resolved state of one path: content identity (Blob doubles
// as the ETag), plus provenance where the source knows it.
type FileInfo struct {
	Blob   string
	Size   int64
	Time   time.Time
	Author string
	Device string
}

// volume is one browsable/syncable file set: a source plus its snapshot
// cache. File listings are cached for refresh between fetches; if the source
// becomes unreachable, the last good snapshot keeps being served.
type volume struct {
	source  Source
	refresh time.Duration

	mu   sync.Mutex
	snap *snapshot
	at   time.Time
}

type snapshot struct {
	files map[string]FileInfo
}

func (v *volume) snapshot(ctx context.Context) (*snapshot, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.snap != nil && time.Since(v.at) < v.refresh {
		return v.snap, nil
	}
	files, err := v.source.Files(ctx)
	if err != nil {
		if v.snap != nil {
			return v.snap, nil // serve stale rather than fail
		}
		return nil, err
	}
	v.snap, v.at = &snapshot{files: files}, time.Now()
	return v.snap, nil
}

// invalidate forces the next snapshot to refetch, so an upload shows up in
// the tree immediately instead of after refresh.
func (v *volume) invalidate() {
	v.mu.Lock()
	v.at = time.Time{}
	v.mu.Unlock()
}

func (v *volume) uploader() Uploader {
	u, _ := v.source.(Uploader)
	return u
}

// single returns the single-volume mode volume.
func (s *Server) single() *volume {
	s.volOnce.Do(func() {
		s.vol = &volume{source: s.Source, refresh: s.Refresh}
	})
	return s.vol
}

// projectVolume resolves a project id to its volume, creating the (cached)
// source over the project's storage prefix on first use.
func (s *Server) projectVolume(id string) (*volume, error) {
	if s.Root == nil || s.Projects == nil {
		return nil, fmt.Errorf("this server does not host projects")
	}
	if !projectIDRe.MatchString(id) {
		return nil, fmt.Errorf("invalid project id %q", id)
	}
	if _, ok := s.Projects.Get(id); !ok {
		return nil, fmt.Errorf("no such project %q", id)
	}
	s.volsMu.Lock()
	defer s.volsMu.Unlock()
	if s.vols == nil {
		s.vols = make(map[string]*volume)
	}
	v, ok := s.vols[id]
	if !ok {
		v = &volume{
			source:  &RemoteSource{Backend: remote.Prefixed(s.Root, id), Device: s.Device},
			refresh: s.Refresh,
		}
		s.vols[id] = v
	}
	return v, nil
}

// RemoteSource reads a beardrive remote: it fetches every journal and folds the
// ops into the current volume state (same total order as journal.Replay,
// but keeping author/device/time of the winning op per path). With Device set
// it also accepts uploads, journaled under that identity.
type RemoteSource struct {
	Backend remote.Backend
	// Device identifies this server in ops it journals for uploads. Required
	// for uploads; irrelevant for reading.
	Device Identity

	upmu sync.Mutex // serializes read-modify-write of our own journal
}

// Identity is the device identity uploads are journaled under.
type Identity struct {
	ID, Name, Author string
}

// loadOps fetches and parses every journal on the remote.
func (r *RemoteSource) loadOps(ctx context.Context) ([]journal.Op, error) {
	objs, err := r.Backend.List(ctx, "journal/")
	if err != nil {
		return nil, fmt.Errorf("list journals: %w", err)
	}
	var all []journal.Op
	for _, o := range objs {
		if !strings.HasSuffix(o.Key, ".jsonl") {
			continue
		}
		rc, err := r.Backend.Get(ctx, o.Key)
		if err != nil {
			return nil, fmt.Errorf("fetch %s: %w", o.Key, err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, err
		}
		ops, err := journal.Parse(data)
		if err != nil {
			continue // corrupt journal; ignore rather than break the view
		}
		all = append(all, ops...)
	}
	return all, nil
}

func (r *RemoteSource) Files(ctx context.Context) (map[string]FileInfo, error) {
	all, err := r.loadOps(ctx)
	if err != nil {
		return nil, err
	}
	journal.Sort(all)
	files := make(map[string]FileInfo)
	for _, op := range all {
		switch op.Kind {
		case journal.KindPut:
			files[op.Path] = FileInfo{
				Blob: op.Blob, Size: op.Size, Time: op.Time,
				Author: op.Author, Device: op.DeviceName,
			}
		case journal.KindDelete:
			delete(files, op.Path)
		}
	}
	return files, nil
}

func (r *RemoteSource) Open(ctx context.Context, _ string, fi FileInfo) (io.ReadCloser, error) {
	return r.Backend.Get(ctx, "blobs/"+fi.Blob)
}

// Handler returns the HTTP handler: /api/* plus the embedded frontend.
func (s *Server) Handler() http.Handler {
	static, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic(err) // embedded FS; cannot fail at runtime
	}
	mux := http.NewServeMux()

	// Volume resolution per route family: fixed single volume, or by
	// project id in hub mode. One handler implementation serves both.
	single := func(h func(*volume, http.ResponseWriter, *http.Request)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if s.Source == nil {
				http.Error(w, "this server hosts projects; use /api/p/<project-id>/...", http.StatusNotFound)
				return
			}
			h(s.single(), w, r)
		}
	}
	proj := func(h func(*volume, http.ResponseWriter, *http.Request)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			v, err := s.projectVolume(r.PathValue("project"))
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			h(v, w, r)
		}
	}

	mux.HandleFunc("GET /api/config", s.handleConfig)
	mux.HandleFunc("GET /api/projects", s.handleProjectList)
	mux.HandleFunc("POST /api/projects", s.handleProjectCreate)
	mux.HandleFunc("GET /api/projects/{project}", s.handleProjectGet)

	for prefix, resolve := range map[string]func(func(*volume, http.ResponseWriter, *http.Request)) http.HandlerFunc{
		"/api/":             single,
		"/api/p/{project}/": proj,
	} {
		mux.HandleFunc("GET "+prefix+"tree", resolve(s.handleTree))
		mux.HandleFunc("GET "+prefix+"file", resolve(s.handleFile))
		mux.HandleFunc("GET "+prefix+"download", resolve(s.handleDownload))
		mux.HandleFunc("GET "+prefix+"render", resolve(s.handleRender))
		mux.HandleFunc("POST "+prefix+"upload/init", resolve(s.handleUploadInit))
		mux.HandleFunc("PUT "+prefix+"upload/content", resolve(s.handleUploadContent))
		mux.HandleFunc("POST "+prefix+"upload/commit", resolve(s.handleUploadCommit))
	}

	mux.HandleFunc("GET /api/p/{project}/history", proj(s.handleHistory))
	mux.HandleFunc("GET /api/p/{project}/blob", proj(s.handleBlob))
	mux.HandleFunc("POST /api/p/{project}/shares", proj(s.handleShareCreate))
	mux.HandleFunc("GET /api/p/{project}/shares", proj(s.handleShareList))
	mux.HandleFunc("DELETE /api/shares/{token}", s.handleShareRevoke)
	mux.HandleFunc("GET /s/{token}", s.handleShared)

	// The sync (store) API only exists per project: hub mode is what
	// storage-blind devices sync through.
	mux.HandleFunc("GET /api/p/{project}/store/list", proj(s.handleStoreList))
	mux.HandleFunc("GET /api/p/{project}/store/object", proj(s.handleStoreGet))
	mux.HandleFunc("GET /api/p/{project}/store/exists", proj(s.handleStoreExists))
	mux.HandleFunc("POST /api/p/{project}/store/sign", proj(s.handleStoreSign))
	mux.HandleFunc("PUT /api/p/{project}/store/object", proj(s.handleStorePut))

	mux.Handle("GET /", http.FileServerFS(static))
	if s.Auth != nil {
		s.Auth.Register(mux)
	}
	return s.authGate(mux)
}

// handleConfig tells the client how this server is configured. Deliberately
// nothing about the storage backend.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	mode := "volume"
	if s.Root != nil {
		mode = "hub"
	}
	auth := map[string]any{"enabled": s.Auth != nil}
	if s.Auth != nil {
		auth["cli_login"] = s.Auth.CLILoginPath()
	}
	writeJSON(w, map[string]any{
		"mode":   mode,
		"volume": s.Volume,
		"upload": map[string]any{
			"enabled": s.Upload.Enabled,
		},
		"auth": auth,
	})
}

func (s *Server) handleProjectList(w http.ResponseWriter, r *http.Request) {
	if s.Projects == nil {
		http.Error(w, "this server does not host projects", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{"projects": s.Projects.List()})
}

func (s *Server) handleProjectGet(w http.ResponseWriter, r *http.Request) {
	if s.Projects == nil {
		http.Error(w, "this server does not host projects", http.StatusNotFound)
		return
	}
	p, ok := s.Projects.Get(r.PathValue("project"))
	if !ok {
		http.Error(w, "no such project", http.StatusNotFound)
		return
	}
	writeJSON(w, p)
}

// handleProjectCreate creates a project by name, or returns the existing one
// with that name (create-or-join). Creating is a write, so it follows the
// upload setting.
func (s *Server) handleProjectCreate(w http.ResponseWriter, r *http.Request) {
	if s.Projects == nil {
		http.Error(w, "this server does not host projects", http.StatusNotFound)
		return
	}
	if !s.Upload.Enabled {
		http.Error(w, "this server is read-only; projects cannot be created", http.StatusForbidden)
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	p, created, err := s.Projects.GetOrCreate(req.Name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"project": p, "created": created})
}

// Node is one entry of the file tree returned by the tree endpoint.
type Node struct {
	Name     string    `json:"name"`
	Path     string    `json:"path"`
	Dir      bool      `json:"dir"`
	Size     int64     `json:"size,omitempty"`
	Time     time.Time `json:"time,omitzero"`
	Author   string    `json:"author,omitempty"`
	Device   string    `json:"device,omitempty"`
	Children []*Node   `json:"children,omitempty"`
}

func (s *Server) handleTree(v *volume, w http.ResponseWriter, r *http.Request) {
	snap, err := v.snapshot(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, buildTree(snap.files))
}

func buildTree(files map[string]FileInfo) *Node {
	root := &Node{Name: "/", Dir: true}
	dirs := map[string]*Node{"": root}
	for _, p := range slices.Sorted(maps.Keys(files)) {
		fi := files[p]
		parent := root
		segs := strings.Split(p, "/")
		for i := 0; i < len(segs)-1; i++ {
			dp := strings.Join(segs[:i+1], "/")
			n, ok := dirs[dp]
			if !ok {
				n = &Node{Name: segs[i], Path: dp, Dir: true}
				dirs[dp] = n
				parent.Children = append(parent.Children, n)
			}
			parent = n
		}
		parent.Children = append(parent.Children, &Node{
			Name: segs[len(segs)-1], Path: p,
			Size: fi.Size, Time: fi.Time, Author: fi.Author, Device: fi.Device,
		})
	}
	sortTree(root)
	return root
}

func sortTree(n *Node) {
	sort.SliceStable(n.Children, func(i, j int) bool {
		a, b := n.Children[i], n.Children[j]
		if a.Dir != b.Dir {
			return a.Dir // folders first, like Obsidian
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	for _, c := range n.Children {
		if c.Dir {
			sortTree(c)
		}
	}
}

// lookup resolves ?path= against the volume's current snapshot.
func lookup(v *volume, r *http.Request) (string, FileInfo, int, error) {
	p := r.URL.Query().Get("path")
	if p == "" {
		return "", FileInfo{}, http.StatusBadRequest, fmt.Errorf("missing ?path=")
	}
	snap, err := v.snapshot(r.Context())
	if err != nil {
		return "", FileInfo{}, http.StatusBadGateway, err
	}
	fi, ok := snap.files[p]
	if !ok {
		return "", FileInfo{}, http.StatusNotFound, fmt.Errorf("no such file: %s", p)
	}
	return p, fi, 0, nil
}

func serveBlob(v *volume, w http.ResponseWriter, r *http.Request, attach bool) {
	p, fi, code, err := lookup(v, r)
	if err != nil {
		http.Error(w, err.Error(), code)
		return
	}
	etag := `"` + fi.Blob + `"`
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	rc, err := v.source.Open(r.Context(), p, fi)
	if err != nil {
		http.Error(w, fmt.Sprintf("fetch content: %v", err), http.StatusBadGateway)
		return
	}
	defer rc.Close()
	w.Header().Set("ETag", etag)
	w.Header().Set("Content-Type", contentType(p))
	w.Header().Set("Content-Length", fmt.Sprint(fi.Size))
	if attach {
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", path.Base(p)))
	}
	io.Copy(w, rc)
}

func (s *Server) handleFile(v *volume, w http.ResponseWriter, r *http.Request) {
	serveBlob(v, w, r, false)
}

func (s *Server) handleDownload(v *volume, w http.ResponseWriter, r *http.Request) {
	serveBlob(v, w, r, true)
}

func (s *Server) handleRender(v *volume, w http.ResponseWriter, r *http.Request) {
	p, fi, code, err := lookup(v, r)
	if err != nil {
		http.Error(w, err.Error(), code)
		return
	}
	rc, err := v.source.Open(r.Context(), p, fi)
	if err != nil {
		http.Error(w, fmt.Sprintf("fetch content: %v", err), http.StatusBadGateway)
		return
	}
	src, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	html, err := RenderMarkdown(src)
	if err != nil {
		http.Error(w, fmt.Sprintf("render: %v", err), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"path": p, "html": html,
		"size": fi.Size, "time": fi.Time, "author": fi.Author, "device": fi.Device,
	})
}

func contentType(p string) string {
	switch strings.ToLower(path.Ext(p)) {
	case ".md", ".markdown":
		return "text/markdown; charset=utf-8"
	case ".txt", ".log", ".go", ".py", ".js", ".ts", ".sh", ".yaml", ".yml", ".toml", ".csv":
		return "text/plain; charset=utf-8"
	case ".json":
		return "application/json"
	}
	if t := mime.TypeByExtension(path.Ext(p)); t != "" {
		return t
	}
	return "application/octet-stream"
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
