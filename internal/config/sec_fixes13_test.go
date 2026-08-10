package config_test

import (
	"testing"

	"github.com/runbear-io/beardrive/internal/config"
)

// AgentConfigDir is the predicate `bdrive init` refuses a mount root on. It
// answers about a single path SEGMENT, and it has to fold the same way
// ReservedDir does or the refusal is bypassed by the spelling: APFS and NTFS
// fold case, and NTFS/SMB strip trailing dots and spaces, so ~/.CLAUDE and
// ~/.claude. open the same directory the guard is there to protect.
func TestSec_AgentConfigDirFoldsTheWayTheFilesystemDoes(t *testing.T) {
	for _, name := range []string{
		".claude", ".codex", ".gemini", ".hermes",
		".CLAUDE", ".Codex", // case-folded by APFS/NTFS
		".claude.", ".claude ", ".claude..", // stripped by NTFS/SMB
	} {
		if !config.AgentConfigDir(name) {
			t.Errorf("AgentConfigDir(%q) = false; the filesystem opens it as an agent "+
				"config directory, so mounting it exposes settings.json as a top-level file", name)
		}
	}
	// Not agent config directories — and crucially not ~/.claude/skills,
	// which is the path every doc tells people to sync.
	for _, name := range []string{"", ".", "claude", "skills", ".claudex", ".claude/skills", ".bdrive", ".git"} {
		if config.AgentConfigDir(name) {
			t.Errorf("AgentConfigDir(%q) = true; it is an ordinary directory name and refusing "+
				"it would block the very folder users are told to sync", name)
		}
	}
}

// The predicate must stay derived from agentHookConfigs rather than from a
// literal list: every directory that keys a reserved hook config is one whose
// files lose their directory segment at a mount root.
func TestSec_AgentConfigDirCoversEveryReservedHookDir(t *testing.T) {
	for dir, file := range map[string]string{
		".claude": "settings.json",
		".codex":  "hooks.json",
		".gemini": "settings.json",
		".hermes": "config.yaml",
	} {
		if !config.ReservedPath(dir + "/" + file) {
			t.Fatalf("fixture: %s/%s should be reserved below a mount root", dir, file)
		}
		// The same file at a mount root has no directory segment left...
		if config.ReservedPath(file) {
			t.Fatalf("fixture: %q is unexpectedly reserved on its own", file)
		}
		// ...so the mount root itself is what has to be refused.
		if !config.AgentConfigDir(dir) {
			t.Errorf("AgentConfigDir(%q) = false while %s/%s is reserved: at that mount root "+
				"%s becomes an ordinary top-level file and syncs to the whole team",
				dir, dir, file, file)
		}
	}
}
