package syncer

// Delta sync (docs/delta-sync-prd.md): files larger than chunkThreshold move
// as content-defined chunks plus a manifest instead of one whole blob. The
// journal format is untouched — a manifest is keyed by the FILE's sha256, so
// Op.Blob alone locates it. Chunk boundaries are chosen by content (rolling
// hash), so an insertion shifts only the chunk it lands in and every device
// derives the same chunks from the same bytes with no negotiation: the
// remote's "does this key exist" is the whole handshake.
//
// The local volume store is deliberately untouched: blobs stay whole on disk,
// chunking exists only on the wire and in the remote store.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/restic/chunker"

	"github.com/runbear-io/beardrive/internal/journal"
)

const (
	// chunkThreshold: files at or below this size take the whole-blob path —
	// below it the extra round trips cost more than the bytes they save.
	chunkThreshold = 4 << 20
	chunkMin       = 256 << 10
	chunkMax       = 4 << 20
	// manifestBound caps how much of a manifest is read: it is remote content
	// a peer wrote. 8 MiB of JSON is ~100k chunks — far past maxPullBytes'
	// worth of file.
	manifestBound = 8 << 20
)

// chunkPol is the Rabin polynomial every device uses. It must be one fixed
// value for the whole world: two devices chunking the same bytes must produce
// the same chunks, or dedup silently degrades to nothing.
const chunkPol = chunker.Pol(0x3DA3358B4DC173)

type chunkRef struct {
	H string `json:"h"`
	N int64  `json:"n"`
}

type manifest struct {
	V      int        `json:"v"`
	Size   int64      `json:"size"`
	Chunks []chunkRef `json:"chunks"`
}

// span is one chunk of a local blob: its content hash and where it lives in
// the whole file, so its bytes can be re-read without holding them all.
type span struct {
	sha  string
	off  int64
	n    int64
}

// chunkSpans splits a local blob into content-defined chunks.
func (s *Session) chunkSpans(sum string) ([]span, error) {
	f, err := s.Store.OpenBlob(sum)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	ch := chunker.NewWithBoundaries(f, chunkPol, chunkMin, chunkMax)
	buf := make([]byte, chunkMax)
	var spans []span
	var off int64
	for {
		c, err := ch.Next(buf)
		if err == io.EOF {
			return spans, nil
		}
		if err != nil {
			return nil, err
		}
		h := sha256.Sum256(c.Data)
		spans = append(spans, span{sha: hex.EncodeToString(h[:]), off: off, n: int64(len(c.Data))})
		off += int64(len(c.Data))
	}
}

// pushChunked uploads one large blob as chunks + manifest, skipping only the
// chunks the remote CONFIRMS it holds. Returns bytes actually uploaded.
// Order is chunks → manifest, so a manifest's presence implies its chunks
// exist (the delta-sync content-before-journal invariant).
//
// The skip proof is one Exists per chunk, nothing cleverer. Three cheaper
// proxies were tried and every one was false or forgeable: "I hold the
// previous version locally" (false for a basis that went up whole — grown
// across the threshold, or pushed by a pre-delta binary); "a manifest exists
// for the basis" (its first write is unverifiable, so a member could plant an
// empty one); "the stored manifest's own hash list" (a member who can read
// the file can publish its true hashes without uploading a byte). There is no
// client-side proof that a chunk is on the remote except asking the remote —
// so ask. ~1 HEAD per unchanged chunk per push of a changed file, still
// three orders of magnitude cheaper than uploading it.
func (s *Session) pushChunked(ctx context.Context, blob string) (int64, error) {
	spans, err := s.chunkSpans(blob)
	if err != nil {
		return 0, err
	}
	f, err := s.Store.OpenBlob(blob)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var uploaded int64
	man := manifest{V: 1}
	for _, sp := range spans {
		man.Size += sp.n
		man.Chunks = append(man.Chunks, chunkRef{H: sp.sha, N: sp.n})
		if ok, err := s.Backend.Exists(ctx, "chunks/"+sp.sha); err == nil && ok {
			continue
		}
		sec := io.NewSectionReader(f, sp.off, sp.n)
		if err := s.Backend.Put(ctx, "chunks/"+sp.sha, sec, sp.n); err != nil {
			return uploaded, err
		}
		uploaded += sp.n
	}
	mb, err := json.Marshal(man)
	if err != nil {
		return uploaded, err
	}
	if err := s.Backend.Put(ctx, "manifests/"+blob, bytes.NewReader(mb), int64(len(mb))); err != nil {
		// The manifest key is write-once on the hub, so a DIFFERENT body
		// already under it (a squatter, or a client with other chunker
		// parameters) is refused forever — and a push error here would have
		// wedged this device's entire push leg on every future cycle, since
		// the journal never goes up and the same job is rebuilt each time.
		// The whole blob is content-addressed and always accepted: push it
		// and move on. Costs one full upload for that file; costs nothing in
		// correctness, and turns a future chunker-parameter change into a
		// non-event.
		f, ferr := s.Store.OpenBlob(blob)
		if ferr != nil {
			return uploaded, err
		}
		fi, serr := f.Stat()
		if serr != nil {
			f.Close()
			return uploaded, err
		}
		perr := s.Backend.Put(ctx, "blobs/"+blob, f, fi.Size())
		f.Close()
		if perr != nil {
			return uploaded, err
		}
		return uploaded + fi.Size(), nil
	}
	return uploaded + int64(len(mb)), nil
}

