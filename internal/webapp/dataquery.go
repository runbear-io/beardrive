package webapp

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

/* A data query names files by a path pattern and reads them as documents.

   The query carries the whole interpretation — which files, which part of
   each, which fields, how to count — so nothing about it is configured in
   the project or in the hub, and any reader can ask a new question of
   anything it can see:

	{"pattern": "projects/acme/{client}/placements/**\/*.json",
	 "each":    "/sends",
	 "where":   {"stage": "Awaiting reply", "dr": {"gt": 50}},
	 "select":  ["$path", "domain", "next_action.text"],
	 "order":   ["-$changed"],
	 "limit":   200,
	 "group":   ["$client", "week(at, Asia/Seoul)"]}

   An expression is a string: a dot path into the document ("next_action.due"),
   a system field ($path $sha $changed $changed_by $url $item $doc), a capture
   from the pattern ($client), or lower(x) / week(x) / week(x, tz). Comparisons
   are typed: dr > 50 is false for a dr that is null or a string, rather than
   an error. Everything compiles to parameterized SQL; no caller text is ever
   SQL. */

// dataQuery is the request body of /data/query and of the MCP query tool.
type dataQuery struct {
	Pattern string         `json:"pattern" jsonschema:"Project path pattern. * matches within a segment, ** any number of segments, {name} captures a segment as $name."`
	Each    string         `json:"each,omitempty" jsonschema:"JSON pointer to an array in each file (e.g. /sends). Each element becomes a row; fields then read the element."`
	Where   map[string]any `json:"where,omitempty" jsonschema:"Filter: {field: value} for equality, {field: {gt|gte|lt|lte|ne|in|nin|exists|contains: value}}, and/or/not to combine."`
	Select  []string       `json:"select,omitempty" jsonschema:"Fields to return. Default [$path]."`
	Order   []string       `json:"order,omitempty" jsonschema:"Fields to sort by; prefix - for descending. Nulls sort last."`
	Limit   int            `json:"limit,omitempty" jsonschema:"Rows per page, at most 1000. Default 100."`
	Cursor  string         `json:"cursor,omitempty" jsonschema:"The next value of a previous page."`
	Group   []string       `json:"group,omitempty" jsonschema:"Group rows by these fields and count each group."`
	Count   bool           `json:"count,omitempty" jsonschema:"Without group: return only the number of matching rows."`
}

const (
	dataDefaultLimit = 100
	dataMaxLimit     = 1000
	dataMaxGroups    = 1000
	dataMaxResponse  = 4 << 20
	// dataMaxFile bounds what one file may put in the index. A file over it
	// is a row with an error, so a query counts it as missing rather than
	// silently leaving it out.
	dataMaxFile = 2 << 20
	// dataEngine versions the meaning of a query. It is part of every ETag:
	// a change to how files decode or how a query reads them must not be
	// answered with a 304 for the old meaning.
	dataEngine = "1"
)

