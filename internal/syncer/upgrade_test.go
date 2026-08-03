package syncer

import (
	"testing"
)

// TestUpgradedScopedDeviceKeepsUploading is the collateral check on the legacy
// SyncState seed (vouchedFloor). The security half of that fix lives in
// TestSec_Scope_AnUpgradedDeviceDoesNotAdoptAPeersWideningAsItsOwn; this is the
// other direction, and it is the one a blanket "drop every `!` line" would have
// broken silently.
//
// `bdrive init --only wiki` writes `/*` plus `!/wiki/` into the shared
// .bdriveignore, and `bdrive scope add` lets a TEAMMATE edit that block. So on
// an upgraded device the rules on disk are routinely a peer's, and they are
// nothing but negations. A floor that dropped them would be `/*` — every path
// excluded from upload — and the device would stop pushing anything at all,
// with no error and no cycle failure.
func TestUpgradedScopedDeviceKeepsUploading(t *testing.T) {
	be := sharedRemote(t)
	d := newDevice(t, "scoped", be)

	// The shape `init --only wiki` produces.
	const scope = "/*\n!/wiki/\n"
	write(t, d.Folder, IgnoreFile, scope)
	write(t, d.Folder, "wiki/page.md", "hello")
	write(t, d.Folder, "secret.txt", "not in scope")
	secfx13Cycle(t, d)

	paths := secfx13Paths(t, be)
	if !secfx13Has(paths, "wiki/page.md") || secfx13Has(paths, "secret.txt") {
		t.Fatalf("fixture: the scope block is not in force, hub holds %v", paths)
	}

	// The upgrade: a SyncState written before the two fields existed.
	st, err := d.Store.LoadSync()
	if err != nil {
		t.Fatal(err)
	}
	st.IgnoreAccepted, st.IgnorePulled = "", ""
	if err := d.Store.SaveSync(st); err != nil {
		t.Fatal(err)
	}

	// New work in the folder, on the first cycle after the upgrade.
	write(t, d.Folder, "wiki/second.md", "more")
	secfx13Cycle(t, d)

	paths = secfx13Paths(t, be)
	if !secfx13Has(paths, "wiki/second.md") {
		st, _ := d.Store.LoadSync()
		t.Fatalf("an upgraded device with a `bdrive scope` block stopped uploading: "+
			"the legacy floor is %q, which excludes everything.\nhub holds %v",
			st.IgnoreAccepted, paths)
	}
	if secfx13Has(paths, "secret.txt") {
		t.Fatal("the upgrade widened the scope: an out-of-scope file reached the hub")
	}
}
