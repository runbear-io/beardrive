package webapp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	"gopkg.in/yaml.v3"

	"github.com/runbear-io/beardrive/internal/journal"
)

// ---- the applier ----
//
// The index follows the fold, never the op stream: an offline device's late
// op with an older lamport, a joining device's adopted copies, a put whose
// blob is not a sha — the fold already decided all of those, and the applier
// takes its answer. It runs after a device push has been folded in, inside the
// request of any write through the hub (so the writer reads its own write),
// and once after boot. Each pass diffs the whole fold against the index, so a
// pass that was missed is made up by the next one.

// dataProject is one project's applier state. mu admits one pass at a time;
// queued says a background pass is already waiting for it.
type dataProject struct {
	mu     sync.Mutex
	known  map[string]dataKnown // what the index holds; nil until loaded
	qmu    sync.Mutex
	queued bool
}

func (s *Server) dataProjectState(id string) *dataProject {
	v, _ := s.dataState.LoadOrStore(id, &dataProject{})
	return v.(*dataProject)
}

// dataApplyAsync runs a pass in the background. Triggers that arrive while one
// is waiting are already covered by it: it reads the fold when it starts.
func (s *Server) dataApplyAsync(id string) {
	if s.Data == nil {
		return
	}
	st := s.dataProjectState(id)
	st.qmu.Lock()
	if st.queued {
		st.qmu.Unlock()
		return
	}
	st.queued = true
	st.qmu.Unlock()
	go func() {
		st.mu.Lock()
		defer st.mu.Unlock()
		st.qmu.Lock()
		st.queued = false
		st.qmu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if err := s.dataApplyLocked(ctx, id, st); err != nil {
			log.Printf("beardrive: data index for %s: %v (retried on the next change)", id, err)
		}
	}()
}

// dataWrote brings the index up to a write the hub has just made, before the
// request that made it returns. A failure is logged, never returned: the write
// itself succeeded, and the next pass catches the index up.
func (s *Server) dataWrote(ctx context.Context, id string) {
	if s.Data == nil || id == "" {
		return
	}
	st := s.dataProjectState(id)
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := s.dataApplyLocked(ctx, id, st); err != nil {
		log.Printf("beardrive: data index for %s: %v (retried on the next change)", id, err)
	}
}

// dataBoot queues a pass for every project with data on, once per process:
// a write this process never saw happened while it was down.
func (s *Server) dataBoot() {
	if s.Data == nil || s.Projects == nil {
		return
	}
	for _, p := range s.Projects.List() {
		if p.Data {
			s.dataApplyAsync(p.ID)
		}
	}
}

// isDataFile says whether a path is indexed: JSON, or Markdown (for its
// frontmatter), and not a link.
func isDataFile(path string, fi FileInfo) bool {
	if fi.Link != "" || !isBlobSHA(fi.Blob) {
		return false
	}
	ext := strings.ToLower(path[strings.LastIndexByte(path, '.')+1:])
	return ext == "json" || ext == "md"
}