var (
	captureRe  = regexp.MustCompile(`^\{([a-z_][a-z0-9_]*)\}$`)
	fieldKeyRe = regexp.MustCompile(`^[^.]+(\.[^.]+)*$`)
	funcRe     = regexp.MustCompile(`^(lower|week)\((.*)\)$`)
	isoTimeRe  = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}(T([01]\d|2[0-3]):[0-5]\d:[0-5]\d(\.\d+)?(Z|[+-]\d{2}:\d{2})?)?$`)
)

// isoTimePattern is isoTimeRe for Postgres's own regex engine.
const isoTimePattern = `^\d{4}-\d{2}-\d{2}(T([01]\d|2[0-3]):[0-5]\d:[0-5]\d(\.\d+)?(Z|[+-]\d{2}:\d{2})?)?$`

// ---- patterns ----

type patSeg struct {
	lit     string // a literal segment
	glob    string // a segment with * or ?
	capture string // {name}
	star    bool   // **
}

type dataPattern struct {
	segs   []patSeg
	star   int // index of **, or -1
	prefix string
}

func parsePattern(s string) (dataPattern, error) {
	p := dataPattern{star: -1}
	if s == "" || strings.HasPrefix(s, "/") || strings.HasSuffix(s, "/") {
		return p, fmt.Errorf("pattern must be a project path like docs/*.json")
	}
	seen := map[string]bool{}
	literal := true
	for i, seg := range strings.Split(s, "/") {
		switch {
		case seg == "":
			return p, fmt.Errorf("pattern has an empty segment")
		case seg == "**":
			if p.star >= 0 {
				return p, fmt.Errorf("pattern may use ** once")
			}
			p.star, literal = i, false
			p.segs = append(p.segs, patSeg{star: true})
		case captureRe.MatchString(seg):
			name := captureRe.FindStringSubmatch(seg)[1]
			if seen[name] || systemField(name) {
				return p, fmt.Errorf("capture {%s} is used twice or is a reserved name", name)
			}
			seen[name], literal = true, false
			p.segs = append(p.segs, patSeg{capture: name})
		case strings.ContainsAny(seg, "{}"):
			return p, fmt.Errorf("segment %q: a capture is a whole segment, like {name}", seg)
		case strings.ContainsAny(seg, "*?"):
			literal = false
			p.segs = append(p.segs, patSeg{glob: seg})
		case seg == "." || seg == "..":
			return p, fmt.Errorf("pattern may not contain %q", seg)
		default:
			if literal {
				p.prefix += seg + "/"
			}
			p.segs = append(p.segs, patSeg{lit: seg})
		}
	}
	return p, nil
}

// pos is a segment's split_part position: from the start before **, from
// the end after it.
func (p dataPattern) pos(i int) int {
	if p.star >= 0 && i > p.star {
		return i - len(p.segs)
	}
	return i + 1
}

func (p dataPattern) capturePos(name string) (int, bool) {
	for i, s := range p.segs {
		if s.capture == name {
			return p.pos(i), true
		}
	}
	return 0, false
}

// ---- compiling ----

// dataSQL builds one statement's WHERE clause and select list, collecting the
// arguments in order.
type dataSQL struct {
	d     dialect
	pat   dataPattern
	each  []string // pointer keys; nil without each
	args  []any
	where []string
}

// arg adds a parameter and returns its numbered placeholder. Numbered, so a
// fragment means the same thing wherever it lands in the statement and the
// file filter can be reused verbatim after the select list.
func (c *dataSQL) arg(v any) string {
	c.args = append(c.args, v)
	if c.d == dialectPostgres {
		return "$" + strconv.Itoa(len(c.args))
	}
	return "?" + strconv.Itoa(len(c.args))
}

// fileWhere is everything that picks the files a query reads: the project,
// live rows, the pattern and the caller's folder visibility. ETags and errors
// are computed over exactly this.
func (c *dataSQL) fileWhere(project string, vis pathFilter) {
	c.where = append(c.where, "f.project = "+c.arg(project), "f.deleted = 0")
	p := c.pat
	n := len(p.segs)
	if p.prefix != "" {
		if n == strings.Count(p.prefix, "/") && p.star < 0 {
			c.where = append(c.where, "f.path = "+c.arg(strings.TrimSuffix(p.prefix, "/")))
		} else {
			// A range on the primary key, not LIKE: exact in every collation
			// and case rule, and the index serves it.
			c.where = append(c.where, "f.path >= "+c.arg(p.prefix), "f.path < "+c.arg(prefixUpper(p.prefix)))
		}
	}
	count := "(length(f.path) - length(replace(f.path, '/', '')) + 1)"
	if p.star < 0 {
		c.where = append(c.where, count+" = "+strconv.Itoa(n))
	} else {
		c.where = append(c.where, count+" >= "+strconv.Itoa(n-1))
	}
	litSegs := strings.Count(p.prefix, "/")
	for i, s := range p.segs {
		if i < litSegs || s.star || s.capture != "" {
			continue
		}
		seg := c.segment(p.pos(i))
		switch {
		case s.lit != "":
			c.where = append(c.where, seg+" = "+c.arg(s.lit))
		case c.d == dialectPostgres:
			c.where = append(c.where, seg+` LIKE `+c.arg(globLike(s.glob))+` ESCAPE '\'`)
		default:
			c.where = append(c.where, seg+" GLOB "+c.arg(globGlob(s.glob)))
		}
	}
	if vis.hides() {
		c.where = append(c.where, c.visible(vis))
	}
}

