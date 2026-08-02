package webapp

import (
	"fmt"
	"os"
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
	// Stderr, not t.Log: t.Log is invisible without -v, which is exactly how
	// "postgres UNTESTED" went unnoticed for two rounds.
	if os.Getenv("BDRIVE_TEST_POSTGRES") == "" {
		fmt.Fprintln(os.Stderr,
			"NOTE: BDRIVE_TEST_POSTGRES is unset — row 14 ran on file+sqlite only; "+
				"the Postgres backend is UNTESTED in this run "+
				"(e.g. docker run -e POSTGRES_PASSWORD=x -p 5432:5432 postgres:16, then "+
				"BDRIVE_TEST_POSTGRES='postgres://postgres:x@localhost:5432/postgres?sslmode=disable')")
	}
}
