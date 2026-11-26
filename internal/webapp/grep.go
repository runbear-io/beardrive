package webapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"net/http"
	"regexp"

	"github.com/runbear-io/beardrive/internal/remote"
)

// Hub-side grep for devices in lazy mode (POST /api/p/<id>/grep).
//
// A lazy device holds the metadata of every file but not the content of the
// large ones, so `bdrive grep` searches what it holds and asks the hub to
// search the rest. The request names the paths explicitly — the device knows
// exactly which files it lacks — and the hub scans them in order within one
// page's budget (the same caps the MCP grep uses), answering how many it
// consumed. The device resends the remainder, so a large search pages rather
// than being silently cut. Paths the caller may not read are answered as
// missing, the same way a hidden file is absent everywhere else.

// maxGrepPaths bounds one request's path list.
const maxGrepPaths = 5000

// grepPageBytes is one page's read budget (a var so tests can shrink it).
var grepPageBytes int64 = maxGrepBytes

func (s *Server) handleGrep(v *volume, w http.ResponseWriter, r *http.Request) {
	var req remote.GrepRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxRequestBody)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Pattern == "" || len(req.Paths) > maxGrepPaths {
		http.Error(w, fmt.Sprintf("need a pattern and at most %d paths", maxGrepPaths), http.StatusBadRequest)
		return
	}
	expr := req.Pattern
	if req.IgnoreCase {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		http.Error(w, "bad pattern: "+err.Error(), http.StatusBadRequest)
		return
	}
	maxMatches := req.MaxMatches
	if maxMatches <= 0 || maxMatches > 5*maxGrepMatches {
		maxMatches = maxGrepMatches
	}
	snap, err := v.snapshotForRequest(r)
	if err != nil {
		http.Error(w, "project unavailable", http.StatusServiceUnavailable)
		return
	}
	files := visibleFiles(snap.files, s.visibility(r))
	out := remote.GrepResponse{Matches: []remote.GrepMatch{}, Files: []string{}}
	var bytesRead int64
	for i, p := range req.Paths {
		// Always make progress: the first path of a page is scanned whatever
		// its size, so one huge file cannot stall a caller's paging loop.
		if i > 0 && (i >= maxGrepFiles || bytesRead >= grepPageBytes || len(out.Matches) >= maxMatches) {
			break
		}
		fi, ok := files[p]
		// Sized BEFORE opening: on a hub, opening a blob can hash or
		// reassemble the whole object, so the budget must decide whether it
		// is opened at all. The size is the STORED object's (trustedSize),
		// never the journal's declared op.Size, which a writer chooses. A
		// file that does not fit what is left of this page starts the next
		// one; a file larger than a whole page, or one whose stored size
		// cannot be established, is reported as not searched, unopened.
		size, cost := int64(-1), int64(-1)
		if ok {
			var probed int64
			size, cost, probed = trustedSize(r.Context(), v, fi)
			bytesRead += probed // reading a manifest to size a file spends the page too
		}
		if ok && i > 0 && size >= 0 && cost > grepPageBytes-bytesRead && cost <= grepPageBytes {
			break
		}
		out.Scanned = i + 1
		if !ok {
			out.Missing = append(out.Missing, p)
			continue
		}
		if size < 0 || cost > grepPageBytes {
			out.Unreadable = append(out.Unreadable, p)
			continue
		}
		fi.Size = size
		// Charged BEFORE opening: an open that fails after verifying or
		// reassembling the object cost that work too, so it spends the
		// page's budget like a successful read.
		budget := max(grepPageBytes-bytesRead, 0)
		if i == 0 {
			budget = grepPageBytes
		}
		bytesRead += min(cost, budget)
		rc, verify, err := openForGrep(r.Context(), v, p, fi, budget)
		if err != nil {
			out.Unreadable = append(out.Unreadable, p)
			continue
		}
		// The read is bounded by the same budget, so a file cut by it is
		// reported as not fully searched, never as a clean negative. This
		// file's results are held aside until verify confirms the bytes were
		// the content its sha names.
		var mine []remote.GrepMatch
		total, capped := 0, false
		room := maxMatches - len(out.Matches)
		found, isBin, cut := scanBlob(io.LimitReader(rc, budget), re, 0, func(line int, text string, isMatch bool) bool {
			if !isMatch {
				return true
			}
			total++
			if req.FilesOnly {
				return false
			}
			if len(mine) >= room {
				// Stop at the cap. Total is one past what was returned,
				// which tells the caller there was more.
				capped = true
				return false
			}
			mine = append(mine, remote.GrepMatch{Path: p, Line: line, Text: text})
			return true
		})
		ok = verify()
		rc.Close()
		if !ok {
			out.Unreadable = append(out.Unreadable, p)
			continue
		}
		out.Total += total
		out.Matches = append(out.Matches, mine...)
		switch {
		case isBin:
			out.Binary = append(out.Binary, p)
		case cut > 0 || (fi.Size > budget && !capped && !(req.FilesOnly && found)):
			out.Unreadable = append(out.Unreadable, p)
		}
		if found {
			out.Files = append(out.Files, p)
		}
	}
	writeJSON(w, out)
}