func (c *dataSQL) segment(pos int) string {
	if c.d == dialectPostgres {
		return fmt.Sprintf("split_part(f.path, '/', %d)", pos)
	}
	return fmt.Sprintf("bd_seg(f.path, %d)", pos)
}

// visible is the caller's folder rules as one predicate: the longest matching
// prefix decides, exactly as ruleFor does, and a path no rule covers is
// visible (the caller passed proj() at read).
func (c *dataSQL) visible(vis pathFilter) string {
	rules := append([]FolderRule(nil), vis.project.Folders...)
	sort.Slice(rules, func(i, j int) bool { return len(rules[i].Prefix) > len(rules[j].Prefix) })
	var b strings.Builder
	b.WriteString("(CASE")
	for _, r := range rules {
		v := 0
		if atLeast(folderLevel(vis.project, vis.email, r.Prefix, vis.base), PermRead) {
			v = 1
		}
		fmt.Fprintf(&b, " WHEN f.path >= %s AND f.path < %s THEN %d", c.arg(r.Prefix), c.arg(prefixUpper(r.Prefix)), v)
	}
	b.WriteString(" ELSE 1 END) = 1")
	return b.String()
}

// prefixUpper is the least string greater than every string with prefix p,
// for a p that ends in "/".
func prefixUpper(p string) string { return strings.TrimSuffix(p, "/") + "0" }

