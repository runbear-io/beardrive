package webapp

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

// Round 6, from the coverage audit: two ways a run can quietly cover less than
// the scoreboard claims, neither of which was visible from inside the repo.
//
//   - `-short` removes every test built on seccfgRealHub and newCLIEnv, which
//     is where rows 12 and 2 are actually decided (a real `bdrive serve -c`
//     with a real DSN, SMTP password and storage credential; the real binary
//     with an isolated HOME). Nothing in the tree said so, so "go test -short
//     ./..." looked like a green security run.
//   - the Postgres arm of row 14 degrades to a t.Log, which Go prints only on
//     failure or under -v. "Verified on a real Postgres 16" was therefore not
//     reproducible from the repo: a normal run says nothing at all.
//
// This test is the record. It is not an attack test and closes no row.
func TestSec_Suite_RunModeIsVisible(t *testing.T) {
	if testing.Short() {
		t.Fatal("-short is not a security run: it skips seccfgRealHub and newCLIEnv, and with them " +
			"TestSec_Config_NoServedConfigurationReachesTheAdminEscape, " +
			"TestSec_Config_OrgMigrationLeavesNoProjectWorldWritable, " +
			"TestSec_Leak_RealConfigPathKeepsSecretsOffTheWire, " +
			"TestSec_Password_LoginTimingDoesNotEnumerateAccounts and the whole cli_e2e suite. " +
			"Run `go test ./...` with no -short")
	}
	// This is the SINGLE place the run reports the gap; dsnGatedTests names what
	// goes unmeasured.
	if os.Getenv("BDRIVE_TEST_POSTGRES") == "" {
		secrunNotify("NOTE: BDRIVE_TEST_POSTGRES is unset — row 14 ran on file+sqlite only; " +
			"the Postgres backend is UNTESTED in this run, and these tests SKIPPED " +
			"rather than passed: " + strings.Join(dsnGatedTests, ", ") +
			" (e.g. docker run -e POSTGRES_PASSWORD=x -p 5432:5432 postgres:16, then " +
			"BDRIVE_TEST_POSTGRES='postgres://postgres:x@localhost:5432/postgres?sslmode=disable')")
	}
}

// secrunNotify writes a run-mode note where `go test` cannot swallow it.
//
// Round 10 moved this note from t.Log to os.Stderr precisely because t.Log is
// invisible without -v. That was not enough, and round 11 measured it: `go test`
// BUFFERS a package's output and discards it on success without -v, stderr
// included. So the note appeared only when the suite was already red — the
// mechanism designed to make a silent gap loud was itself only audible during a
// failure, which is the same shape as the hole it exists to prevent.
//
// The controlling terminal survives that buffering, so an interactive run always
// sees it. ponytail: on CI there is no tty and the stderr copy is the fallback —
// CI runs -v or reads the JSON stream, where it is visible again. If a CI setup
// ever runs neither, this needs a real reporting channel, not a louder print.
func secrunNotify(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	if tty, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0); err == nil {
		fmt.Fprintln(tty, msg)
		tty.Close()
	}
}

// dsnGatedTests are the tests that measure NOTHING without a Postgres DSN and
// say so by skipping. Round 11 chose skip-plus-a-loud-note over a permanently
// red default suite: a red nobody can fix without Docker is a red everybody
// learns to scroll past, and the next real regression hides behind it. The
// choice only holds while the note is true, which is what the check below is.
var dsnGatedTests = []string{
	"TestSec_DB_EveryBackendAgreesWhichTextIsStorable",
	"TestSec_DB_ASchemaRoundTripDoesNotWidenAProjectDefault",
}

// TestSec_Suite_DSNGatedTestsStillSkipLoudly is the other half of the bargain.
// The danger of a skip is that it becomes invisible: someone deletes the guard
// (and the test then passes with two arms, measuring nothing about agreement),
// or deletes the test, and the note above keeps promising coverage that is no
// longer merely unmeasured but gone. So the reporter checks its own claim
// against the source: every name it prints must exist and must still refuse to
// run without the DSN.
func TestSec_Suite_DSNGatedTestsStillSkipLoudly(t *testing.T) {
	src, err := os.ReadFile("sec_pg_test.go")
	if err != nil {
		t.Fatalf("the DSN-gated tests live in sec_pg_test.go: %v", err)
	}
	for _, name := range dsnGatedTests {
		body, ok := secrunBody(string(src), name)
		if !ok {
			t.Errorf("%s is named in the run-mode note but no longer exists in sec_pg_test.go — "+
				"the note promises a gap is merely unmeasured while the measurement is gone", name)
			continue
		}
		if !secrunSkips(string(src), body) {
			t.Errorf("%s no longer skips when the DSN is absent: without a skip it either fails "+
				"for everyone without Docker, or — worse — passes having measured nothing. "+
				"Round 11's choice was skip PLUS the loud note; keep both or change both", name)
		}
	}
}

// secrunSkips reports whether a test body refuses to run without the DSN —
// directly, or through a same-file helper that does the skipping for it
// (secpgSQL is one; the fixture builders are where the guard naturally lives).
// One level of indirection is enough: a helper that itself delegates would be
// a fixture chain deep enough to be its own smell.
func secrunSkips(src, body string) bool {
	if strings.Contains(body, "t.Skip") {
		return true
	}
	for _, m := range regexp.MustCompile(`func (secpg\w+)\(`).FindAllStringSubmatch(src, -1) {
		if !strings.Contains(body, m[1]+"(") {
			continue
		}
		if h, ok := secrunFuncBody(src, m[1]); ok && strings.Contains(h, "t.Skip") {
			return true
		}
	}
	return false
}

// secrunBody returns one top-level test function's body by brace matching.
func secrunBody(src, name string) (string, bool) {
	return secrunSpan(src, strings.Index(src, "func "+name+"(t *testing.T) {"))
}

// secrunFuncBody is the same, for a helper of any signature.
func secrunFuncBody(src, name string) (string, bool) {
	return secrunSpan(src, strings.Index(src, "func "+name+"("))
}

// secrunSpan returns the braced body starting at a func declaration at i.
func secrunSpan(src string, i int) (string, bool) {
	if i < 0 {
		return "", false
	}
	i = strings.Index(src[i:], "{") + i + 1
	depth := 1
	for j := i; j < len(src); j++ {
		switch src[j] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[i:j], true
			}
		}
	}
	return "", false
}
