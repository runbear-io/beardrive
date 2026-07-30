package main

import (
	"strings"
	"testing"
)

// The hub has no device list and no revoke route, and device tokens never
// expire — so the CLI's logout note may not claim otherwise (BEA-13). Drop
// this assert only when a real revoke surface ships.
func TestLogoutNoteClaimsNoRevokeSurface(t *testing.T) {
	for _, claim := range []string{"device list", "expires"} {
		if strings.Contains(strings.ToLower(logoutNote), claim) {
			t.Errorf("logoutNote mentions %q — no device list or token expiry exists", claim)
		}
	}
}
