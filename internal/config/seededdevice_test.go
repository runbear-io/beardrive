package config

import (
	"path/filepath"
	"testing"
)

/* A hub whose $BDRIVE_HOME does not survive a restart.

   LoadDevice mints a random id when there is no device.json and writes it
   down. On a laptop that happens once. In production the hub runs with
   BDRIVE_HOME=/tmp/bdrive on a per-instance tmpfs, so it happened on every
   cold start and every revision — and because the hub journals its own writes
   under that id, each one became a permanent journal/<rand>.jsonl in every
   project it touched.

   The bill, measured on app.beardrive.ai 2026-09-21: 43 journal files on one
   project with about five real devices, a long tail of them 321 bytes — an
   instance that woke, wrote two ops and died. Every reader's cold fold pays a
   round trip per id that has ever existed, forever, and nothing ever collects
   them because a journal is append-only and might hold real work.

   So an identity that cannot be persisted is DERIVED instead of invented. */

func TestSeededDeviceSurvivesAWipedHome(t *testing.T) {
	const seed = "gs://beardrive-prod-bdrive-cloud-root"

	t.Setenv("BDRIVE_HOME", t.TempDir())
	first, err := LoadDeviceSeeded(seed)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == "" {
		t.Fatal("no device id")
	}

	// The restart: same hub, same storage, a home that did not survive.
	t.Setenv("BDRIVE_HOME", t.TempDir())
	second, err := LoadDeviceSeeded(seed)
	if err != nil {
		t.Fatal(err)
	}

	if first.ID != second.ID {
		t.Fatalf("a restart with a wiped home changed the hub's identity: %q then %q.\n"+
			"Every project this hub touches gains a journal/%s.jsonl that nothing "+
			"will ever write to again, and every reader folds it forever.",
			first.ID, second.ID, second.ID)
	}
}

// Two hubs on two different stores are two different writers, and must not
// collide: sharing one journal key is the corruption the whole "each device
// writes only its own journal" invariant exists to prevent.
func TestSeededDeviceDiffersPerStore(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	a, err := LoadDeviceSeeded("gs://bucket-one")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("BDRIVE_HOME", t.TempDir())
	b, err := LoadDeviceSeeded("gs://bucket-two")
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == b.ID {
		t.Fatalf("two hubs on different stores derived the same id %q; they would "+
			"both append one journal key", a.ID)
	}
}

// An identity already on disk WINS. Self-hosted hubs have a persistent home
// and a device.json that is already in every peer's journal listing; deriving
// over the top of it would strand the real one exactly like the bug above.
func TestSeededDeviceKeepsAnExistingIdentity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BDRIVE_HOME", home)

	existing, err := LoadDevice()
	if err != nil {
		t.Fatal(err)
	}
	got, err := LoadDeviceSeeded("gs://somewhere-else")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != existing.ID {
		t.Fatalf("a seeded load replaced the identity on disk: %q became %q; "+
			"an upgrade would strand every journal this hub has ever written",
			existing.ID, got.ID)
	}
	if _, err := readDeviceFile(filepath.Join(home, "device.json")); err != nil {
		t.Fatalf("device.json unreadable after a seeded load: %v", err)
	}
}

// The plain constructor still mints at random — that is right for a laptop,
// and it is what makes the seeded one necessary rather than a nicety.
func TestLoadDeviceStillMintsPerHome(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	a, err := LoadDevice()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("BDRIVE_HOME", t.TempDir())
	b, err := LoadDevice()
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == b.ID {
		t.Fatal("two fresh homes produced one device id; LoadDevice is supposed to mint")
	}
}
