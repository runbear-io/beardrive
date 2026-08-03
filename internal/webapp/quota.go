package webapp

import "io"

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
	// paths report through RecordEgress and are governed by fair use.
	CheckRead(org string, bytes int64) error
	// RecordEgress runs after bytes have been served, with the number
	// actually written. Every read path reports here — share links, the sync
	// proxy, viewer downloads — so egress is measurable even where it is not
	// enforced.
	RecordEgress(org string, bytes int64)
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
