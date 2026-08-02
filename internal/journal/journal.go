// Package journal implements beardrive's append-only operation log.
//
// Every change to a volume is recorded as an Op in a per-device JSONL
// journal. Journals are append-only and each device only ever writes its
// own journal, so syncing is conflict-free at the transport level: a sync
// uploads your journal and downloads everyone else's. The merged view of
// a volume is a deterministic replay of the union of all ops ordered by
// (lamport, time, device, seq) — every device converges to the same state.
package journal

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	KindPut    = "put"
	KindDelete = "delete"
)

// Op is a single journaled file operation.
type Op struct {
	Seq        int64     `json:"seq"`     // per-device sequence number, 1-based
	Lamport    int64     `json:"lamport"` // logical clock for cross-device ordering
	Time       time.Time `json:"time"`
	Device     string    `json:"device"`
	DeviceName string    `json:"device_name,omitempty"`
	Author     string    `json:"author,omitempty"`    // OS/git identity (offline fallback)
	User       string    `json:"user,omitempty"`      // signed-in account email
	UserName   string    `json:"user_name,omitempty"` // signed-in account display name
	Kind       string    `json:"kind"`                // "put" or "delete"
	Path       string    `json:"path"`                // slash-separated, relative to volume root
	Blob       string    `json:"blob,omitempty"`      // sha256 hex of content (put only)
	Size       int64     `json:"size,omitempty"`
	Mode       uint32    `json:"mode,omitempty"` // permission bits
	Note       string    `json:"note,omitempty"` // e.g. "conflict copy of <path>"
	// Mtime is when the file was last written, as opposed to Time, which is
	// when the op was committed. Display only — never an input to Less or
	// Replay, since it comes from the filesystem and can be anything.
	Mtime time.Time `json:"mtime,omitzero"` // put only
}

// opWire is Op without its JSON methods, so the marshallers below can reuse
// the struct tags without recursing.
type opWire Op

// pathRaw carries Op.Path byte-exactly when it is not valid UTF-8.
// encoding/json rewrites an invalid byte to U+FFFD, and any byte but NUL and
// '/' is a legal unix filename — so without this, "caf\xe9.md" and
// "caf\xff.md" both arrive at every peer as the same path and one file
// silently overwrites the other. The extra field is emitted only for the paths
// that need it, and an older reader still sees the (lossy) path field.
type pathRaw struct {
	opWire
	PathRaw string `json:"path_raw,omitempty"`
}

func (o Op) MarshalJSON() ([]byte, error) {
	w := pathRaw{opWire: opWire(o)}
	if !utf8.ValidString(o.Path) {
		w.PathRaw = base64.StdEncoding.EncodeToString([]byte(o.Path))
	}
	return json.Marshal(w)
}

func (o *Op) UnmarshalJSON(data []byte) error {
	var w pathRaw
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	*o = Op(w.opWire)
	if w.PathRaw != "" {
		// path_raw is only ever the byte-exact SOURCE of the path field, so it
		// is applied only when it re-encodes to the path the line already
		// carries. Applied unconditionally, one line named two different files
		// — this reader materialized path_raw, a reader without this field
		// materialized `path`, and the writer picked which devices in a mixed
		// fleet saw which.
		if raw, err := base64.StdEncoding.DecodeString(w.PathRaw); err == nil &&
			lossy(string(raw)) == o.Path {
			o.Path = string(raw)
		}
	}
	return nil
}

// lossy is what encoding/json does to a string that is not valid UTF-8: each
// invalid BYTE becomes U+FFFD (not each run — strings.ToValidUTF8 collapses a
// run, which would reject a legitimate path with two bad bytes in a row). It
// is the round trip path_raw exists to undo.
func lossy(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b.WriteRune(utf8.RuneError)
			i++
			continue
		}
		b.WriteString(s[i : i+size])
		i += size
	}
	return b.String()
}

// SafePath reports whether p is a path an Op may name. It is THE rule, in one
// place: an op's path is arbitrary JSON off a peer's journal, and it is joined
// onto a working folder on every device, stored as a metadata row on the hub
// and rendered as a tree entry in the browser.
//
// It used to be spelled three times — syncer.unsafeRel (device), the core of
// webapp.cleanUploadPath (browser door) and templates.SafePath (seeding) — and
// they disagreed: unsafeRel, the rule the /store/* journal door relies on, had
// no control-character clause, so a NUL-bearing path the browser door answered
// 400 to was journaled and handed to every device. Three spellings of one rule
// is how these holes happen; callers add their OWN extra rules (reserved dirs,
// on-disk boundary) on top of this one, never a second copy of it.
//
// Refused, never normalized: normalizing would land two different journal
// paths on one file.
func SafePath(p string) bool {
	if p == "" || p == "." || p == ".." || strings.HasPrefix(p, "../") ||
		path.IsAbs(p) || filepath.IsAbs(p) || path.Clean(p) != p {
		return false
	}
	// C0 and DEL. Byte-wise on purpose: a path is bytes (see lossy — two
	// distinct legal unix filenames must not collapse), and in UTF-8 no
	// continuation byte is < 0x80, so a byte in this range is always a real
	// control character and never part of a multi-byte rune.
	//
	// They are not filenames anybody types, and NUL is a value the metadata
	// backends disagree about: Postgres refuses it in a text column (a share
	// on such a path 500s) while sqlite and the file backend keep it. DEL and
	// the C0s render as nothing, so "notes\x7f.md" and "notes.md" are two
	// indistinguishable entries in one tree. Refusing at every ingest is what
	// keeps that divergence unreachable.
	for i := 0; i < len(p); i++ {
		if p[i] < 0x20 || p[i] == 0x7f {
			return false
		}
	}
	return true
}