func (s *Server) dataApplyLocked(ctx context.Context, id string, st *dataProject) error {
	p, ok := s.Projects.Get(id)
	if !ok || !p.Data {
		return nil
	}
	_, v, err := s.projectVolume(id)
	if err != nil {
		return err
	}
	rs, ok := v.source.(*RemoteSource)
	if !ok {
		return nil
	}
	snap, err := v.snapshot(ctx)
	if err != nil {
		return err
	}
	if st.known == nil {
		if st.known, err = s.Data.known(ctx, id); err != nil {
			return err
		}
	}

	type put struct {
		path string
		fi   FileInfo
		win  []byte
	}
	var puts []put
	for path, fi := range snap.files {
		if !isDataFile(path, fi) {
			continue
		}
		win := winKey(fi.winOp(path))
		if k, ok := st.known[path]; ok && k.win == string(win) {
			continue
		}
		puts = append(puts, put{path, fi, win})
	}
	var gone []string
	for path, k := range st.known {
		if fi, ok := snap.files[path]; k.deleted || (ok && isDataFile(path, fi)) {
			continue
		}
		gone = append(gone, path)
	}

	// Fetched, decoded and written a slice at a time, so a first build of a
	// large project holds one slice of documents in memory, not all of them.
	complete := true
	for len(puts) > 0 {
		n, bytes := 0, int64(0)
		for n < len(puts) && n < dataApplyChunkFiles && bytes < dataApplyChunkBytes {
			bytes += min(puts[n].fi.Size, dataMaxFile)
			n++
		}
		rows := make([]dataRow, n)
		fetched := make([]bool, n)
		g, gctx := errgroup.WithContext(ctx)
		g.SetLimit(journalFetchConcurrency)
		for i, pt := range puts[:n] {
			g.Go(func() error {
				row := dataRow{
					path: pt.path, win: pt.win, sha: pt.fi.Blob, changedAt: pt.fi.Time,
					changedBy: firstNonEmpty(pt.fi.User, pt.fi.Author),
				}
				if pt.fi.Size > dataMaxFile {
					row.err = fmt.Sprintf("larger than the %d MiB a file may index", dataMaxFile>>20)
				} else {
					rc, err := rs.OpenBlob(gctx, pt.fi.Blob)
					if err != nil {
						return nil // retried next pass; this one is incomplete
					}
					b, err := io.ReadAll(io.LimitReader(rc, dataMaxFile+1))
					rc.Close()
					if err != nil {
						return nil
					}
					row.doc, row.err = decodeData(pt.path, b)
				}
				rows[i], fetched[i] = row, true
				return nil
			})
		}
		g.Wait()
		var write []dataRow
		for i := range rows {
			if fetched[i] {
				write = append(write, rows[i])
			} else {
				complete = false
			}
		}
		if err := s.dataWriteKnown(ctx, id, st, write); err != nil {
			return err
		}
		puts = puts[n:]
	}

	// A path that stopped being data is a tombstone keyed on the op that ended
	// it, so an older fold can never put it back. A path still in the fold is
	// no longer data because its winner is something else (a link): that
	// winner is the key. A path absent from the fold ends in a delete, and
	// every delete takes effect, so its key is the greatest delete of the path.
	var forget []string
	var write []dataRow
	var absent []string
	for _, path := range gone {
		if fi, ok := snap.files[path]; ok {
			win := fi.winOp(path)
			write = append(write, dataRow{
				path: path, win: winKey(win), changedAt: win.Time,
				changedBy: firstNonEmpty(fi.User, fi.Author), deleted: true,
			})
			continue
		}
		absent = append(absent, path)
	}
	if gone = absent; len(gone) > 0 {
		ops, err := rs.loadSourcedOps(ctx)
		if err != nil {
			return err
		}
		want := map[string]bool{}
		for _, path := range gone {
			want[path] = true
		}
		last := map[string]journal.Op{}
		for _, so := range ops {
			op := so.Op
			if op.Kind != journal.KindDelete || !want[op.Path] {
				continue
			}
			if prev, ok := last[op.Path]; !ok || journal.Less(prev, op) {
				last[op.Path] = op
			}
		}
		for _, path := range gone {
			op, ok := last[path]
			if !ok {
				forget = append(forget, path)
				continue
			}
			write = append(write, dataRow{
				path: path, win: winKey(op), changedAt: op.Time,
				changedBy: firstNonEmpty(op.User, op.Author), deleted: true,
			})
		}
	}

	if err := s.dataWriteKnown(ctx, id, st, write); err != nil {
		return err
	}
	for _, path := range forget {
		if err := s.Data.forget(ctx, id, path, []byte(st.known[path].win)); err != nil {
			return err
		}
		delete(st.known, path)
	}
	if complete {
		return s.Data.setState(ctx, id, true, time.Now())
	}
	return nil
}

