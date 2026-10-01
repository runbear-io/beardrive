package webapp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

type renderOutgrown struct {
	Outgrown []outgrownRef `json:"outgrown"`
}

func outgrownOf(t *testing.T, srv *Server, h http.Handler, url string) ([]outgrownRef, string) {
	t.Helper()
	rec := getAs(t, srv, h, url)
	if rec.Code != 200 {
		t.Fatalf("render %s: %d %s", url, rec.Code, rec.Body)
	}
	var doc renderOutgrown
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Outgrown, rec.Body.String()
}

// TestRenderOutgrown: a doc whose linked file was written after it carries
// `outgrown` naming that file (BEA-279); rewriting the doc clears it; older
// refs and past versions never carry the field.
func TestRenderOutgrown(t *testing.T) {
	srv, p, _, f, h := shareHub(t)
	base := "/api/p/" + p.ID + "/render?path="
	now := time.Now().UTC()
	runbook := "# Runbook\n\nSee [config](deploy-config.yaml) and [[notes]].\n"
	f.putAt("dev1", "runbook.md", runbook, now.Add(-72*time.Hour))
	f.putAt("dev1", "notes.md", "old notes\n", now.Add(-96*time.Hour))
	f.putAt("dev2", "deploy-config.yaml", "timeout: 120s\n", now.Add(-time.Hour))

	got, _ := outgrownOf(t, srv, h, base+"runbook.md")
	if len(got) != 1 || got[0].Path != "deploy-config.yaml" {
		t.Fatalf("outgrown = %+v, want the yaml only", got)
	}
	if !got[0].Time.Equal(now.Add(-time.Hour)) || got[0].Gap < 70*3600 {
		t.Fatalf("yaml dated %v gap %ds", got[0].Time, got[0].Gap)
	}

	// A past version is outgrown by definition: never flagged.
	sum := sha256.Sum256([]byte(runbook))
	if _, body := outgrownOf(t, srv, h, base+"runbook.md&sha="+hex.EncodeToString(sum[:])); strings.Contains(body, "outgrown") {
		t.Fatalf("past version carries outgrown: %s", body)
	}

	// The doc rewritten after the yaml: clean, and the field is omitted.
	f.putAt("dev1", "runbook.md", runbook+"\nupdated.\n", now)
	if _, body := outgrownOf(t, srv, h, base+"runbook.md"); strings.Contains(body, "outgrown") {
		t.Fatalf("rewritten doc still outgrown: %s", body)
	}
}
