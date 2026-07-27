package main

import (
	"os"
	"strings"
	"testing"
)

// The hub has no device list and no revoke route, and device tokens never
// expire — so neither the CLI's logout note nor the skill may claim otherwise
// (BEA-13). Drop these asserts only when a real revoke surface ships.
func TestLogoutNoteClaimsNoRevokeSurface(t *testing.T) {
	skill, err := os.ReadFile("../../plugin/skills/beardrive/SKILL.md")
	if err != nil {
		t.Fatalf("read SKILL.md: %v", err)
	}
	// Only the logout row: `bdrive share --expires` elsewhere is a real flag.
	var logoutRow string
	for _, line := range strings.Split(string(skill), "\n") {
		if strings.HasPrefix(line, "| Sign this device out |") {
			logoutRow = line
		}
	}
	if logoutRow == "" {
		t.Fatal("SKILL.md has no `Sign this device out` row — did it get renamed?")
	}
	for _, src := range []struct{ name, text string }{
		{"logoutNote", logoutNote},
		{"SKILL.md logout row", logoutRow},
	} {
		for _, claim := range []string{"device list", "expires"} {
			if strings.Contains(strings.ToLower(src.text), claim) {
				t.Errorf("%s mentions %q — no device list or token expiry exists", src.name, claim)
			}
		}
	}
}
