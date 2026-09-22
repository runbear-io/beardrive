package webapp

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
)

/* The change relay over Postgres LISTEN/NOTIFY.

   Chosen because it adds NO INFRASTRUCTURE: the managed hub already runs
   Cloud SQL for its metadata, so cross-instance fan-out costs a channel
   rather than a Redis instance and another thing to operate. Its envelope is
   well under what a hub needs — the published guidance is roughly under 10k
   events a second and a handful of listener processes, against a hub whose
   entire measured traffic is single-digit requests a second and whose
   instance count is capped at one today. Redis becomes the right answer past
   that, and eventRelay is the interface that makes it a swap.

   Two connections on purpose. LISTEN must own its connection — it sits in a
   blocking read for the life of the process — so publishing down the same one
   would serialise every write behind the listener. NOTIFY therefore goes
   through the ordinary pool, which is already there.

   The payload is sent with pg_notify($1,$2) rather than a built NOTIFY
   statement. NOTIFY takes a literal, so the payload would have to be escaped
   into SQL text, and the thing being escaped is a file path chosen by whoever
   wrote the file. That is a needless place to be careful; a bound parameter
   is careful by construction. */

// pgNotifyChannel is the one channel every hub process listens on. One rather
// than one-per-project: Postgres delivers a NOTIFY to every LISTENer and the
// project is in the payload, so per-project channels would mean re-LISTENing
// whenever a project appeared and buy nothing — a hub process is interested in
// every project it might be holding a stream for, which is all of them.
const pgNotifyChannel = "bdrive_events"

// pgNotifyMax is Postgres's own payload ceiling (8000 bytes), less room for
// the origin and project prefix. A frame past maxRelayFrame has already been
// turned into a resync by the time it arrives here; this is the belt to that
// pair of braces, because the ceiling belongs to this transport and a future
// one may be tighter.
const pgNotifyMax = 7900

type pgRelay struct {
	db     *sql.DB
	dsn    string
	origin relayOrigin

	ctx    context.Context
	cancel context.CancelFunc
}

// NewPostgresRelay carries change frames between hub processes over
// LISTEN/NOTIFY. db is the existing pool (used for NOTIFY); dsn opens the
// dedicated connection LISTEN needs.
func NewPostgresRelay(db *sql.DB, dsn string) (*pgRelay, error) {
	if db == nil || dsn == "" {
		return nil, fmt.Errorf("postgres relay needs a pool and a dsn")
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &pgRelay{db: db, dsn: dsn, origin: newRelayOrigin(), ctx: ctx, cancel: cancel}, nil
}

func (r *pgRelay) publish(project string, frame []byte) error {
	if len(frame) > maxRelayFrame {
		frame = resyncFrame()
	}
	payload := encodeRelay(r.origin, project, frame)
	if len(payload) > pgNotifyMax {
		payload = encodeRelay(r.origin, project, resyncFrame())
	}
	// Bounded: publish is on the sync push path and must not hold a write open
	// while a database decides how it feels.
	ctx, cancel := context.WithTimeout(r.ctx, 5*time.Second)
	defer cancel()
	_, err := r.db.ExecContext(ctx, "SELECT pg_notify($1, $2)", pgNotifyChannel, payload)
	return err
}

func (r *pgRelay) start(deliver func(project string, frame []byte)) error {
	// Connect once synchronously so a misconfigured DSN is an error the caller
	// can log at startup rather than a silence that looks like "no writes yet".
	conn, err := pgx.Connect(r.ctx, r.dsn)
	if err != nil {
		return err
	}
	if _, err := conn.Exec(r.ctx, "LISTEN "+pgNotifyChannel); err != nil {
		conn.Close(r.ctx)
		return err
	}
	go r.listen(conn, deliver)
	return nil
}

/* listen delivers notifications until the relay is closed, reconnecting when
   the connection dies.

   Reconnecting matters more than it looks. A dropped listener is SILENT — the
   hub keeps serving, keeps publishing, and simply stops hearing other
   processes, so the symptom is "some people's tabs are stale" rather than an
   error anybody sees. Cloud SQL will close an idle connection eventually, and
   an idle listener is the normal state of a quiet hub. */
func (r *pgRelay) listen(conn *pgx.Conn, deliver func(project string, frame []byte)) {
	backoff := time.Second
	for {
		n, err := conn.WaitForNotification(r.ctx)
		if err != nil {
			if r.ctx.Err() != nil {
				conn.Close(context.Background())
				return // closed on purpose
			}
			log.Printf("beardrive: change relay lost its connection, reconnecting: %v", err)
			conn.Close(context.Background())
			conn = r.redial(&backoff)
			if conn == nil {
				return
			}
			continue
		}
		backoff = time.Second
		if project, frame, ok := decodeRelay(r.origin, n.Payload); ok {
			deliver(project, frame)
		}
	}
}

// redial retries until it succeeds or the relay closes. It never gives up on
// its own: a hub that stopped listening does not recover by itself, and the
// alternative to retrying is a process that is quietly half-connected for the
// rest of its life.
func (r *pgRelay) redial(backoff *time.Duration) *pgx.Conn {
	for {
		select {
		case <-r.ctx.Done():
			return nil
		case <-time.After(*backoff):
		}
		if *backoff < 30*time.Second {
			*backoff *= 2
		}
		conn, err := pgx.Connect(r.ctx, r.dsn)
		if err != nil {
			continue
		}
		if _, err := conn.Exec(r.ctx, "LISTEN "+pgNotifyChannel); err != nil {
			conn.Close(context.Background())
			continue
		}
		log.Printf("beardrive: change relay reconnected")
		return conn
	}
}

func (r *pgRelay) Close() error {
	r.cancel()
	return nil
}
