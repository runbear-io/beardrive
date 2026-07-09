package webapp

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
}

// UnlimitedQuota is the open-source default: everything is allowed.
type UnlimitedQuota struct{}

func (UnlimitedQuota) CheckWrite(string, int64) error { return nil }
func (UnlimitedQuota) CheckSeat(string, int) error    { return nil }
func (UnlimitedQuota) RecordUsage(string, int64)      {}

// quota returns the configured provider, defaulting to unlimited.
func (s *Server) quota() QuotaProvider {
	if s.Quota != nil {
		return s.Quota
	}
	return UnlimitedQuota{}
}