func globLike(g string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`, `*`, `%`, `?`, `_`)
	return r.Replace(g)
}

func globGlob(g string) string {
	r := strings.NewReplacer(`[`, `[[]`, `]`, `[]]`)
	return r.Replace(g)
}

// ---- expressions ----

type exprKind int

const (
	exprJSON exprKind = iota // a value from the document: typed
	exprText                 // a system field, capture or function: text or NULL
)

// dataExpr is one compiled expression.
type dataExpr struct {
	label string
	kind  exprKind
	field []string // document path, for exprJSON
	sql   string   // for exprText
}

func systemField(name string) bool {
	switch name {
	case "path", "sha", "changed", "changed_by", "url", "item", "doc":
		return true
	}
	return false
}

func (c *dataSQL) expr(s string) (dataExpr, error) {
	s = strings.TrimSpace(s)
	if m := funcRe.FindStringSubmatch(s); m != nil {
		args := strings.Split(m[2], ",")
		inner, err := c.expr(args[0])
		if err != nil {
			return dataExpr{}, err
		}
		text := c.asText(inner)
		switch m[1] {
		case "lower":
			if len(args) != 1 {
				return dataExpr{}, fmt.Errorf("lower takes one field")
			}
			f := "lower"
			if c.d == dialectSQLite {
				f = "bd_lower"
			}
			return dataExpr{label: s, kind: exprText, sql: f + "(" + text + ")"}, nil
		case "week":
			tz := "UTC"
			if len(args) == 2 {
				tz = strings.TrimSpace(args[1])
			} else if len(args) > 2 {
				return dataExpr{}, fmt.Errorf("week takes a field and a time zone")
			}
			if _, err := time.LoadLocation(tz); err != nil {
				return dataExpr{}, fmt.Errorf("unknown time zone %q", tz)
			}
			if c.d == dialectSQLite {
				return dataExpr{label: s, kind: exprText, sql: "bd_week(" + text + ", " + c.arg(tz) + ")"}, nil
			}
			v := text
			return dataExpr{label: s, kind: exprText, sql: fmt.Sprintf(
				`(CASE WHEN %[1]s ~ CAST(%[2]s AS text) AND pg_input_is_valid(%[1]s, 'timestamptz') `+
					`THEN to_char((%[1]s)::timestamptz AT TIME ZONE CAST(%[3]s AS text), 'IYYY-"W"IW') END)`,
				v, c.arg(isoTimePattern), c.arg(tz))}, nil
		}
	}
	if name, ok := strings.CutPrefix(s, "$"); ok {
		switch name {
		case "path", "url":
			return dataExpr{label: s, kind: exprText, sql: "f.path"}, nil
		case "sha":
			return dataExpr{label: s, kind: exprText, sql: "f.sha"}, nil
		case "changed":
			return dataExpr{label: s, kind: exprText, sql: "f.changed_at"}, nil
		case "changed_by":
			return dataExpr{label: s, kind: exprText, sql: "f.changed_by"}, nil
		case "item":
			if c.each == nil {
				return dataExpr{}, fmt.Errorf("$item needs each")
			}
			ptr := "/" + strings.Join(escapePointer(c.each), "/") + "/"
			if c.d == dialectPostgres {
				return dataExpr{label: s, kind: exprText, sql: "CAST(" + c.arg(ptr) + " AS text) || (item.idx - 1)::text"}, nil
			}
			return dataExpr{label: s, kind: exprText, sql: c.arg(ptr) + " || item.key"}, nil
		case "doc":
			return dataExpr{label: s, kind: exprJSON}, nil
		}
		if pos, ok := c.pat.capturePos(name); ok {
			return dataExpr{label: s, kind: exprText, sql: c.segment(pos)}, nil
		}
		return dataExpr{}, fmt.Errorf("unknown field %s", s)
	}
	if !fieldKeyRe.MatchString(s) {
		return dataExpr{}, fmt.Errorf("bad field %q", s)
	}
	return dataExpr{label: s, kind: exprJSON, field: strings.Split(s, ".")}, nil
}

func escapePointer(keys []string) []string {
	out := make([]string, len(keys))
	r := strings.NewReplacer("~", "~0", "/", "~1")
	for i, k := range keys {
		out[i] = r.Replace(k)
	}
	return out
}

// parsePointer reads a JSON pointer of object keys.
func parsePointer(p string) ([]string, error) {
	if !strings.HasPrefix(p, "/") || p == "/" {
		return nil, fmt.Errorf("each must be a JSON pointer like /sends")
	}
	keys := strings.Split(p[1:], "/")
	r := strings.NewReplacer("~1", "/", "~0", "~")
	for i, k := range keys {
		if k == "" {
			return nil, fmt.Errorf("each has an empty key")
		}
		keys[i] = r.Replace(k)
	}
	return keys, nil
}

// The document value an expression reads, per dialect. jsonSQL is the JSON
// value (for select and grouping), typeSQL its JSON type, textSQL a string's
// text, numSQL a number's value.

func (c *dataSQL) sqlitePath(keys []string) string {
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(`."`)
		b.WriteString(strings.ReplaceAll(strings.ReplaceAll(k, `\`, `\\`), `"`, `\"`))
		b.WriteString(`"`)
	}
	return b.String()
}

// sqliteDocPath is the JSON path of keys inside each row's document: from the
// root, or from the current element of each.
func (c *dataSQL) sqliteDocPath(keys []string) string {
	if c.each != nil {
		return "(item.fullkey || " + c.arg(c.sqlitePath(keys)) + ")"
	}
	return c.arg("$" + c.sqlitePath(keys))
}

func (c *dataSQL) pgBase() string {
	if c.each != nil {
		return "item.value"
	}
	return "f.doc"
}

func (c *dataSQL) pgPath(keys []string) string {
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = c.arg(k)
	}
	return strings.Join(parts, ", ")
}

func (c *dataSQL) jsonSQL(e dataExpr) string {
	if c.d == dialectPostgres {
		if e.field == nil {
			return c.pgBase() + "::text"
		}
		return "jsonb_extract_path(" + c.pgBase() + ", " + c.pgPath(e.field) + ")::text"
	}
	if e.field == nil {
		if c.each != nil {
			return "(f.doc -> item.fullkey)"
		}
		return "f.doc"
	}
	return "(f.doc -> " + c.sqliteDocPath(e.field) + ")"
}

func (c *dataSQL) typeSQL(keys []string) string {
	if c.d == dialectPostgres {
		return "jsonb_typeof(jsonb_extract_path(" + c.pgBase() + ", " + c.pgPath(keys) + "))"
	}
	return "json_type(f.doc, " + c.sqliteDocPath(keys) + ")"
}

func (c *dataSQL) textSQL(keys []string) string {
	if c.d == dialectPostgres {
		return "jsonb_extract_path_text(" + c.pgBase() + ", " + c.pgPath(keys) + ")"
	}
	return "json_extract(f.doc, " + c.sqliteDocPath(keys) + ")"
}

// isType is the condition that a document value has a JSON type, in the
// dialect's own names.
func (c *dataSQL) isType(keys []string, typ string) string {
	t := c.typeSQL(keys)
	if c.d == dialectPostgres {
		return t + " = '" + typ + "'"
	}
	switch typ {
	case "number":
		return t + " IN ('integer', 'real')"
	case "string":
		return t + " = 'text'"
	case "boolean":
		return t + " IN ('true', 'false')"
	}
	return t + " = '" + typ + "'"
}

// asText is an expression as text: a string field's value, or NULL for any
// other type, so that a function never turns a number into a string.
func (c *dataSQL) asText(e dataExpr) string {
	if e.kind == exprText {
		return e.sql
	}
	if e.field == nil {
		return "NULL"
	}
	return "(CASE WHEN " + c.isType(e.field, "string") + " THEN " + c.textSQL(e.field) + " END)"
}

func (c *dataSQL) asNumber(e dataExpr) string {
	if e.kind == exprText || e.field == nil {
		return "NULL"
	}
	num := c.textSQL(e.field)
	if c.d == dialectPostgres {
		num = "(" + num + ")::numeric"
	}
	return "(CASE WHEN " + c.isType(e.field, "number") + " THEN " + num + " END)"
}

// ---- where ----

func (c *dataSQL) cond(where map[string]any) (string, error) {
	if len(where) == 0 {
		return "1 = 1", nil
	}
	keys := make([]string, 0, len(where))
	for k := range where {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		v := where[k]
		var part string
		var err error
		switch k {
		case "and", "or":
			list, ok := v.([]any)
			if !ok || len(list) == 0 {
				return "", fmt.Errorf("%s takes a list of filters", k)
			}
			var subs []string
			for _, item := range list {
				m, ok := item.(map[string]any)
				if !ok {
					return "", fmt.Errorf("%s takes a list of filters", k)
				}
				sub, err := c.cond(m)
				if err != nil {
					return "", err
				}
				subs = append(subs, "("+sub+")")
			}
			part = strings.Join(subs, " "+strings.ToUpper(k)+" ")
		case "not":
			m, ok := v.(map[string]any)
			if !ok {
				return "", fmt.Errorf("not takes a filter")
			}
			sub, err := c.cond(m)
			if err != nil {
				return "", err
			}
			part = "NOT (" + sub + ")"
		default:
			part, err = c.fieldCond(k, v)
		}
		if err != nil {
			return "", err
		}
		parts = append(parts, "("+part+")")
	}
	return strings.Join(parts, " AND "), nil
}

func (c *dataSQL) fieldCond(field string, v any) (string, error) {
	e, err := c.expr(field)
	if err != nil {
		return "", err
	}
	if e.kind == exprJSON && e.field == nil {
		return "", fmt.Errorf("$doc cannot be filtered on")
	}
	ops, ok := v.(map[string]any)
	if !ok {
		return c.compare(e, "eq", v)
	}
	names := make([]string, 0, len(ops))
	for op := range ops {
		names = append(names, op)
	}
	sort.Strings(names)
	var parts []string
	for _, op := range names {
		p, err := c.compare(e, op, ops[op])
		if err != nil {
			return "", err
		}
		parts = append(parts, "("+p+")")
	}
	return strings.Join(parts, " AND "), nil
}

func (c *dataSQL) compare(e dataExpr, op string, v any) (string, error) {
	switch op {
	case "eq":
		return c.eq(e, v)
	case "ne":
		p, err := c.eq(e, v)
		return "NOT (" + p + ")", err
	case "in", "nin":
		list, ok := v.([]any)
		if !ok {
			return "", fmt.Errorf("%s takes a list", op)
		}
		var subs []string
		for _, item := range list {
			p, err := c.eq(e, item)
			if err != nil {
				return "", err
			}
			subs = append(subs, "("+p+")")
		}
		if len(subs) == 0 {
			subs = []string{"1 = 0"}
		}
		p := strings.Join(subs, " OR ")
		if op == "nin" {
			p = "NOT (" + p + ")"
		}
		return p, nil
	case "exists":
		want, ok := v.(bool)
		if !ok {
			return "", fmt.Errorf("exists takes true or false")
		}
		p := c.asText(e) + " IS NOT NULL"
		if e.kind == exprJSON {
			p = "(" + c.typeSQL(e.field) + " IS NOT NULL AND NOT (" + c.isType(e.field, "null") + "))"
		}
		if !want {
			p = "NOT " + p
		}
		return p, nil
	case "contains":
		s, ok := v.(string)
		if !ok {
			return "", fmt.Errorf("contains takes a string")
		}
		f := "strpos(%s, %s) > 0"
		if c.d == dialectSQLite {
			f = "instr(%s, %s) > 0"
		}
		text := c.asText(e)
		return fmt.Sprintf("("+text+" IS NOT NULL AND "+f+")", text, c.arg(s)), nil
	case "gt", "gte", "lt", "lte":
		sym := map[string]string{"gt": ">", "gte": ">=", "lt": "<", "lte": "<="}[op]
		switch x := v.(type) {
		case float64:
			return c.asNumber(e) + " " + sym + " " + c.numArg(x), nil
		case string:
			return c.asText(e) + c.collate() + " " + sym + " " + c.arg(x), nil
		}
		return "", fmt.Errorf("%s takes a number or a string", op)
	}
	return "", fmt.Errorf("unknown operator %q", op)
}

func (c *dataSQL) eq(e dataExpr, v any) (string, error) {
	switch x := v.(type) {
	case nil:
		if e.kind == exprText {
			return e.sql + " IS NULL", nil
		}
		return "(" + c.typeSQL(e.field) + " IS NULL OR " + c.isType(e.field, "null") + ")", nil
	case string:
		return c.asText(e) + " = " + c.arg(x), nil
	case float64:
		return c.asNumber(e) + " = " + c.numArg(x), nil
	case bool:
		if e.kind == exprText {
			return "1 = 0", nil
		}
		if c.d == dialectPostgres {
			return "(" + c.isType(e.field, "boolean") + " AND " + c.textSQL(e.field) + " = " + c.arg(strconv.FormatBool(x)) + ")", nil
		}
		return c.typeSQL(e.field) + " = " + c.arg(strconv.FormatBool(x)), nil
	}
	return "", fmt.Errorf("compare with a string, a number, true, false or null")
}

func (c *dataSQL) numArg(x float64) string {
	if c.d == dialectPostgres {
		return "CAST(" + c.arg(strconv.FormatFloat(x, 'g', -1, 64)) + " AS numeric)"
	}
	return c.arg(x)
}

// collate makes Postgres compare text byte-wise, as SQLite and Go do.
func (c *dataSQL) collate() string {
	if c.d == dialectPostgres {
		return ` COLLATE "C"`
	}
	return ""
}

// ---- order ----

// orderBy sorts by each expression with nulls last; a document value sorts
// numbers by value and strings byte-wise, numbers first.
func (c *dataSQL) orderBy(e dataExpr, desc bool) []string {
	dir := " ASC"
	if desc {
		dir = " DESC"
	}
	if e.kind == exprText {
		return []string{"(" + e.sql + " IS NULL)", e.sql + c.collate() + dir}
	}
	num, text := c.asNumber(e), c.asText(e)
	return []string{
		"(" + num + " IS NULL AND " + text + " IS NULL)",
		"(" + num + " IS NULL)",
		num + dir,
		text + c.collate() + dir,
	}
}

// ---- the statement ----

// dataPlan is one compiled query.
type dataPlan struct {
	fileWhere string // the files: pattern + visibility, over f
	fileArgs  []any
	sql       string // the rows or groups
	args      []any
	cols      []dataExpr
	group     bool
	countOnly bool
	limit     int
	offset    int
}

func compileDataQuery(d dialect, project string, vis pathFilter, q dataQuery, offset int) (*dataPlan, error) {
	pat, err := parsePattern(q.Pattern)
	if err != nil {
		return nil, err
	}
	c := &dataSQL{d: d, pat: pat}
	if q.Each != "" {
		if c.each, err = parsePointer(q.Each); err != nil {
			return nil, err
		}
	}
	plan := &dataPlan{limit: q.Limit, offset: offset}
	if plan.limit == 0 {
		plan.limit = dataDefaultLimit
	}
	if plan.limit < 0 || plan.limit > dataMaxLimit {
		return nil, fmt.Errorf("limit must be between 1 and %d", dataMaxLimit)
	}

	// The file filter first, on its own, for the ETag and the error count.
	c.fileWhere(project, vis)
	plan.fileWhere, plan.fileArgs = strings.Join(c.where, " AND "), c.args
	fileWhere := plan.fileWhere

	// Then the statement, which repeats those arguments in the same order.
	c.args = append([]any(nil), plan.fileArgs...)
	from := "data_items AS f"
	conds := []string{fileWhere, "f.error = ''"}
	if c.each != nil {
		if d == dialectPostgres {
			arr := "jsonb_extract_path(f.doc, " + c.pgPath(c.each) + ")"
			from += " CROSS JOIN LATERAL jsonb_array_elements(CASE WHEN jsonb_typeof(" + arr +
				") = 'array' THEN " + arr + " ELSE '[]'::jsonb END) WITH ORDINALITY AS item(value, idx)"
		} else {
			path := c.arg("$" + c.sqlitePath(c.each))
			from += " JOIN json_each(f.doc, " + path + ") AS item"
			conds = append(conds, "json_type(f.doc, "+c.arg("$"+c.sqlitePath(c.each))+") = 'array'")
		}
	}
	where, err := c.cond(q.Where)
	if err != nil {
		return nil, err
	}
	conds = append(conds, where)

	switch {
	case len(q.Group) > 0:
		plan.group = true
		var sel, by []string
		for _, g := range q.Group {
			e, err := c.expr(g)
			if err != nil {
				return nil, err
			}
			if e.kind == exprJSON && e.field == nil {
				return nil, fmt.Errorf("cannot group by $doc")
			}
			col := e.sql
			if e.kind == exprJSON {
				col = c.jsonSQL(e)
			}
			plan.cols = append(plan.cols, e)
			sel = append(sel, col)
			by = append(by, strconv.Itoa(len(sel)))
		}
		// By the expressions, not positions: Postgres will not collate a
		// position, and the placeholders mean the same thing twice.
		order := make([]string, len(sel))
		for i := range sel {
			order[i] = sel[i] + c.collate()
		}
		plan.sql = "SELECT " + strings.Join(sel, ", ") + ", count(*) FROM " + from +
			" WHERE " + strings.Join(conds, " AND ") + " GROUP BY " + strings.Join(by, ", ") +
			" ORDER BY " + strings.Join(order, ", ") + " LIMIT " + strconv.Itoa(dataMaxGroups+1)
	case q.Count:
		plan.countOnly = true
		plan.sql = "SELECT count(*) FROM " + from + " WHERE " + strings.Join(conds, " AND ")
	default:
		selects := q.Select
		if len(selects) == 0 {
			selects = []string{"$path"}
		}
		sel := []string{"f.path"}
		for _, s := range selects {
			e, err := c.expr(s)
			if err != nil {
				return nil, err
			}
			col := e.sql
			if e.kind == exprJSON {
				col = c.jsonSQL(e)
			}
			plan.cols = append(plan.cols, e)
			sel = append(sel, col)
		}
		var order []string
		for _, o := range q.Order {
			desc := strings.HasPrefix(o, "-")
			e, err := c.expr(strings.TrimPrefix(o, "-"))
			if err != nil {
				return nil, err
			}
			if e.kind == exprJSON && e.field == nil {
				return nil, fmt.Errorf("cannot order by $doc")
			}
			order = append(order, c.orderBy(e, desc)...)
		}
		// A stable tie-break, so pages never overlap or skip.
		order = append(order, "f.path"+c.collate())
		if c.each != nil {
			if d == dialectPostgres {
				order = append(order, "item.idx")
			} else {
				order = append(order, "item.key")
			}
		}
		plan.sql = "SELECT " + strings.Join(sel, ", ") + " FROM " + from +
			" WHERE " + strings.Join(conds, " AND ") + " ORDER BY " + strings.Join(order, ", ") +
			" LIMIT " + strconv.Itoa(plan.limit+1) + " OFFSET " + strconv.Itoa(offset)
	}
	plan.args = c.args
	return plan, nil
}

// canonical is a query as its ETag hashes it: every field, in a fixed order.
func (q dataQuery) canonical() []byte {
	b, _ := json.Marshal(q)
	return b
}
