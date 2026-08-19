package webapp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
)

// Change notifications: when a project has a Webhook set, the hub posts one
// Slack-shaped message per journal write naming who changed what.
//
// This is modeled on analytics.go, deliberately and line for line, because it
// has the same one hard requirement: it must never fail a request. The POST
// runs on its own goroutine after the response is written, behind a bounded
// client, and a failure logs once and is forgotten. A synchronous POST here
// would turn a slow Slack into a 502 on a device push — and the client
// retries a 502, which re-fires the webhook.
//
// No retry queue and no coalescing window: a delivery that fails is gone. A
// notifier that guarantees delivery is a queue with durability, which is a
// different feature.

// notifyClient bounds a hung or black-holed endpoint, so a goroutine cannot
// be pinned for the life of the process.
var notifyClient = &http.Client{Timeout: 10 * time.Second}

// notifyWarnOnce logs the first delivery failure and nothing after it — a hub
// whose channel endpoint is dead would otherwise write a line per sync cycle
// per device forever, about something no operator can act on from the log.
// Once, not a bool: these run on their own goroutines.
var notifyWarnOnce sync.Once

// notifyProject posts the ops as one message to the project's webhook, or
// does nothing at all when none is set — which is every project by default,
// so an unconfigured hub makes zero new outbound requests.
//
// Call it AFTER the response is written. It returns immediately.
func (s *Server) notifyProject(id string, ops []journal.Op) {
	if s.Projects == nil || len(ops) == 0 {
		return
	}
	p, ok := s.Projects.Get(id)
	if !ok || p.Webhook == "" {
		return
	}
	text := notifyText(ops)
	if text == "" {
		return
	}
	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return
	}
	url := p.Webhook
	go func() {
		resp, err := notifyClient.Post(url, "application/json", bytes.NewReader(body))
		if err != nil {
			notifyFailed(err)
			return
		}
		resp.Body.Close()
	}()
}

// notifyLineMax caps one message. A first cycle on a fresh mount materializes
// the whole project, so the honest failure is "…and N more", not a wall.
const notifyLineMax = 20

// agentPlatforms are the platforms the agent hook stamps into Op.Note as
// "<platform> session <id>" (internal/agenthooks). Matching it is what turns
// "Dana updated x.md" into "Dana's Claude updated x.md" — the line GTM asked
// for, and the reason this posts agent activity rather than file activity.
var agentPlatforms = map[string]string{
	"claude": "Claude", "codex": "Codex", "gemini": "Gemini", "hermes": "Hermes",
}

// notifyText renders one batch as the message body. Pure, so it is
// table-testable without a server.
//
// The copy is the feature: "1 file changed" is a 2015 file-sync notification
// and says nothing a person can act on. Every line names an actor.
func notifyText(ops []journal.Op) string {
	// One line per path, latest op wins — a batch can carry several ops for
	// the same file.
	last := journal.LastOps(ops)
	paths := make([]string, 0, len(last))
	for path := range last {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	var b strings.Builder
	for i, path := range paths {
		if i > 0 {
			b.WriteByte('\n')
		}
		if i == notifyLineMax {
			fmt.Fprintf(&b, "…and %d more", len(paths)-notifyLineMax)
			break
		}
		b.WriteString(notifyLine(last[path]))
	}
	return b.String()
}

func notifyLine(op journal.Op) string {
	who := opActor(op)
	if agent := agentOf(op.Note); agent != "" {
		who += "'s " + agent
	}
	verb := "updated"
	if op.Kind == journal.KindDelete {
		verb = "deleted"
	}
	return fmt.Sprintf("%s %s %s", who, verb, op.Path)
}

// opActor is the display name for an op, the same precedence `bdrive log`
// prints and the post_sync payload carries: the signed-in account first, the
// git/OS identity as the offline fallback.
func opActor(op journal.Op) string {
	for _, s := range []string{op.UserName, op.User, op.Author} {
		if s != "" {
			return s
		}
	}
	return "Someone"
}

// agentOf reads the agent platform out of a note the agent hook stamped, and
// returns "" for anything else — including a note a member typed by hand
// (`bdrive sync --note`). The note is display-only and never proof: it names
// an agent, it does not establish one.
func agentOf(note string) string {
	fields := strings.Fields(note)
	if len(fields) < 2 || fields[1] != "session" {
		return ""
	}
	return agentPlatforms[strings.ToLower(fields[0])]
}

func notifyFailed(err error) {
	notifyWarnOnce.Do(func() {
		log.Printf("beardrive: change notification delivery failed (further failures silent): %v", err)
	})
}
