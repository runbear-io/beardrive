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
	"io"

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
	// Text only. Merging bytes that are not lines produces a file that is
	// neither side's, and for a binary format that means a corrupt one.
	if !textmerge.IsText(baseBytes) || !textmerge.IsText(mineBytes) || !textmerge.IsText(theirsBytes) {
		return mergedBlob{}, false
	}
	out, ok := mergeText(baseBytes, mineBytes, theirsBytes)
	if !ok {
		return mergedBlob{}, false
	}
	sum, n, err := s.Store.PutBlobReader(bytes.NewReader(out))
	if err != nil {
		return mergedBlob{}, false
	}
	return mergedBlob{blob: sum, size: n}, true
}

// commonAncestor is the state both sides edited: the greatest op for this path
// ordered before both of them. Ops for other paths are irrelevant, and an op
// that is not Less than BOTH is not something both sides could have seen.
func commonAncestor(path string, mine, theirs journal.Op, all []journal.Op) (journal.Op, bool) {
	var best journal.Op
	found := false
	for _, op := range all {
		if op.Path != path || op.Kind != journal.KindPut || op.Blob == "" {
			continue
		}
		if !journal.Less(op, mine) || !journal.Less(op, theirs) {
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
