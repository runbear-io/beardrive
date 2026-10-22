package webapp

import (
	"context"
	"io"
	"strings"

	"github.com/runbear-io/beardrive/internal/remote"
)

// QuotaProvider is the seam a managed deployment uses to enforce plan
// limits, exactly like AuthProvider is the seam for identity. The
// open-source server ships only UnlimitedQuota; billing and plan logic live
// outside this repo. Hooks fire on every write path (browser uploads, the
// device sync store proxy) and on seat growth, keyed by org id.
type QuotaProvider interface {
	// CheckWrite runs before addedBytes land in the org's storage; a non-nil
	// error rejects the write (surfaced to the client as 403).
	CheckWrite(org string, addedBytes int64) error
	// CheckSeat runs before an invite adds a member; members is the current
	// count. A non-nil error rejects the join.
	CheckSeat(org string, members int) error
	// RecordUsage runs after a write succeeds, for accounting.
	RecordUsage(org string, addedBytes int64)

	// CheckRead runs before bytes are served to an UNAUTHENTICATED reader —
	// today that is public share links (/s/*) and nothing else. bytes is the
	// size about to be streamed. A non-nil error refuses the transfer and its
	// message is shown to the reader, so write it for a stranger who has no
	// idea what BearDrive is.
	//
	// Deliberately NOT called on the sync proxy or the viewer: a device that
	// gets refused mid-sync reads it as "access revoked" and stops touching
	// the folder, which is a far worse outcome than an over-quota bill. Those
	// paths are governed by fair use.
	CheckRead(org string, bytes int64) error
	// RecordEgress is CheckRead's other half: it runs after a share-link
	// transfer with the number of bytes actually written, and reports
	// nothing else, so a provider can enforce CheckRead against what it has
	// recorded.
	RecordEgress(org string, bytes int64)
}

// SyncEgressRecorder is the sync proxy's meter, an optional QuotaProvider
// capability: RecordSyncEgress runs after /store/* served a device, with
// the bytes that left the socket (compressed, when the response was).
//
// It is not RecordEgress because (org, bytes) cannot say which door the
// bytes left by: when sync reported to RecordEgress, a team's own syncing
// spent the public share allowance until every link in the org was
// refused. Measured, never enforced — there is no check for it, and a
// provider must not feed it into one.
//
// Optional rather than a QuotaProvider method because the managed hub pins
// this repo and a new required method fails its build on the bump; a
// provider without it is simply not told, and nothing is refused.
type SyncEgressRecorder interface {
	RecordSyncEgress(org string, bytes int64)
}

// UnlimitedQuota is the open-source default: everything is allowed.
type UnlimitedQuota struct{}

func (UnlimitedQuota) CheckWrite(string, int64) error { return nil }
func (UnlimitedQuota) CheckSeat(string, int) error    { return nil }
func (UnlimitedQuota) RecordUsage(string, int64)      {}
func (UnlimitedQuota) CheckRead(string, int64) error  { return nil }
func (UnlimitedQuota) RecordEgress(string, int64)     {}

// countingWriter counts what actually reached the client. The journal's Size
// field and the stat size are both claims made before the write; a connection
// that drops halfway must not be billed as a full transfer.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// quota returns the configured provider, defaulting to unlimited.
func (s *Server) quota() QuotaProvider {
	if s.Quota != nil {
		return s.Quota
	}
	return UnlimitedQuota{}
}

// billableBytes is what a write of size bytes to key should be charged.
//
// Blob keys are content-addressed, so writing one the store already holds
// replaces it with byte-identical content and grows storage by NOTHING. It
// must therefore cost nothing, on both doors: charging it inflates the usage
// counter (which only ever climbs — deletes never decrement it, and only the
// reconciler corrects it), and refusing it would block a save that adds no
// bytes at all.
//
// That matters most for co-editing, where it is not an edge case but the
// design: every peer's idle timer fires after the LAST change by ANYONE, so
// all N editors write the same converged text at once, and sharedfile.ts
// counts on "a second writer is a no-op put of a blob the store already has".
// It is a no-op for storage; it was not a no-op for the bill. N editors on
// one file charged N times per save for one blob, which is how a room of
// seven reached a storage limit that a room of one never would.
//
// Only blobs: a journal key is appended to, so its rewrite genuinely grows.
// An Exists failure charges the full size — billing must fail closed.
func billableBytes(ctx context.Context, be remote.Backend, key string, size int64) int64 {
	if !strings.HasPrefix(key, "blobs/") {
		return size
	}
	if ok, err := be.Exists(ctx, key); err == nil && ok {
		return 0
	}
	return size
}
