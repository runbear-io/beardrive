package webapp

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/runbear-io/beardrive/internal/remote"
)

// The lazy-device grep: it pages, answers hidden paths as missing (never as a
// match), and counts every match even past the page's cap.
func TestHubGrepPagesAndHonorsFolderRules(t *testing.T) {
	h, _, c, p, _ := hiddenHub(t)
	paths := []string{"vault/secret.md", "notes/open.md", "nope.md"}
	for i := 0; i < 3; i++ {
		path := fmt.Sprintf("many/f%d.md", i)
		body := []byte("secret line\nsecret line\n")
		if rec := doAs(t, h, "PUT", "/api/p/"+p.ID+"/upload/content?path="+path, body, c["alice"]); rec.Code != 200 {
			t.Fatalf("seed: %d", rec.Code)
		}
		paths = append(paths, path)
	}
	grep := func(who string, req remote.GrepRequest) remote.GrepResponse {
		rec := doAs(t, h, "POST", "/api/p/"+p.ID+"/grep", req, c[who])
		if rec.Code != 200 {
			t.Fatalf("grep as %s: %d %s", who, rec.Code, rec.Body)
		}
		var out remote.GrepResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	out := grep("bob", remote.GrepRequest{Pattern: "(?i)secret", Paths: paths})
	if out.Scanned != len(paths) || out.Total != 6 || len(out.Files) != 3 {
		t.Fatalf("bob: %+v", out)
	}
	if len(out.Missing) != 2 || out.Missing[0] != "vault/secret.md" {
		t.Fatalf("hidden path not reported missing: %+v", out.Missing)
	}
	for _, m := range out.Matches {
		if m.Path == "vault/secret.md" {
			t.Fatal("hidden file matched")
		}
	}
	if out := grep("carol", remote.GrepRequest{Pattern: "the secret", Paths: paths[:1]}); out.Total != 1 {
		t.Fatalf("carol is granted the folder: %+v", out)
	}
	// A page stops at the match cap, and the caller resumes from Scanned.
	out = grep("bob", remote.GrepRequest{Pattern: "secret", Paths: paths[3:], MaxMatches: 2})
	if out.Scanned != 1 || len(out.Matches) != 2 {
		t.Fatalf("page 1: %+v", out)
	}
	out = grep("bob", remote.GrepRequest{Pattern: "secret", Paths: paths[3+out.Scanned:], MaxMatches: 2})
	if out.Scanned != 1 || len(out.Matches) != 2 {
		t.Fatalf("page 2: %+v", out)
	}
}
