package webapp

import (
	"encoding/json"
	"fmt"
	"strings"
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

// One request never reads past its byte budget, even inside one file, and a
// file it could not finish is reported, not passed off as a clean negative.
// A page also stops reading at the match cap (bot review P1).
func TestHubGrepBoundsEachRead(t *testing.T) {
	h, _, c, p, _ := hiddenHub(t)
	big := strings.Repeat("filler\n", 2000) + "needle at the end\n"
	if rec := doAs(t, h, "PUT", "/api/p/"+p.ID+"/upload/content?path=big.txt", []byte(big), c["alice"]); rec.Code != 200 {
		t.Fatalf("seed: %d", rec.Code)
	}
	old := grepPageBytes
	grepPageBytes = 1024
	defer func() { grepPageBytes = old }()
	rec := doAs(t, h, "POST", "/api/p/"+p.ID+"/grep", remote.GrepRequest{Pattern: "needle", Paths: []string{"big.txt"}}, c["bob"])
	var out remote.GrepResponse
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Total != 0 || len(out.Unreadable) != 1 || out.Unreadable[0] != "big.txt" {
		t.Fatalf("a partially read file must be reported: %+v", out)
	}
	// A file that does not fit what is left of a page starts the next page.
	grepPageBytes = int64(len(big)) + 100
	doAs(t, h, "PUT", "/api/p/"+p.ID+"/upload/content?path=big2.txt", []byte(big), c["alice"])
	rec = doAs(t, h, "POST", "/api/p/"+p.ID+"/grep", remote.GrepRequest{Pattern: "needle", Paths: []string{"big.txt", "big2.txt"}}, c["bob"])
	out = remote.GrepResponse{}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Scanned != 1 || out.Total != 1 || len(out.Unreadable) != 0 {
		t.Fatalf("second file should wait for the next page: %+v", out)
	}
	grepPageBytes = old
	many := strings.Repeat("hit\n", 5000)
	doAs(t, h, "PUT", "/api/p/"+p.ID+"/upload/content?path=many.txt", []byte(many), c["alice"])
	rec = doAs(t, h, "POST", "/api/p/"+p.ID+"/grep", remote.GrepRequest{Pattern: "hit", Paths: []string{"many.txt"}, MaxMatches: 3}, c["bob"])
	out = remote.GrepResponse{}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Matches) != 3 || out.Total != 4 {
		t.Fatalf("must stop reading at the match cap (Total one past): %+v", out.Total)
	}
}
