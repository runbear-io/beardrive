// Package webapp serves a read-only web view of a beardrive remote: the volume's
// file tree reconstructed from the journals, rendered markdown, and file
// downloads. It talks straight to the object store — no local volume state,
// mount, or daemon is needed.
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

// Source supplies the file set and content the server renders. Two
// implementations: RemoteSource (a beardrive remote, the normal mode) and
// DirSource (a plain local folder, for debugging without any remote).
type Source interface {
	Files(ctx context.Context) (map[string]FileInfo, error)
	Open(ctx context.Context, path string, fi FileInfo) (io.ReadCloser, error)
}

// Server renders one source as a website. File listings are cached for
// Refresh between fetches; if the source becomes unreachable, the last good
// snapshot keeps being served.
type Server struct {
	Source  Source
	Remote  string // display only
	Volume  string // display only
	Refresh time.Duration

	mu   sync.Mutex
	snap *snapshot
	at   time.Time
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

type snapshot struct {
	files map[string]FileInfo
}

func (s *Server) snapshot(ctx context.Context) (*snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snap != nil && time.Since(s.at) < s.Refresh {
		return s.snap, nil
	}
	files, err := s.Source.Files(ctx)
	if err != nil {
		if s.snap != nil {
			return s.snap, nil // serve stale rather than fail
		}
		return nil, err
	}
	s.snap, s.at = &snapshot{files: files}, time.Now()
	return s.snap, nil
}

// RemoteSource reads a beardrive remote: it fetches every journal and folds the
// ops into the current volume state (same total order as journal.Replay,
// but keeping author/device/time of the winning op per path).
type RemoteSource struct {
	Backend remote.Backend
}

func (r *RemoteSource) Files(ctx context.Context) (map[string]FileInfo, error) {
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
	mux.HandleFunc("GET /api/volume", s.handleVolume)
	mux.HandleFunc("GET /api/tree", s.handleTree)
	mux.HandleFunc("GET /api/file", s.handleFile)
	mux.HandleFunc("GET /api/download", s.handleDownload)
	mux.HandleFunc("GET /api/render", s.handleRender)
	mux.Handle("GET /", http.FileServerFS(static))
	return mux
}

func (s *Server) handleVolume(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"volume": s.Volume, "remote": s.Remote})
}

// Node is one entry of the file tree returned by /api/tree.
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

func (s *Server) handleTree(w http.ResponseWriter, r *http.Request) {
	snap, err := s.snapshot(r.Context())
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

// lookup resolves ?path= against the current snapshot.
func (s *Server) lookup(r *http.Request) (string, FileInfo, int, error) {
	p := r.URL.Query().Get("path")
	if p == "" {
		return "", FileInfo{}, http.StatusBadRequest, fmt.Errorf("missing ?path=")
	}
	snap, err := s.snapshot(r.Context())
	if err != nil {
		return "", FileInfo{}, http.StatusBadGateway, err
	}
	fi, ok := snap.files[p]
	if !ok {
		return "", FileInfo{}, http.StatusNotFound, fmt.Errorf("no such file: %s", p)
	}
	return p, fi, 0, nil
}

func (s *Server) serveBlob(w http.ResponseWriter, r *http.Request, attach bool) {
	p, fi, code, err := s.lookup(r)
	if err != nil {
		http.Error(w, err.Error(), code)
		return
	}
	etag := `"` + fi.Blob + `"`
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	rc, err := s.Source.Open(r.Context(), p, fi)
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

func (s *Server) handleFile(w http.ResponseWriter, r *http.Request) {
	s.serveBlob(w, r, false)
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	s.serveBlob(w, r, true)
}

func (s *Server) handleRender(w http.ResponseWriter, r *http.Request) {
	p, fi, code, err := s.lookup(r)
	if err != nil {
		http.Error(w, err.Error(), code)
		return
	}
	rc, err := s.Source.Open(r.Context(), p, fi)
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
