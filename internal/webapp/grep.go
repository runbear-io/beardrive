package webapp

import (
	"context"
	"encoding/json"
	"fmt"
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
		size := int64(-1)
		if ok {
			size = trustedSize(r.Context(), v, fi)
		}
		if ok && i > 0 && size >= 0 && size > grepPageBytes-bytesRead && size <= grepPageBytes {
			break
		}
		out.Scanned = i + 1
		if !ok {
			out.Missing = append(out.Missing, p)
			continue
		}
		if size < 0 || size > grepPageBytes {
			out.Unreadable = append(out.Unreadable, p)
			continue
		}
		fi.Size = size
		rc, err := v.source.Open(r.Context(), p, fi)
		if err != nil {
			out.Unreadable = append(out.Unreadable, p)
			continue
		}
		// The read itself is bounded by what is left of the page's budget
		// (the whole budget for the first file), so one huge file cannot make
		// a request read without limit. A file cut by it is reported as not
		// fully searched, never as a clean negative.
		budget := max(grepPageBytes-bytesRead, 0)
		if i == 0 {
			budget = grepPageBytes
		}
		bytesRead += min(fi.Size, budget)
		capped := false
		found, isBin, cut := scanBlob(io.LimitReader(rc, budget), re, 0, func(line int, text string, isMatch bool) bool {
			if !isMatch {
				return true
			}
			out.Total++
			if req.FilesOnly {
				return false
			}
			if len(out.Matches) >= maxMatches {
				// Stop reading at the cap. Total is one past what was
				// returned, which tells the caller there was more.
				capped = true
				return false
			}
			out.Matches = append(out.Matches, remote.GrepMatch{Path: p, Line: line, Text: text})
			return true
		})
		rc.Close()
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
func trustedSize(ctx context.Context, v *volume, fi FileInfo) int64 {
	rs, ok := v.source.(*RemoteSource)
	if !ok {
		return fi.Size
	}
	return rs.storedSize(ctx, fi.Blob)
}

func (r *RemoteSource) storedSize(ctx context.Context, sha string) int64 {
	if !blobRe.MatchString(sha) {
		return -1
	}
	if objs, err := r.Backend.List(ctx, "blobs/"+sha); err == nil {
		for _, o := range objs {
			if o.Key == "blobs/"+sha {
				return o.Size
			}
		}
	}
	mrc, err := r.Backend.Get(ctx, "manifests/"+sha)
	if err != nil {
		return -1
	}
	defer mrc.Close()
	var man struct {
		Size   int64 `json:"size"`
		Chunks []struct {
			N int64 `json:"n"`
		} `json:"chunks"`
	}
	if json.NewDecoder(io.LimitReader(mrc, 8<<20)).Decode(&man) != nil || man.Size < 0 || len(man.Chunks) > maxManifestChunks {
		return -1
	}
	var total int64
	for _, c := range man.Chunks {
		if c.N < 0 || c.N > maxReassembleChunkBytes {
			return -1
		}
		total += c.N
	}
	if total != man.Size {
		return -1
	}
	return total
}
