package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/mattn/go-isatty"

	"github.com/runbear-io/beardrive/internal/syncer"
)

// progressReporter returns a syncer.OnProgress callback that shows upload
// progress: an in-place bar on a TTY, periodic percentage lines otherwise.
// It's safe to call concurrently from upload workers, and renders are
// throttled so many small blobs don't thrash the terminal. It stays silent
// when there's nothing to upload.
func progressReporter() func(syncer.Progress) {
	tty := isatty.IsTerminal(os.Stderr.Fd())
	var mu sync.Mutex
	var lastDraw time.Time
	lastPct := -1
	return func(p syncer.Progress) {
		if p.Total == 0 {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		final := p.Done >= p.Total
		pct := p.Done * 100 / p.Total
		if tty {
			if !final && time.Since(lastDraw) < 100*time.Millisecond {
				return
			}
			lastDraw = time.Now()
			const w = 24
			filled := pct * w / 100
			bar := strings.Repeat("█", filled) + strings.Repeat("░", w-filled)
			fmt.Fprintf(os.Stderr, "\r  uploading [%s] %3d%%  %d/%d files  %s / %s",
				bar, pct, p.Done, p.Total, humanBytes(p.Bytes), humanBytes(p.ToBytes))
			if final {
				fmt.Fprintln(os.Stderr)
			}
			return
		}
		// Non-TTY: announce the total once, then a line every ~10% and at the end.
		if lastPct < 0 {
			fmt.Fprintf(os.Stderr, "  uploading %d files (%s)\n", p.Total, humanBytes(p.ToBytes))
		}
		if final || pct/10 > lastPct/10 {
			fmt.Fprintf(os.Stderr, "  %d%%  %d/%d files  %s / %s\n",
				pct, p.Done, p.Total, humanBytes(p.Bytes), humanBytes(p.ToBytes))
		}
		lastPct = pct
	}
}