// dataWriteKnown writes rows and records what the index then holds. A row the
// index refused already held a newer one, from another process or a newer
// fold; recording ours as known would make later passes skip a path whose row
// they never wrote, so a refusal reloads what is really there instead.
func (s *Server) dataWriteKnown(ctx context.Context, id string, st *dataProject, rows []dataRow) error {
	refused, err := s.Data.write(ctx, id, rows)
	if err != nil || refused > 0 {
		var lerr error
		if st.known, lerr = s.Data.known(ctx, id); lerr != nil {
			st.known = nil
			return errors.Join(err, lerr)
		}
		return err
	}
	for _, r := range rows {
		st.known[r.path] = dataKnown{win: string(r.win), deleted: r.deleted}
	}
	return nil
}

// A pass fetches and writes at most this much at a time.
var (
	dataApplyChunkFiles       = 256
	dataApplyChunkBytes int64 = 16 << 20
)

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// decodeData turns a file's bytes into the document a query reads, or the
// reason it cannot. JSON is stored as written, so a number keeps every digit.
// Markdown is its frontmatter, typed by YAML; a note with none is an empty
// document, not an error — most notes are not data.
func decodeData(path string, b []byte) ([]byte, string) {
	if len(b) > dataMaxFile {
		return nil, fmt.Sprintf("larger than the %d MiB a file may index", dataMaxFile>>20)
	}
	if strings.HasSuffix(strings.ToLower(path), ".json") {
		if !json.Valid(b) {
			return nil, "not valid JSON"
		}
		return b, ""
	}
	fm, ok := frontmatterYAML(b)
	if !ok {
		return []byte("{}"), ""
	}
	var v map[string]any
	if err := yaml.Unmarshal(fm, &v); err != nil {
		return nil, "frontmatter is not valid YAML"
	}
	if v == nil {
		return []byte("{}"), ""
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil, "frontmatter has a value JSON cannot hold"
	}
	return out, ""
}

// frontmatterYAML is the YAML between a leading --- fence and its closing
// fence, the same block the viewer renders as a table.
func frontmatterYAML(src []byte) ([]byte, bool) {
	rest, ok := bytes.CutPrefix(src, []byte("---\n"))
	if !ok {
		if rest, ok = bytes.CutPrefix(src, []byte("---\r\n")); !ok {
			return nil, false
		}
	}
	loc := fmCloseRe.FindIndex(rest)
	if loc == nil {
		return nil, false
	}
	return rest[:loc[0]], true
}

// ---- the Data setting ----

// handleProjectData turns a project's data index on or off. Project admins
// only — like folder rules, it changes what the hub holds about the project.
func (s *Server) handleProjectData(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("project")
	if _, ok := s.project(w, r, id, PermAdmin); !ok {
		return
	}
	if s.Data == nil {
		http.Error(w, "this hub has no data index", http.StatusNotImplemented)
		return
	}
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&req); err != nil || req.Enabled == nil {
		http.Error(w, `send {"enabled": true} or {"enabled": false}`, http.StatusBadRequest)
		return
	}
	if err := s.Projects.SetData(id, *req.Enabled); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	st := s.dataProjectState(id)
	if *req.Enabled {
		if err := s.Data.setState(r.Context(), id, false, time.Time{}); err != nil {
			storageErr(w, http.StatusBadGateway, "could not start the data index", err)
			return
		}
		s.dataApplyAsync(id)
	} else {
		st.mu.Lock()
		err := s.Data.dropProject(r.Context(), id)
		st.known = nil
		st.mu.Unlock()
		if err != nil {
			storageErr(w, http.StatusBadGateway, "could not drop the data index", err)
			return
		}
	}
	writeJSON(w, map[string]any{"ok": true, "data": *req.Enabled})
}

// ---- queries ----

