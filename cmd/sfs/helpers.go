package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/runbear-io/sfs/internal/config"
	"github.com/runbear-io/sfs/internal/remote"
	"github.com/runbear-io/sfs/internal/store"
	"github.com/runbear-io/sfs/internal/syncer"
)

func absFolder(args []string) (string, error) {
	arg := "."
	if len(args) > 0 {
		arg = args[0]
	}
	return filepath.Abs(arg)
}

// mustMount resolves a folder's settings: the .sfs project file wins over
// the global registry, so a folder that carries its own .sfs works even
// before it is registered on this device.
func mustMount(folder string) (config.MountInfo, error) {
	mi, _, found, err := config.EffectiveMount(folder)
	if err != nil {
		return mi, err
	}
	if !found {
		return mi, fmt.Errorf("%s is not an sfs mount (run `sfs mnt %s` first)", folder, folder)
	}
	if mi.Volume == "" {
		mi.Volume = filepath.Base(folder)
	}
	return mi, nil
}

// openSession builds a syncer session for a mounted folder. When withRemote
// is set and the remote is unreachable, it degrades to offline with a warning
// rather than failing.
func openSession(ctx context.Context, folder string, withRemote bool) (*syncer.Session, config.MountInfo, error) {
	mi, err := mustMount(folder)
	if err != nil {
		return nil, mi, err
	}
	dev, err := config.LoadDevice()
	if err != nil {
		return nil, mi, err
	}
	vdir, err := config.VolumeDir(mi.Volume)
	if err != nil {
		return nil, mi, err
	}
	st, err := store.Open(vdir)
	if err != nil {
		return nil, mi, err
	}
	sess := &syncer.Session{Folder: folder, Store: st, Device: dev}
	if withRemote && mi.Remote != "" {
		be, err := remote.Open(ctx, mi.Remote)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: remote unavailable, working offline: %v\n", err)
		} else {
			sess.Backend = be
		}
	}
	return sess, mi, nil
}

func closeSession(sess *syncer.Session) {
	if sess != nil && sess.Backend != nil {
		sess.Backend.Close()
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func printCycle(res *syncer.Result) {
	fmt.Printf("  local changes:  %d\n", res.LocalOps)
	fmt.Printf("  pulled changes: %d\n", res.PulledOps)
	if res.Conflicts > 0 {
		fmt.Printf("  conflicts:      %d (preserved as *.sfs-conflict-* files)\n", res.Conflicts)
	}
	fmt.Printf("  files updated:  %d\n", res.Materialized)
	switch {
	case res.Offline:
		fmt.Printf("  remote:         offline (%v)\n", res.OfflineErr)
	case res.Pushed:
		fmt.Printf("  remote:         pushed\n")
	}
}
