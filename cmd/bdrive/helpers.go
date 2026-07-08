package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/remote"
	"github.com/runbear-io/beardrive/internal/store"
	"github.com/runbear-io/beardrive/internal/syncer"
)

func absFolder(args []string) (string, error) {
	arg := "."
	if len(args) > 0 {
		arg = args[0]
	}
	return filepath.Abs(arg)
}

// mustProject resolves a folder's project settings (from .bdrive/config.json,
// self-healing the registry when the folder moved).
func mustProject(folder string) (config.Project, error) {
	proj, found, err := config.ResolveMount(folder)
	if err != nil {
		return proj, err
	}
	if !found {
		return proj, fmt.Errorf("%s is not a beardrive project (run `bdrive init` there first)", folder)
	}
	if proj.Volume == "" {
		proj.Volume = filepath.Base(folder)
	}
	return proj, nil
}

// openSession builds a syncer session for a project folder. When withRemote
// is set and the remote is unreachable, it degrades to offline with a warning
// rather than failing.
func openSession(ctx context.Context, folder string, withRemote bool) (*syncer.Session, config.Project, error) {
	proj, err := mustProject(folder)
	if err != nil {
		return nil, proj, err
	}
	dev, err := config.LoadDevice()
	if err != nil {
		return nil, proj, err
	}
	vdir, err := config.VolumeDir(proj.ID)
	if err != nil {
		return nil, proj, err
	}
	st, err := store.Open(vdir)
	if err != nil {
		return nil, proj, err
	}
	settings, _ := config.LoadSettings()
	sess := &syncer.Session{Folder: folder, MountID: proj.ID, Store: st, Device: dev, Account: settings}
	if withRemote && proj.Remote != "" {
		be, err := remote.Open(ctx, proj.Remote)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: remote unavailable, working offline: %v\n", err)
		} else {
			sess.Backend = be
		}
	}
	return sess, proj, nil
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
		fmt.Printf("  conflicts:      %d (preserved as *.bdrive-conflict-* files)\n", res.Conflicts)
	}
	fmt.Printf("  files updated:  %d\n", res.Materialized)
	switch {
	case res.Offline:
		fmt.Printf("  remote:         offline (%v)\n", res.OfflineErr)
	case res.Pushed:
		fmt.Printf("  remote:         pushed\n")
	}
}