// dataReady answers for a project whose data is off, or a hub with no index.
func (s *Server) dataReady(w http.ResponseWriter, r *http.Request) bool {
	p, ok := projectFromCtx(r)
	if s.Data == nil || !ok || !p.Data {
		http.Error(w, "data queries are not enabled for this project", http.StatusNotFound)
		return false
	}
	actor := s.requestUser(r).Email
	if actor == "" {
		actor = s.clientIP(r)
	}
	if !s.dataLimiter().allow(actor) {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "too many data queries; slow down", http.StatusTooManyRequests)
		return false
	}
	return true
}

// dataLimiter is a token bucket per principal, so one caller's loop cannot
// take the query pool every screen shares.
func (s *Server) dataLimiter() *rateLimiter {
	s.dataLimitOnce.Do(func() { s.dataLimit = newRateLimiter(dataQueriesPerMinute) })
	return s.dataLimit
}

const dataQueriesPerMinute = 600

// dataCursor binds a page to the version of the results it came from.
type dataCursor struct {
	Offset int    `json:"o"`
	ETag   string `json:"e"`
}

func (s *Server) handleDataQuery(v *volume, w http.ResponseWriter, r *http.Request) {
	if !s.dataReady(w, r) {
		return
	}
	var q dataQuery
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&q); err != nil {
		http.Error(w, "bad query: "+err.Error(), http.StatusBadRequest)
		return
	}
	var cur dataCursor
	if q.Cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(q.Cursor)
		if err != nil || json.Unmarshal(raw, &cur) != nil || cur.Offset < 0 {
			http.Error(w, "invalid cursor", http.StatusBadRequest)
			return
		}
	}
	project := projectID(r)
	plan, err := compileDataQuery(s.Data.d, project, s.visibility(r), q, cur.Offset)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), dataStatementTimeout+time.Second)
	defer cancel()
	tx, err := s.Data.readTx(ctx)
	if err != nil {
		storageErr(w, http.StatusBadGateway, "data is temporarily unavailable", err)
		return
	}
	defer tx.Rollback()

	digest, errs, err := s.Data.rowsDigest(ctx, tx, plan.fileWhere, plan.fileArgs)
	if err != nil {
		storageErr(w, http.StatusBadGateway, "data is temporarily unavailable", err)
		return
	}
	pageless := q
	pageless.Cursor = ""
	etag := dataETag(pageless, digest)
	if q.Cursor != "" && cur.ETag != etag {
		http.Error(w, "the results changed since this cursor; start again from the first page", http.StatusConflict)
		return
	}
	pageTag := etag
	if cur.Offset > 0 {
		pageTag = dataETag(q, digest)
	}
	w.Header().Set("ETag", pageTag)
	w.Header().Set("Cache-Control", "private, no-cache")
	if etagMatches(r.Header.Get("If-None-Match"), pageTag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	complete, asOf, err := s.Data.state(ctx, tx, project)
	if err != nil {
		storageErr(w, http.StatusBadGateway, "data is temporarily unavailable", err)
		return
	}
	out := map[string]any{"errors": errs, "as_of": asOf, "complete": complete}

	rows, err := tx.QueryContext(ctx, plan.sql, plan.args...)
	if err != nil {
		dataQueryErr(w, err)
		return
	}
	defer rows.Close()
	switch {
	case plan.countOnly:
		var n int
		if rows.Next() {
			rows.Scan(&n)
		}
		out["count"] = n
	case plan.group:
		var groups []map[string]any
		for rows.Next() {
			vals := make([]any, len(plan.cols)+1)
			ptrs := make([]any, len(vals))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				dataQueryErr(w, err)
				return
			}
			g := map[string]any{}
			for i, e := range plan.cols {
				g[e.label] = dataValue(e, vals[i])
			}
			g["count"] = vals[len(vals)-1]
			groups = append(groups, g)
		}
		if len(groups) > dataMaxGroups {
			http.Error(w, fmt.Sprintf("more than %d groups; narrow the query", dataMaxGroups), http.StatusBadRequest)
			return
		}
		out["groups"] = nonNil(groups)
	default:
		base := dataURLBase(r) + "/" + url.PathEscape(project) + "/"
		var list []map[string]any
		docRead := map[string]bool{}
		for rows.Next() {
			vals := make([]any, len(plan.cols)+1)
			ptrs := make([]any, len(vals))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				dataQueryErr(w, err)
				return
			}
			path := sqlString(vals[0])
			row := map[string]any{}
			for i, e := range plan.cols {
				if e.label == "$url" {
					row[e.label] = base + encodeSegments(path)
					continue
				}
				if e.label == "$doc" {
					docRead[path] = true
				}
				row[e.label] = dataValue(e, vals[i+1])
			}
			list = append(list, row)
		}
		if len(list) > plan.limit {
			list = list[:plan.limit]
			next, _ := json.Marshal(dataCursor{Offset: plan.offset + plan.limit, ETag: etag})
			out["next"] = base64.RawURLEncoding.EncodeToString(next)
		}
		out["rows"] = nonNil(list)
		s.recordDataReads(r, docRead)
	}
	if err := rows.Err(); err != nil {
		dataQueryErr(w, err)
		return
	}
	body, err := json.Marshal(out)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(body) > dataMaxResponse {
		http.Error(w, "the answer is larger than 4 MiB; select fewer fields or lower the limit", http.StatusRequestEntityTooLarge)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(body)
}

