package syncer

// Three-way merge for text files, so two people editing different parts of
// one file get one file instead of a conflict copy each.
//
// Hand-written, no dependency — the same call the frontend's line diff made
// and documented ("an LCS is ~40 lines, which is less code than auditing a
// diff package would be"). The same reasoning applies harder here: this runs
// inside conflictCopies, which has a security history, so the code that
// decides what a merged file contains should be readable in one sitting.
//
// Line-based on purpose. Word- or character-level merging resolves more
// cases and silently produces sentences neither author wrote; a line is the
// unit people actually edit, and anything overlapping falls through to the
// conflict copy that exists today.

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"

	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/textmerge"
)

// mergeText merges two edits of the same base; see textmerge.Merge.
func mergeText(base, a, b []byte) ([]byte, bool) { return textmerge.Merge(base, a, b) }

// mergeNote marks an op this device produced by merging two concurrent edits,
// rather than by a local write. Display only — like every other Note, it is
// never an input to Less or Replay.
const mergeNote = "merged concurrent edits"

// keptOverEmptyNote marks an op that puts content back at a path an empty
// concurrent write had won. Display only, like mergeNote.
const keptOverEmptyNote = "kept content over a concurrent empty write"

// emptyBlob is sha256(""): the one blob that is an empty file.
const emptyBlob = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// mergedBlob is what tryMerge stored.
type mergedBlob struct {
	blob string
	size int64
}

// maxMergeBytes caps what will be read into memory to merge. A file bigger
// than this is a file nobody is hand-editing two copies of, and the conflict
// copy is the honest answer for it.
const maxMergeBytes = 4 << 20

// tryMerge attempts a three-way merge of two concurrent puts at the same path
// and, on success, stores the result as a blob.
//
// The common ancestor is the newest op for this path that both sides are
// built on: the greatest op (by journal.Less) that is Less than both. That is
// the state each side started from, which is exactly what a three-way merge
// needs.
//
// Everything here declines rather than guesses. No ancestor, a binary file,
// anything too big, overlapping edits, a blob that will not read — all fall
// back to the conflict copy that existed before.
func (s *Session) tryMerge(path string, mine, theirs journal.Op, all []journal.Op) (mergedBlob, bool) {
	if mine.Kind != journal.KindPut || theirs.Kind != journal.KindPut {
		return mergedBlob{}, false // a delete has no text to merge
	}
	if mine.Blob == theirs.Blob {
		return mergedBlob{}, false // same content; the caller already handles this
	}
	if mine.Size > maxMergeBytes || theirs.Size > maxMergeBytes {
		return mergedBlob{}, false
	}
	base, ok := commonAncestor(path, mine, theirs, all)
	if !ok {
		return mergedBlob{}, false
	}
	baseBytes, ok1 := s.readBlob(base.Blob, maxMergeBytes)
	mineBytes, ok2 := s.readBlob(mine.Blob, maxMergeBytes)
	theirsBytes, ok3 := s.readBlob(theirs.Blob, maxMergeBytes)
	if !ok1 || !ok2 || !ok3 {
		return mergedBlob{}, false
	}
	// JSONL is append-only in practice. Preserve both valid EOF appends,
	// ordering them by the same total order used by replay.
	if strings.HasSuffix(strings.ToLower(path), ".jsonl") {
		if out, ok := mergeJSONLAppends(baseBytes, mineBytes, theirsBytes, mine, theirs); ok && len(out) <= maxMergeBytes {
			sum, n, err := s.Store.PutBlobReader(bytes.NewReader(out))
			if err != nil {
				return mergedBlob{}, false
			}
			return mergedBlob{blob: sum, size: n}, true
		}
	}
	// Text only. Merging bytes that are not lines produces a file that is
	// neither side's, and for a binary format that means a corrupt one.
	if !textmerge.IsText(baseBytes) || !textmerge.IsText(mineBytes) || !textmerge.IsText(theirsBytes) {
		return mergedBlob{}, false
	}
	out, ok := mergeText(baseBytes, mineBytes, theirsBytes)
	// Two sides each under the cap can insert past it together; a merge no
	// device would merge again is left to the conflict copy.
	if !ok || len(out) > maxMergeBytes {
		return mergedBlob{}, false
	}
	if strings.HasSuffix(strings.ToLower(path), ".json") && !json.Valid(out) {
		return mergedBlob{}, false
	}
	sum, n, err := s.Store.PutBlobReader(bytes.NewReader(out))
	if err != nil {
		return mergedBlob{}, false
	}
	return mergedBlob{blob: sum, size: n}, true
}

