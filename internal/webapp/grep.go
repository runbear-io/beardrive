package webapp

import (
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
		if i > 0 && (i >= maxGrepFiles || bytesRead >= maxGrepBytes || len(out.Matches) >= maxMatches) {
			break
		}
		out.Scanned = i + 1
		fi, ok := files[p]
		if !ok {
			out.Missing = append(out.Missing, p)
			continue
		}
		bytesRead += fi.Size
		rc, err := v.source.Open(r.Context(), p, fi)
		if err != nil {
			out.Unreadable = append(out.Unreadable, p)
			continue
		}
		found, isBin, cut := scanBlob(rc, re, 0, func(line int, text string, isMatch bool) bool {
			if !isMatch {
				return true
			}
			out.Total++
			if req.FilesOnly {
				return false
			}
			if len(out.Matches) < maxMatches {
				out.Matches = append(out.Matches, remote.GrepMatch{Path: p, Line: line, Text: text})
			}
			return true
		})
		rc.Close()
		switch {
		case isBin:
			out.Binary = append(out.Binary, p)
		case cut > 0:
			out.Unreadable = append(out.Unreadable, p)
		}
		if found {
			out.Files = append(out.Files, p)
		}
	}
	writeJSON(w, out)
}