// dataETag is a query's version: its meaning (the engine), the question, and
// the rows it reads as this caller sees them.
func dataETag(q dataQuery, digest string) string {
	h := sha256.New()
	h.Write([]byte(dataEngine + "\n"))
	h.Write(q.canonical())
	h.Write([]byte("\n" + digest))
	return `"` + hex.EncodeToString(h.Sum(nil)[:16]) + `"`
}

// dataQueryErr reports a statement the database refused or gave up on. The
// timeout is the caller's to act on; anything else is the hub's.
func dataQueryErr(w http.ResponseWriter, err error) {
	if strings.Contains(err.Error(), "statement timeout") || strings.Contains(err.Error(), "deadline exceeded") ||
		strings.Contains(err.Error(), "canceling statement") || strings.Contains(err.Error(), "interrupted") {
		http.Error(w, "the query took longer than 2s; narrow the pattern or the filter", http.StatusRequestTimeout)
		return
	}
	storageErr(w, http.StatusBadGateway, "the query failed", err)
}

func nonNil(v []map[string]any) []map[string]any {
	if v == nil {
		return []map[string]any{}
	}
	return v
}

func sqlString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	}
	return ""
}

// dataValue is one scanned column as JSON: a document value as the JSON it
// is, anything else as text or null.
func dataValue(e dataExpr, v any) any {
	if v == nil {
		return nil
	}
	s := sqlString(v)
	if e.kind == exprJSON {
		return json.RawMessage(s)
	}
	if n, ok := v.(int64); ok {
		return n
	}
	return s
}

// dataURLBase is the origin links point at: the MCP door's when the query
// came through it, else the request's own.
func dataURLBase(r *http.Request) string {
	if base, ok := r.Context().Value(mcpBaseKey{}).(string); ok && base != "" {
		return base
	}
	return requestBaseURL(r)
}

// recordDataReads counts a whole document returned by a query as a read of
// its file, as reading the file would: an agent's through MCP, a person's
// otherwise. A projection of a few fields is not a read.
func (s *Server) recordDataReads(r *http.Request, paths map[string]bool) {
	for path := range paths {
		if _, mcp := grantFrom(r.Context()); mcp {
			s.recordAgentRead(r, path)
		} else {
			s.recordRead(r, path)
		}
	}
}

// ---- describe ----