// Less defines the total order used to replay ops from many devices.
//
// The trailing comparisons are what make it a TOTAL order rather than a
// pre-order: (lamport, time, device, seq) is forgeable — a peer may push two
// ops sharing all four — and Sort is only stable, so without them the winner
// of a tie would be whatever order the caller happened to collect the ops in.
// Everything Replay reads is compared here, so any two ops that still tie fold
// to the same state and the invariant holds on the ops themselves.
func Less(a, b Op) bool {
	if a.Lamport != b.Lamport {
		return a.Lamport < b.Lamport
	}
	if !a.Time.Equal(b.Time) {
		return a.Time.Before(b.Time)
	}
	if a.Device != b.Device {
		return a.Device < b.Device
	}
	if a.Seq != b.Seq {
		return a.Seq < b.Seq
	}
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	if a.Path != b.Path {
		return a.Path < b.Path
	}
	if a.Blob != b.Blob {
		return a.Blob < b.Blob
	}
	if a.Size != b.Size {
		return a.Size < b.Size
	}
	return a.Mode < b.Mode
}

func Sort(ops []Op) {
	sort.SliceStable(ops, func(i, j int) bool { return Less(ops[i], ops[j]) })
}

// FileState is the resolved state of one path after replay.
type FileState struct {
	Blob string
	Size int64
	Mode uint32
}

// Replay folds a set of ops (from any number of devices) into the
// resulting volume state. Last writer wins per path under the total order.
func Replay(ops []Op) map[string]FileState {
	sorted := append([]Op(nil), ops...)
	Sort(sorted)
	state := make(map[string]FileState)
	for _, op := range sorted {
		switch op.Kind {
		case KindPut:
			state[op.Path] = FileState{Blob: op.Blob, Size: op.Size, Mode: op.Mode}
		case KindDelete:
			delete(state, op.Path)
		}
	}
	return state
}

// Parse decodes a JSONL journal.
//
// A line that does not decode is skipped, never fatal. Append is a plain
// O_APPEND write (the one state file that cannot be written atomically — it
// only ever grows), so a crash or a full disk leaves a torn final line; and a
// peer's journal is bytes someone else chose. All-or-nothing parsing turned
// either of those into "every op this device ever committed is unreadable",
// with no recovery path in the CLI. The ops that did decode are still the
// device's history, and every reader drops the same lines from the same bytes,
// so replay stays in agreement.
func Parse(data []byte) ([]Op, error) {
	var ops []Op
	for _, raw := range bytes.Split(data, []byte("\n")) {
		line := bytes.TrimSpace(raw)
		if len(line) == 0 {
			continue
		}
		var op Op
		if err := json.Unmarshal(line, &op); err != nil {
			continue
		}
		// `null`, `{}` and any object with no kind decode without error and
		// are not operations. They must produce no op: op COUNTS are the sync
		// engine's cursors (pull's fresh[len(prev):], commit's seqBase), so a
		// line that yields a phantom op shifts every op after it for one
		// reader and not another — a divergence the writer chooses.
		if op.Kind != KindPut && op.Kind != KindDelete {
			continue
		}
		ops = append(ops, op)
	}
	return ops, nil
}

// ReadFile reads a journal file; a missing file is an empty journal.
func ReadFile(path string) ([]Op, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return Parse(data)
}

// Marshal encodes ops as JSONL, the journal wire format.
func Marshal(ops []Op) ([]byte, error) {
	var buf bytes.Buffer
	for _, op := range ops {
		b, err := json.Marshal(op)
		if err != nil {
			return nil, err
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	return buf.Bytes(), nil
}

// Append appends ops to a journal file as JSONL.
func Append(path string, ops []Op) error {
	if len(ops) == 0 {
		return nil
	}
	data, err := Marshal(ops)
	if err != nil {
		return err
	}
	// 0600: a journal is the full path list, authorship and signed-in email
	// addresses of a private project, and it lives in $BDRIVE_HOME, whose
	// directories are 0755.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}
