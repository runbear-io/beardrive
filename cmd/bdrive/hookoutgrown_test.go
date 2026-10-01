package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/store"
)

// outgrownMount is device A: a runbook linking a config, both older than the
// teammate write each test then lands from device B.
func outgrownMount(t *testing.T) (string, config.Project, *store.Store) {
	t.Helper()
	folder := staleProject(t, map[string]string{
		"runbooks/deploy.md": "# Deploy\n\nTimeouts live in [config](../deploy-config.yaml).\n",
		"deploy-config.yaml": "timeout: 30s\n",
		"other.txt":          "unlinked\n",
	}, map[string]time.Duration{
		"runbooks/deploy.md": -10 * day,
		"deploy-config.yaml": -20 * day,
		"other.txt":          -20 * day,
	})
	proj, found, err := config.LoadProject(folder)
	if err != nil || !found {
		t.Fatalf("load project: %v %v", err, found)
	}
	vdir, err := config.VolumeDir(proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(vdir)
	if err != nil {
		t.Fatal(err)
	}
	return folder, proj, st
}

var devbSeq int64

// teammateWrite is device B's journal gaining a put — the op a pull brings —
// and the inbound spool recording that a cycle materialized it.
func teammateWrite(t *testing.T, st *store.Store, proj config.Project, path, body string) {
	t.Helper()
	sum, size, err := st.PutBlobBytes([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	devbSeq++
	now := time.Now().Add(-time.Minute)
	if err := st.AppendOps("devb", []journal.Op{{
		Seq: devbSeq, Lamport: 1000 + devbSeq, Time: now, Mtime: now, Device: "devb",
		Kind: journal.KindPut, Path: path, Blob: sum, Size: size, Mode: 0o644,
	}}); err != nil {
		t.Fatal(err)
	}
	seedInbound(t, proj, path)
}

func hookJSON(t *testing.T, out string) string {
	t.Helper()
	var v struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("stdout is not one JSON object: %v\n%s", err, out)
	}
	return v.HookSpecificOutput.AdditionalContext
}

// BEA-279: device B rewrites the config; device A's next turn is told the
// runbook that links it may be out of date — the doc, not just the yaml.
func TestSyncHookModeNamesOutgrownDocs(t *testing.T) {
	devbSeq = 0
	folder, proj, st := outgrownMount(t)
	teammateWrite(t, st, proj, "deploy-config.yaml", "timeout: 120s\n")

	ctx := hookJSON(t, runHook(t, folder))
	for _, want := range []string{
		"re-read before editing: `deploy-config.yaml`",
		"Docs that link a file changed since your last turn may be out of date",
		"`runbooks/deploy.md` (links `deploy-config.yaml`)",
	} {
		if !strings.Contains(ctx, want) {
			t.Errorf("context missing %q:\n%s", want, ctx)
		}
	}
}

func TestSyncHookModeOutgrownSilentForUnlinkedChange(t *testing.T) {
	devbSeq = 0
	folder, proj, st := outgrownMount(t)
	teammateWrite(t, st, proj, "other.txt", "changed\n")

	ctx := hookJSON(t, runHook(t, folder))
	if !strings.Contains(ctx, "`other.txt`") {
		t.Fatalf("the change itself should still be listed:\n%s", ctx)
	}
	if strings.Contains(ctx, "may be out of date") {
		t.Errorf("a change no doc links named a doc:\n%s", ctx)
	}
}

// The index is keyed on each doc's blob: once built, a turn whose docs did
// not change parses none of them.
func TestSyncHookModeOutgrownReparsesOnlyChangedDocs(t *testing.T) {
	devbSeq = 0
	folder, proj, st := outgrownMount(t)
	teammateWrite(t, st, proj, "other.txt", "first\n")
	docIndexParses = 0
	runHook(t, folder) // builds the index
	if docIndexParses == 0 {
		t.Fatal("the first turn parsed nothing: the counter proves nothing below")
	}
	docIndexParses = 0

	teammateWrite(t, st, proj, "deploy-config.yaml", "timeout: 120s\n")
	ctx := hookJSON(t, runHook(t, folder))
	if docIndexParses != 0 {
		t.Errorf("re-parsed %d unchanged docs", docIndexParses)
	}
	if !strings.Contains(ctx, "`runbooks/deploy.md` (links `deploy-config.yaml`)") {
		t.Errorf("the cached index lost the link:\n%s", ctx)
	}
}

// A garbage index is rebuilt, not trusted; an index that cannot be read at
// all drops only the new sentence — exit 0, the rest of the context intact.
func TestSyncHookModeOutgrownCorruptIndexIsHarmless(t *testing.T) {
	devbSeq = 0
	folder, proj, st := outgrownMount(t)
	cfg, _, err := config.LoadProject(folder)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := st.DocIndexPath(cfg.ID)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(idx, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	teammateWrite(t, st, proj, "deploy-config.yaml", "timeout: 120s\n")
	if ctx := hookJSON(t, runHook(t, folder)); !strings.Contains(ctx, "`runbooks/deploy.md`") {
		t.Errorf("a garbage index was not rebuilt:\n%s", ctx)
	}

	if err := os.Remove(idx); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(idx, 0o700); err != nil { // unreadable as a file, unwritable too
		t.Fatal(err)
	}
	teammateWrite(t, st, proj, "deploy-config.yaml", "timeout: 240s\n")
	ctx := hookJSON(t, runHook(t, folder))
	if !strings.Contains(ctx, "re-read before editing") || !strings.Contains(ctx, "[🔗](") {
		t.Errorf("the rest of the context was lost:\n%s", ctx)
	}
	if strings.Contains(ctx, "may be out of date") {
		t.Errorf("an unreadable index still produced the sentence:\n%s", ctx)
	}
}
