package webapp

import (
	"context"
	"fmt"
	"strings"

	"github.com/runbear-io/beardrive/internal/templates"
)

// seedTemplate writes a starting structure into a freshly created project,
// journaled under this server's own device exactly like a browser upload —
// so the files are simply there when the first device syncs, and a user who
// picked PARA in a browser sees PARA in the browser.
//
// A path that already exists is skipped rather than overwritten: seeding is
// only ever reached on a just-created project, but the same rule in
// templates.WriteTo is what makes a double-seed a no-op, and this is the
// other half of it.
func (s *Server) seedTemplate(ctx context.Context, projectID string, t templates.Template, who User) error {
	v, err := s.projectVolume(projectID)
	if err != nil {
		return err
	}
	up := v.uploader()
	if up == nil {
		return fmt.Errorf("this project's storage is read-only")
	}
	var total int64
	for _, f := range t.Files {
		total += int64(len(f.Content))
	}
	org := s.orgOf(projectID)
	// + reservedBytes(org): every other write door counts the presigned grants
	// that are outstanding but not yet stored (see reserve.go). Passing the bare
	// total let a template-seeded project push an org past its cap while a grant
	// was in flight.
	if err := s.quota().CheckWrite(org, total+s.reservedBytes(org)); err != nil {
		return err
	}

	existing := map[string]bool{}
	if snap, err := v.snapshot(ctx); err == nil {
		for p := range snap.files {
			existing[p] = true
		}
	}
	for _, f := range t.Files {
		// The same guard every other write door on the hub applies before
		// anything is journaled. This one called up.Upload directly, so a
		// template path was the one destination nothing checked — and a hub
		// that journals `../../etc/cron.d/pwned` or `.git/hooks/pre-commit`
		// has already handed it to every device that syncs.
		clean, err := cleanUploadPath(f.Path)
		if err != nil {
			return fmt.Errorf("template %s: %w", t.Name, err)
		}
		if existing[clean] {
			continue
		}
		if err := up.Upload(ctx, clean, strings.NewReader(f.Content), int64(len(f.Content)), who); err != nil {
			return fmt.Errorf("%s: %w", clean, err)
		}
	}
	s.quota().RecordUsage(org, total)
	v.invalidate()
	return nil
}