// errNoManifest tells pull's fetch loop to take the whole-blob path: the
// remote has no manifest for this blob (an old writer pushed it whole, or the
// hub will reassemble on Get).
var errNoManifest = errors.New("no manifest")

// fetchChunked pulls a large blob via its manifest, sourcing unchanged chunks
// from the basis version already in the local store and fetching only the
// rest. The assembled bytes go through PutBlobReader, which files them under
// their COMPUTED hash — so a manifest that does not reassemble to op.Blob
// leaves HasBlob(op.Blob) false and materialize keeps skipping the path,
// exactly like a corrupt whole blob. Returns errNoManifest when the remote
// has no manifest, so the caller can fall back to the whole-blob path.
func (s *Session) fetchChunked(ctx context.Context, op journal.Op, basis string) error {
	mrc, err := s.Backend.Get(ctx, "manifests/"+op.Blob)
	if err != nil {
		return errNoManifest
	}
	var man manifest
	derr := json.NewDecoder(io.LimitReader(mrc, manifestBound)).Decode(&man)
	mrc.Close()
	if derr != nil {
		return fmt.Errorf("manifest for %s: %w", shortSha(op.Blob), derr)
	}
	var total int64
	for _, c := range man.Chunks {
		if !hexSha(c.H) || c.N < 0 {
			return fmt.Errorf("manifest for %s names an invalid chunk", shortSha(op.Blob))
		}
		total += c.N
	}
	if total > maxPullBytes {
		return errNoManifest // too big to assemble; the whole-blob path owns the ceiling
	}

	local := map[string]span{}
	var bf *os.File
	if basis != "" && s.Store.HasBlob(basis) {
		if bspans, err := s.chunkSpans(basis); err == nil {
			if bf, err = s.Store.OpenBlob(basis); err == nil {
				defer bf.Close()
				for _, sp := range bspans {
					local[sp.sha] = sp
				}
			}
		}
	}

	tmp, err := os.CreateTemp(s.Store.Dir(), ".bdrive-tmp-assemble-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	for _, c := range man.Chunks {
		if sp, ok := local[c.H]; ok && bf != nil {
			if _, err := io.Copy(tmp, io.NewSectionReader(bf, sp.off, sp.n)); err != nil {
				return err
			}
			continue
		}
		crc, err := s.Backend.Get(ctx, "chunks/"+c.H)
		if err != nil {
			return err
		}
		h := sha256.New()
		n, cerr := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(crc, c.N+1))
		crc.Close()
		if cerr != nil {
			return cerr
		}
		if n != c.N || hex.EncodeToString(h.Sum(nil)) != c.H {
			return fmt.Errorf("chunk %s of %s does not hash to its key", shortSha(c.H), shortSha(op.Blob))
		}
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	sum, _, err := s.Store.PutBlobReader(tmp)
	if err != nil {
		return err
	}
	if sum != op.Blob {
		return fmt.Errorf("%w: manifest for %s assembles to %s", errBlobContent, shortSha(op.Blob), shortSha(sum))
	}
	return nil
}

func hexSha(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