// dataDescribeQuery is the body of /data/describe and the MCP describe tool.
type dataDescribeQuery struct {
	Pattern string `json:"pattern" jsonschema:"Project path pattern, as in query."`
	Each    string `json:"each,omitempty" jsonschema:"JSON pointer to an array, as in query."`
	Field   string `json:"field,omitempty" jsonschema:"Describe the fields under this one (e.g. next_action). Default: the top level."`
}

// dataDescribeSample bounds how many documents describe reads.
const dataDescribeSample = 500

type fieldStat struct {
	Path    string         `json:"path"`
	Present int            `json:"present"`
	Types   map[string]int `json:"types"`
	Top     []valueCount   `json:"top,omitempty"`
	values  map[string]int
}

type valueCount struct {
	Value any `json:"value"`
	Count int `json:"count"`
}

// handleDataDescribe tells a caller what the documents under a pattern hold:
// each field one level down, how often it is there, its types, and its most
// common values — so an agent can write a query without reading files first.
func (s *Server) handleDataDescribe(v *volume, w http.ResponseWriter, r *http.Request) {
	if !s.dataReady(w, r) {
		return
	}
	var q dataDescribeQuery
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<14)).Decode(&q); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	project := projectID(r)
	plan, err := compileDataQuery(s.Data.d, project, s.visibility(r), dataQuery{
		Pattern: q.Pattern, Each: q.Each, Select: []string{"$doc"}, Limit: dataDescribeSample,
	}, 0)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var under []string
	if q.Field != "" {
		if !fieldKeyRe.MatchString(q.Field) {
			http.Error(w, "bad field", http.StatusBadRequest)
			return
		}
		under = strings.Split(q.Field, ".")
	}
	ctx, cancel := context.WithTimeout(r.Context(), dataStatementTimeout+time.Second)
	defer cancel()
	tx, err := s.Data.readTx(ctx)
	if err != nil {
		storageErr(w, http.StatusBadGateway, "data is temporarily unavailable", err)
		return
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, plan.sql, plan.args...)
	if err != nil {
		dataQueryErr(w, err)
		return
	}
	defer rows.Close()
	stats := map[string]*fieldStat{}
	docs := 0
	for rows.Next() && docs < dataDescribeSample {
		var path, doc any
		if err := rows.Scan(&path, &doc); err != nil {
			dataQueryErr(w, err)
			return
		}
		var v any
		if json.Unmarshal([]byte(sqlString(doc)), &v) != nil {
			continue
		}
		docs++
		for _, k := range under {
			m, _ := v.(map[string]any)
			v = m[k]
		}
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		for k, val := range m {
			name := k
			if q.Field != "" {
				name = q.Field + "." + k
			}
			st := stats[name]
			if st == nil {
				st = &fieldStat{Path: name, Types: map[string]int{}, values: map[string]int{}}
				stats[name] = st
			}
			st.Present++
			st.Types[jsonTypeName(val)]++
			switch val.(type) {
			case string, float64, bool:
				b, _ := json.Marshal(val)
				st.values[string(b)]++
			}
		}
	}
	out := make([]*fieldStat, 0, len(stats))
	for _, st := range stats {
		for v, n := range st.values {
			st.Top = append(st.Top, valueCount{Value: json.RawMessage(v), Count: n})
		}
		sort.Slice(st.Top, func(i, j int) bool {
			if st.Top[i].Count != st.Top[j].Count {
				return st.Top[i].Count > st.Top[j].Count
			}
			return string(st.Top[i].Value.(json.RawMessage)) < string(st.Top[j].Value.(json.RawMessage))
		})
		if len(st.Top) > 5 {
			st.Top = st.Top[:5]
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Present != out[j].Present {
			return out[i].Present > out[j].Present
		}
		return out[i].Path < out[j].Path
	})
	writeJSON(w, map[string]any{"documents": docs, "fields": out})
}

func jsonTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "boolean"
	case []any:
		return "array"
	}
	return "object"
}