func mergeJSONLAppends(base, a, b []byte, mine, theirs journal.Op) ([]byte, bool) {
	if !bytes.HasPrefix(a, base) || !bytes.HasPrefix(b, base) {
		return nil, false
	}
	aa, bb := a[len(base):], b[len(base):]
	valid := func(part []byte) bool {
		part = bytes.TrimSuffix(part, []byte{'\n'})
		if len(part) == 0 {
			return true
		}
		for _, line := range bytes.Split(part, []byte{'\n'}) {
			line = bytes.TrimSpace(line)
			if len(line) == 0 || !json.Valid(line) {
				return false
			}
		}
		return true
	}
	if !valid(base) {
		return nil, false
	}
	if len(base) > 0 && base[len(base)-1] != '\n' {
		if (len(aa) > 0 && aa[0] != '\n') || (len(bb) > 0 && bb[0] != '\n') {
			return nil, false
		}
		aa = bytes.TrimPrefix(aa, []byte{'\n'})
		bb = bytes.TrimPrefix(bb, []byte{'\n'})
	}
	if !valid(aa) || !valid(bb) {
		return nil, false
	}
	// The common prefix may not end at a line boundary. A pure append must
	// preserve the base's line framing and insert one separator when needed.
	sep := func(left, right []byte) []byte {
		if len(left) == 0 || len(right) == 0 || left[len(left)-1] == '\n' {
			return nil
		}
		return []byte{'\n'}
	}
	first, second := aa, bb
	if journal.Less(theirs, mine) {
		first, second = bb, aa
	}
	out := append([]byte(nil), base...)
	out = append(out, sep(out, first)...)
	out = append(out, first...)
	out = append(out, sep(out, second)...)
	out = append(out, second...)
	return out, true
}

// commonAncestor is the state both sides edited: the greatest op for this path
// ordered before both of them. Ops for other paths are irrelevant, and an op
// that is not Less than BOTH is not something both sides could have seen.
func commonAncestor(path string, mine, theirs journal.Op, all []journal.Op) (journal.Op, bool) {
	if mine.Base != "" && mine.Base == theirs.Base {
		for _, op := range all {
			if op.Path == path && op.Kind == journal.KindPut && op.Blob == mine.Base {
				return op, true
			}
		}
		return journal.Op{Kind: journal.KindPut, Path: path, Blob: mine.Base}, true
	}
	var mineCeil, theirsCeil journal.Op
	var hasMineCeil, hasTheirsCeil bool
	for _, op := range all {
		if op.Path != path || op.Kind != journal.KindPut || op.Blob == "" {
			continue
		}
		if mine.Base != "" && op.Blob == mine.Base && journal.Less(op, mine) {
			if !hasMineCeil || journal.Less(mineCeil, op) {
				mineCeil, hasMineCeil = op, true
			}
		}
		if theirs.Base != "" && op.Blob == theirs.Base && journal.Less(op, theirs) {
			if !hasTheirsCeil || journal.Less(theirsCeil, op) {
				theirsCeil, hasTheirsCeil = op, true
			}
		}
	}
	var best journal.Op
	found := false
	for _, op := range all {
		if op.Path != path || op.Kind != journal.KindPut || op.Blob == "" {
			continue
		}
		if !journal.Less(op, mine) || !journal.Less(op, theirs) {
			continue
		}
		if hasMineCeil && journal.Less(mineCeil, op) {
			continue
		}
		if hasTheirsCeil && journal.Less(theirsCeil, op) {
			continue
		}
		if !found || journal.Less(best, op) {
			best, found = op, true
		}
	}
	return best, found
}

// readBlob reads a stored blob, refusing anything past the cap.
func (s *Session) readBlob(sum string, max int64) ([]byte, bool) {
	rc, err := s.Store.OpenBlob(sum)
	if err != nil {
		return nil, false
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, max+1))
	if err != nil || int64(len(b)) > max {
		return nil, false
	}
	return b, true
}
