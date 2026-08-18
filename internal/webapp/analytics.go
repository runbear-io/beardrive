package webapp

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
)

// Server-side product analytics: the events the browser cannot see.
//
// The frontend (frontend/src/analytics.ts) covers everything a person clicks,
// but a device syncing through /store/* never loads a page — an agent editing
// files all day is invisible to it. That is both metrics this exists for:
// how many file changes land, and how many accounts are active at all once
// the headless ones are counted.
//
// No SDK. posthog-go would sit in go.mod and ship inside every self-hoster's
// binary, which is the exact thing the frontend avoids by loading posthog-js
// from a CDN only when a key is configured: an OSS install must not carry a
// tracker it never runs. Capture is one JSON POST, and AnalyticsConfig.Key is
// already a public write-only project token (server.go), so this needs no new
// credential and no new config seam.
//
// Telemetry never fails a request: the POST runs in its own goroutine, its
// error is dropped, and a broken analytics host is at worst a log line.

// analyticsClient bounds a hung ingestion host. Without the timeout a stalled
// POST would pin its goroutine for as long as the process lives.
var analyticsClient = &http.Client{Timeout: 10 * time.Second}

// capture sends one event to PostHog for the given account, or does nothing
// when analytics is unconfigured — which is every self-hosted hub.
//
// email is the distinct id, and it must be the same one the frontend
// identifies with (analytics.ts calls identify(cfg.me.email)), or the same
// person counts twice: once for their browser and once for their laptop.
func (s *Server) capture(email, event string, props map[string]any) {
	if s.Analytics.Key == "" || email == "" {
		return
	}
	body, err := json.Marshal(map[string]any{
		"api_key":     s.Analytics.Key,
		"event":       event,
		"distinct_id": email,
		"properties":  props,
	})
	if err != nil {
		return
	}
	endpoint := s.Analytics.Endpoint() + "/i/v0/e/"
	go func() {
		resp, err := analyticsClient.Post(endpoint, "application/json", bytes.NewReader(body))
		if err != nil {
			analyticsFailed(err)
			return
		}
		resp.Body.Close()
	}()
}

// captureChange records file changes for the account behind r.
//
// Every write path on the hub calls this — sync, upload, remove, restore — so
// "how many files changed" stays ONE number in PostHog instead of a per-route
// event set that has to be summed by hand and silently misses whichever route
// someone forgets. `source` splits agent traffic from browser traffic when
// that is the question; nothing here carries a path or a file name.
func (s *Server) captureChange(r *http.Request, source string, puts, deletes int) {
	if puts+deletes == 0 {
		return
	}
	// The account that made the request, not Op.User: an op authored offline
	// carries no signed-in account, and the pusher is who was active either way.
	s.capture(s.requestUser(r).Email, "files_changed", map[string]any{
		"puts":    puts,
		"deletes": deletes,
		"source":  source,
		"project": r.PathValue("project"),
	})
}

// analyticsFailed logs the first delivery failure and nothing after it. A hub
// that cannot reach PostHog would otherwise write a line per sync cycle per
// device, forever, about a thing no operator can act on. Once, not a bool:
// these run on their own goroutines.
var analyticsWarnOnce sync.Once

func analyticsFailed(err error) {
	analyticsWarnOnce.Do(func() {
		log.Printf("beardrive: product analytics delivery failed (further failures silent): %v", err)
	})
}

// countOps splits ops the hub has not seen before into put and delete totals.
//
// storedMax is the highest Seq already on the hub for this journal, because a
// device PUTs its WHOLE journal every cycle (journalKeepsItsOps depends on
// exactly that). Counting len(ops) would re-count the device's entire history
// every ten seconds. Seq is the device's own monotone counter and a journal
// object belongs to one device, so it is the honest discriminator.
//
// Blob PUTs are deliberately not a change event: content-addressed storage
// skips a blob it already holds, so blob writes undercount edits while ops
// are exact.
func countOps(ops []journal.Op, storedMax int64) (puts, deletes int) {
	for _, op := range ops {
		if op.Seq <= storedMax {
			continue
		}
		if op.Kind == journal.KindDelete {
			deletes++
		} else {
			puts++
		}
	}
	return puts, deletes
}