// trustedSize is the size the hub's own storage records for a file's
// content: the whole blob's stored length, or, for content kept only as
// chunks, the manifest's total — which reassembly enforces, since every chunk
// is hash-verified against its key. -1 when neither can be established. A
// plain-folder source's size comes from the filesystem and is used as is.
//
// cost is what opening it spends of a page: size, plus a floor per chunk
// fetch for chunk-only content (grepChunkFloor).
func trustedSize(ctx context.Context, v *volume, fi FileInfo) (size, cost, probed int64) {
	rs, ok := v.source.(*RemoteSource)
	if !ok {
		return fi.Size, fi.Size, 0
	}
	return rs.storedSize(ctx, fi.Blob)
}

func (r *RemoteSource) storedSize(ctx context.Context, sha string) (size, cost, probed int64) {
	if !blobRe.MatchString(sha) {
		return -1, -1, 0
	}
	// OpenBlob serves the whole blob when it verifies and falls back to
	// reassembling the manifest when it does not, so the budget must cover
	// whichever is LARGER: a small wrong whole blob must not admit a large
	// reassembly. A presigned upload cannot grow an object afterwards — the
	// signature covers Content-Length — so the listed size is the size.
	whole := int64(-1)
	man, chunks, probed := r.manifestSize(ctx, sha)
	if objs, err := r.Backend.List(ctx, "blobs/"+sha); err == nil {
		for _, o := range objs {
			if o.Key == "blobs/"+sha {
				whole = o.Size
			}
		}
	}
	size = max(whole, man)
	return size, max(whole, man+chunks*grepChunkFloor), probed
}

// manifestSize is a chunk manifest's total, which reassembly enforces since
// every chunk is hash-verified against its key; -1 with no valid manifest.
// probed is how many manifest bytes were read; Exists is asked first, so a
// file with no manifest (the common case) reads none.
func (r *RemoteSource) manifestSize(ctx context.Context, sha string) (size, chunks, probed int64) {
	if ok, err := r.Backend.Exists(ctx, "manifests/"+sha); err != nil || !ok {
		return -1, 0, 0
	}
	mrc, err := r.Backend.Get(ctx, "manifests/"+sha)
	if err != nil {
		return -1, 0, 0
	}
	defer mrc.Close()
	cr := &manifestCounter{r: io.LimitReader(mrc, 8<<20)}
	var man struct {
		Size   int64 `json:"size"`
		Chunks []struct {
			N int64 `json:"n"`
		} `json:"chunks"`
	}
	if json.NewDecoder(cr).Decode(&man) != nil || man.Size < 0 || len(man.Chunks) > maxManifestChunks {
		return -1, 0, cr.n
	}
	var total int64
	for _, c := range man.Chunks {
		if c.N < 0 || c.N > maxReassembleChunkBytes {
			return -1, 0, cr.n
		}
		total += c.N
	}
	if total != man.Size {
		return -1, 0, cr.n
	}
	return total, int64(len(man.Chunks)), cr.n
}

// grepChunkFloor is the budget a single chunk fetch costs at minimum: each
// chunk is one storage request however small, so a file of many tiny chunks
// spends a page like its request count, not only its byte count.
const grepChunkFloor = 64 << 10

type manifestCounter struct {
	r io.Reader
	n int64
}

