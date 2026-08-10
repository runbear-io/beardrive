package webapp

import (
	"bytes"
	"regexp"
	"sort"
)

// Minting a share link is the one place on the hub where a member turns private
// bytes into a public URL, so it is the one place worth reading the bytes
// first. The check is deliberately narrow: six anchored rules over the first
// 1 MiB, at mint time only.
//
// It says nothing about the file tomorrow. A link serves the file's LATEST
// content forever (see the package comment in shares.go), so every string a
// user sees says the file was checked *at the moment you shared it* — never
// that the file is clean.

// secretScanLimit is how much of a file the share gate reads. The boundary is
// a decision, not an accident: a key past the first MiB mints silently, which
// is asserted in shares_test.go so nobody "fixes" it by accident.
const secretScanLimit = 1 << 20

// secretFinding is one credential-shaped string: which rule fired, and where.
// Never the matched text — see scanSecrets.
type secretFinding struct {
	Rule string `json:"rule"`
	Line int    `json:"line"`
}

var secretRules = []struct {
	id string
	re *regexp.Regexp
}{
	{"aws_access_key_id", regexp.MustCompile(`AKIA[0-9A-Z]{16}`)},
	// The bodies below are what keep the prefixes off prose: a bare `sk-` in a
	// sentence is not a key. If one still fires on real docs, tighten the body
	// rather than dropping the rule — `--force` and Share anyway are the
	// escape hatch, which is why they ship in the same change.
	{"openai_api_key", regexp.MustCompile(`sk-[A-Za-z0-9_-]{20,}`)},
	{"github_pat", regexp.MustCompile(`ghp_[A-Za-z0-9]{36}`)},
	{"slack_token", regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`)},
	{"private_key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"gitlab_pat", regexp.MustCompile(`glpat-[A-Za-z0-9_-]{20,}`)},
}

// scanSecrets reports credential-shaped strings in buf, as rule ids and line
// numbers ONLY. The matched text must never reach a response body, a log line,
// or a metric label — the same argument reads.go:28-40 makes for actor
// identity, and a 409 body is the easiest place in the codebase to leak it.
//
// Byte-oriented on purpose: a bufio.Scanner over a 1 MiB minified file with no
// newline blows its 64 KiB token limit and returns nothing at all, which is a
// check that silently passes everything.
func scanSecrets(buf []byte) []secretFinding {
	seen := map[secretFinding]bool{}
	var out []secretFinding
	for _, rule := range secretRules {
		for _, m := range rule.re.FindAllIndex(buf, -1) {
			f := secretFinding{Rule: rule.id, Line: bytes.Count(buf[:m[0]], []byte("\n")) + 1}
			if !seen[f] {
				seen[f] = true
				out = append(out, f)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Line != out[j].Line {
			return out[i].Line < out[j].Line
		}
		return out[i].Rule < out[j].Rule
	})
	return out
}
