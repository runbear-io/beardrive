package syncer

import (
	"encoding/json"
	"io"
	"log"
	"os"
	"os/exec"

	"github.com/runbear-io/beardrive/internal/config"
)

// postSyncPayload is what the hook command reads on stdin.
type postSyncPayload struct {
	Project string            `json:"project"`
	Folder  string            `json:"folder"`
	Changed []postSyncChanged `json:"changed"`
}

type postSyncChanged struct {
	Path string `json:"path"`
	Op   string `json:"op"` // "write" | "delete"
	// User is who committed the change, resolved UserName -> User -> Author
	// the way `bdrive log` prints it; Note is that op's note, which the agent
	// hook stamps "<platform> session <id>" so a recipe can render
	// "Dana's Claude updated <path>". Both omitempty: a hook written against
	// the older path/op payload is unaffected.
	User string `json:"user,omitempty"`
	Note string `json:"note,omitempty"`
}

// firePostSync runs the folder's post_sync command once, for a cycle that
// materialized at least one path on a peer's behalf. It is called from Cycle
// AFTER the volume flock is released, so a hook that runs a bdrive command
// completes instead of deadlocking.
//
// Nothing it does can fail the cycle: a missing command, a non-zero exit, or a
// hook that never returns is logged (daemon.log for the daemon, stderr for a
// one-shot CLI cycle) and forgotten.
func (s *Session) firePostSync(res *Result) {
	if len(res.Inbound) == 0 {
		return // inbound only: a scan-and-push cycle fires nothing
	}
	proj, ok, err := config.LoadProject(s.Folder)
	if err != nil || !ok || proj.PostSync == "" {
		return // off unless configured
	}

	payload := postSyncPayload{Project: s.mountID(), Folder: s.Folder}
	for _, e := range res.Inbound {
		op := "write"
		if e.Deleted {
			op = "delete"
		}
		payload.Changed = append(payload.Changed, postSyncChanged{Path: e.Path, Op: op, User: e.User, Note: e.Note})
	}
	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("post_sync: %v", err)
		return
	}

	// stdin is an unlinked temp FILE, not a bytes.Reader: os/exec feeds a
	// non-*os.File stdin through a pipe served by a goroutine in the PARENT,
	// and a one-shot `bdrive sync` exits before the child has read it —
	// delivering truncated JSON. A file descriptor is handed straight to the
	// child, so the parent may exit immediately.
	f, err := os.CreateTemp("", "bdrive-postsync-")
	if err != nil {
		log.Printf("post_sync: %v", err)
		return
	}
	os.Remove(f.Name()) // the fd is the only handle left
	defer f.Close()
	if _, err := f.Write(body); err != nil {
		log.Printf("post_sync: %v", err)
		return
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		log.Printf("post_sync: %v", err)
		return
	}

	// ponytail: no single-flight guard and no timeout — a hook that hangs
	// while inbound keeps arriving accumulates children. The cycle is
	// unaffected either way; add coalescing here if a real hook turns out to
	// be slow.
	cmd := exec.Command("sh", "-c", proj.PostSync)
	cmd.Dir, cmd.Stdin = s.Folder, f
	if err := cmd.Start(); err != nil {
		log.Printf("post_sync: %v", err)
		return
	}
	go func() {
		if err := cmd.Wait(); err != nil {
			log.Printf("post_sync exited: %v", err)
		}
	}()
}