func (c *manifestCounter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// openForGrep opens one file for the grep scan without letting the open
// itself do unbounded work. A plain-folder source is read as is. On a hub,
// a whole blob is read straight from storage — not through OpenBlob, whose
// verification hashes the object before returning however large it has
// grown since it was sized — with its sha256 computed as the scan reads, and
// verify drains the remainder (still within budget) and reports whether the
// bytes were the content the sha names; the caller keeps the file's results
// only then. Content that exists only as chunks is reassembled the same way
// (openChunks), never through OpenBlob.
func openForGrep(ctx context.Context, v *volume, p string, fi FileInfo, budget int64) (io.ReadCloser, func() bool, error) {
	rs, isRemote := v.source.(*RemoteSource)
	if !isRemote {
		rc, err := v.source.Open(ctx, p, fi)
		return rc, func() bool { return true }, err
	}
	var src io.ReadCloser
	if whole, err := rs.Backend.Get(ctx, "blobs/"+fi.Blob); err == nil {
		src = whole
	} else {
		cr, err := rs.openChunks(ctx, fi.Blob, budget)
		if err != nil {
			return nil, nil, err
		}
		src = cr
	}
	h := sha256.New()
	lr := &io.LimitedReader{R: src, N: budget + 1}
	tee := io.TeeReader(lr, h)
	verify := func() bool {
		if _, err := io.Copy(io.Discard, tee); err != nil || lr.N == 0 {
			return false // unreadable, a chunk that failed its hash, or over budget
		}
		return hex.EncodeToString(h.Sum(nil)) == fi.Blob
	}
	return struct {
		io.Reader
		io.Closer
	}{tee, src}, verify, nil
}

// openChunks streams chunk-only content in order, for grep: the manifest is
// read bounded and must total no more than budget, each chunk is fetched
// only when the scan reaches it, read up to its declared length plus one,
// and must hash to its key — a lying chunk is a read error, never content.
// The caller's sha check over the whole stream completes the verification.
func (r *RemoteSource) openChunks(ctx context.Context, sha string, budget int64) (io.ReadCloser, error) {
	mrc, err := r.Backend.Get(ctx, "manifests/"+sha)
	if err != nil {
		return nil, err
	}
	var man struct {
		Size   int64 `json:"size"`
		Chunks []struct {
			H string `json:"h"`
			N int64  `json:"n"`
		} `json:"chunks"`
	}
	derr := json.NewDecoder(io.LimitReader(mrc, 8<<20)).Decode(&man)
	mrc.Close()
	if derr != nil || man.Size < 0 || man.Size > budget || len(man.Chunks) > maxManifestChunks {
		return nil, fmt.Errorf("manifest for %s unusable within budget", sha)
	}
	var total int64
	for _, c := range man.Chunks {
		if c.N < 0 || c.N > maxReassembleChunkBytes || !blobKeyRe.MatchString("blobs/"+c.H) {
			return nil, fmt.Errorf("manifest for %s names an invalid chunk", sha)
		}
		total += c.N
	}
	if total != man.Size {
		return nil, fmt.Errorf("manifest for %s has inconsistent total size", sha)
	}
	cs := &chunkStream{ctx: ctx, be: r.Backend}
	for _, c := range man.Chunks {
		cs.keys = append(cs.keys, c.H)
		cs.sizes = append(cs.sizes, c.N)
	}
	return cs, nil
}

type chunkStream struct {
	ctx   context.Context
	be    remote.Backend
	keys  []string
	sizes []int64
	cur   io.ReadCloser
	lr    *io.LimitedReader
	h     hash.Hash
	want  string
	n     int64
}

func (c *chunkStream) Read(p []byte) (int, error) {
	for {
		if c.cur == nil {
			if len(c.keys) == 0 {
				return 0, io.EOF
			}
			rc, err := c.be.Get(c.ctx, "chunks/"+c.keys[0])
			if err != nil {
				return 0, err
			}
			c.cur, c.want, c.n, c.h = rc, c.keys[0], c.sizes[0], sha256.New()
			c.lr = &io.LimitedReader{R: rc, N: c.n + 1}
			c.keys, c.sizes = c.keys[1:], c.sizes[1:]
		}
		n, err := c.lr.Read(p)
		c.h.Write(p[:n])
		if err == io.EOF {
			read := c.n + 1 - c.lr.N
			c.cur.Close()
			c.cur = nil
			if read != c.n || hex.EncodeToString(c.h.Sum(nil)) != c.want {
				return n, fmt.Errorf("chunk %s does not hash to its key", c.want)
			}
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
}

func (c *chunkStream) Close() error {
	if c.cur != nil {
		return c.cur.Close()
	}
	return nil
}
