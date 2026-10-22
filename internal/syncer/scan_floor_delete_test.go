package syncer

import (
	"testing"
)

// TestScan_FloorHiddenPathIsNotADelete pins the asymmetry between the scan
// walk and the scan's deletion pass.
//
// The walk hides a path with Filter.SkipUp (live rules plus the accepted-rules
// floor). The pass that turns "cached but not seen this scan" into delete ops
// used to ask only Filter.Skip (live rules). A device whose accepted floor is
// wider than the live rules therefore saw a file the floor hides as "gone from
// disk" and journaled a delete for it — while the file was still on its disk —
// and every peer lost it.
//
// The fixture is the way a device ends up there: it joins a project whose
// shared rules do not exclude build/ or *.mp4 with the starter template that
// `bdrive init` writes already on disk. The starter is accepted as this
// device's own (floor = build/, *.mp4); the team's rules then arrive as
// "pulled" and become the live rules, and the team's files land on disk.
func TestScan_FloorHiddenPathIsNotADelete(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	b := newDevice(t, "devb", be)

	// Device A: shared rules that do not exclude build/ or *.mp4.
	write(t, a.Folder, IgnoreFile, "# team rules\nnode_modules/\n")
	write(t, a.Folder, "docs/a.md", "doc")
	write(t, a.Folder, "x/build/n.md", "built")
	write(t, a.Folder, "v/clip.mp4", "video")
	cycle(t, a)

	// Device B: joins with the starter template on disk, so its accepted floor
	// excludes build/ and *.mp4.
	write(t, b.Folder, IgnoreFile, "# starter\nbuild/\n*.mp4\n")
	for i := 0; i < 4; i++ {
		cycle(t, b)
	}
	// A takes in whatever B journaled.
	for i := 0; i < 2; i++ {
		cycle(t, a)
	}

	for _, rel := range []string{"docs/a.md", "x/build/n.md", "v/clip.mp4"} {
		if !exists(t, a.Folder, rel) {
			t.Errorf("%s was deleted from device A by device B, but it is still on B's disk (B exists=%v)",
				rel, exists(t, b.Folder, rel))
		}
	}
}
