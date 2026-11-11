package webapp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/reearth/ygo/cluster"
	redisrelay "github.com/reearth/ygo/cluster/redis"
)

/* Co-editing across hub processes.

   Each process holds its own copy of a room's document; the relay carries
   every update between them, so two people on two instances edit one
   document. ygo ships the relay (cluster/redis); what is ours is the
   bootstrap it does not do — a process that opens a room mid-session
   builds it from the state the writer last stored (ydoc/<sha>, at most
   one snapshot interval old) and the Streams tier replays the retained
   window on top, which is what makes a late joiner converge rather than
   hold a second history of the same text.

   Streams, never pub/sub: pub/sub is at-most-once, and one lost update
   parks every later edit from that client on the receiving process until
   the room is reloaded. */

// CollabConfig is how co-editing crosses processes.
type CollabConfig struct {
	// Redis is the Redis that relays document updates between hub processes:
	// "host:port" or a redis:// URL. Empty means one process holds each
	// document and another refuses the room, so the browser's retry lands
	// it on the one that does (lease.go). Required once a hub runs as more
	// than one process.
	Redis string
}

// How long the relay keeps a room's updates. The retained window is what a
// process opening the room mid-session replays on top of the last stored
// state (ydoc/<sha>), so it has to outlast the gap between two snapshots
// that actually landed — a failed write retried with backoff, a lease holder
// that died and must expire (leaseTTL) before another process writes, an
// idle room (collabRoomIdleTimeout). ygo's 60s default is shorter than
// several of those together; a gap past the window is typing no later
// joiner can see. Vars so a test can shorten them.
var (
	collabStreamRetention       = 30 * time.Minute
	collabStreamMaxLen    int64 = 32768
)

// ConnectCollab attaches the configured relay. Called at startup, before
// any room exists, so a Redis that cannot be reached fails the start
// instead of silently leaving every process alone with its own copy.
func (s *Server) ConnectCollab(ctx context.Context) error {
	if s.Collab.Redis == "" {
		return nil
	}
	addr := s.Collab.Redis
	if !strings.Contains(addr, "://") {
		addr = "redis://" + addr
	}
	opt, err := goredis.ParseURL(addr)
	if err != nil {
		return errors.New("collab redis: invalid URL")
	}
	client := goredis.NewClient(opt)
	if err := client.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("collab redis %s: %w", opt.Addr, err)
	}
	relay, err := redisrelay.New(client, redisrelay.Config{
		Transport:       redisrelay.Streams,
		ChannelPrefix:   "bdrive:collab:",
		StreamPrefix:    "bdrive:collab:stream:",
		StreamRetention: collabStreamRetention,
		StreamMaxLen:    collabStreamMaxLen,
	})
	if err != nil {
		return fmt.Errorf("collab redis: %w", err)
	}
	return s.useCollabRelay(relay)
}

func (s *Server) useCollabRelay(r cluster.Relay) error {
	if err := s.ydocs().AttachRelay(r); err != nil {
		return err
	}
	s.collabRelay = r
	return nil
}
