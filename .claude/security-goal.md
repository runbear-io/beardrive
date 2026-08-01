# Security validation — shared goal

Both agents (`hacker`, `ciso`) read this file first, every round. It is the
only definition of "done". Nothing else — not a score, not an opinion, not a
paragraph of reassurance — ends the loop.

## Mission

Find and close every way one account can reach data or capability it was not
granted on a BearDrive hub, and every way an unauthenticated stranger can
reach anything beyond a valid share link.

## What counts as done

The loop ends when **both** hold:

1. Every row of the scoreboard is `clean` or `fixed` — each backed by a named
   Go test in the repo.
2. Two consecutive hacker rounds produce **zero** new failing tests.

There is no numeric score and no "8/10 is good enough". A row is closed by a
test or it is open.

## The one rule

**A finding does not exist until it is a Go test that fails on the current
tree.** Write it, run it, paste the failure output. Anything without a
reproducer goes in `SUSPICIONS.md` — it is a lead for the next round, not a
finding, and it never closes or opens a scoreboard row.

Symmetrically: **a fix does not exist until that same test passes** and
`go test ./...` is green. Tests are never deleted, skipped, or weakened to
make a round end.

Tests live in `internal/webapp/` (or `internal/syncer/` for sync-layer
attacks), named `TestSec_<Boundary>_<Attack>` — e.g.
`TestSec_Perms_ReadOnlyMemberCanPush`. Existing harnesses to build on:
`cli_e2e_test.go` (real binary, isolated `HOME`), `gating_test.go`,
`perms_test.go`, `store_test.go`, `shares_test.go`, `db_conformance_test.go`.

## Scoreboard

Every row starts `untested`. States: `untested` → `exploit (test name)` →
`fixed (test name)` , or `untested` → `clean (test name)`.
`clean` still needs a test — one that asserts the attack is refused.

| # | Boundary | State after round 1 | Attacks that must be tried |
|---|----------|---------------------|----------------------------|
| 1 | Auth gate (`auth.go:authGate`) | **clean** — `TestSec_AuthGate_AnonymousPathTricksCannotReadAPI`, `…CannotWrite`, `…ConfigLeaksNothingToAnonymous`. Only the open-path half. **Forged/tampered credential never exercised.** | reach any `/api/**` with no/expired/forged credential; abuse the `!HasPrefix("/api/")` open-path rule to hit something sensitive; path tricks (`//api/`, `/api/../`, encoded) that route to a handler but read as "open" |
| 2 | Per-project permission choke point (`perms.go:projectPerm/requirePerm`, `server.go` route table) | **fixed** — `TestSec_Perms_RemovedOrgMemberLosesProjectAccess` (grant outlived org membership), `TestSec_Perms_OrgLessProjectIsNotAdminForEveryone` (fail-open `p.Org == ""` / unknown id). **clean** — `…ReadOnlyMemberCannotWrite`, `…WriteMemberCannotAdmin`, `…NoneMemberReachesNothing`, `…CorruptGrantFailsClosed`. `s.Dir == nil \|\| s.Auth == nil → PermAdmin` **still open, deliberately deferred** (see below), untested. | `read` member performing any `PermWrite` action; `write` member performing `PermAdmin`; `none`/non-member reaching a project; the fail-open escapes (`s.Dir == nil`, `p.Org == ""`) reachable on a configured hub |
| 3 | Routes **outside** `proj()` | **fixed** — `TestSec_Row3_OrgSharesLeaksDeniedProject`, `TestSec_Row3_ExpiredShareRevokableByOutsider`. **clean** — `…ShareMutationByOutsider`, `…PermissionRoutes`, `…ProjectLifecycleRoutes`, `…OrgRoutes`, `…InviteAccept`, `…AdminRoutes`. | each one, exercised by a non-member, a read-only member, and a non-owner — these bypass the wrapper and check themselves, so each needs its own test |
| 4 | Cross-org isolation (`orgs.go`, `projects.go`, `directory.go`) | **clean** — `TestSec_CrossOrg_ProjectRoutesRefuseOutsider`, `TestSec_CrossOrg_OrgRoutesRefuseOutsiderAndNonOwner`. | project id from org B against every route; `/api/projects` and `/api/orgs` leaking names/ids of orgs you're not in; org rename/member routes on someone else's org |
| 5 | Sync proxy `/store/*` (`store.go`, `remote/http.go`) | **fixed** — `TestSec_Store_ForeignDeviceJournalWrite` (one-writer invariant), `…BlobContentMustMatchItsKey`, `…QuotaHonorsUnsizedPut`. **clean** — `…KeyEscapesRefused`. Per-level access to `/store/*` covered by row 2's tests, not by the store tests (they ran on an auth-less hub). **Device identity is still client-asserted** — see gaps. | write a **different device's** journal key (breaks the one-writer invariant); key traversal escaping `<root>/<project-id>/`; read another project's blob by sha; `store/sign` minting a URL outside the project prefix or for a journal key (journals must never be presigned) |
| 6 | Upload (`upload.go`) | **fixed** — `TestSec_Upload_ReservedDirsRefused` + `TestSec_Sync_PeerJournalCannotMaterializeReservedPaths` (`internal/syncer`, the materialize half), `TestSec_Upload_QuotaUsesRealSize`. **clean** — `…TargetStaysInProject`. | presigned target outside the project prefix; `upload/commit` journaling a path with `..` or an absolute path; committing content the caller never uploaded; quota (`quota.go`) bypass on any write path |
| 7 | Share links (`shares.go`, `ratelimit.go`, `server.go:handleShared`) | **fixed** — `TestSec_Share_OrgAuditLeaksDeniedProjectTokens`, `…RateLimitIgnoresSpoofedForwardedFor`, `…ErrorResponsesKeepSandboxCSP`, `…OutsiderCannotRevokeExpiredShare`. **clean** — `…RevokedAndExpiredTokensAreDead`, `…NoAuthCookieOnPublicResponse`, `…LiveShareMutationNeedsWrite`. **"share by a member who since lost access" never exercised.** | revoked or expired token still serves; token guessable/enumerable; CSP `sandbox` header missing on any `/s/*` response (incl. errors, markdown, downloads); auth cookie present on a `/s/*` response; rate-limit bypass (header spoofing, `X-Forwarded-For`); share created by a member who has since lost access still serving |
| 8 | Invites & signup (`authlocal.go`, `authcli.go`, `orgs.go`) | **clean** — `TestSec_Invite_ForgedExpiredRevokedCannotCreateAccount`, `…RedemptionIsOrgScopedAndRevocable`, `…OnlyOwnersMintAndListLinks`, `…CLIOneTimeCodesAreNotReplayable`. **Seat/quota check on redemption (`CheckSeat`) never exercised.** | account created while `allow_signup:false` without a valid invite; invite reused past expiry or after revocation; invite for org A joining org B; `signupInvited` skipping domain/verification gates via a forged token; seat/quota check skipped; CLI one-time codes (`/api/auth/exchange`, device poll) replayable or brute-forceable |
| 9 | Password & token handling (`authlocal.go`) | **fixed** — `TestSec_Password_ResetRevokesExistingTokens`. **clean** — `…ResetGrantIsSingleUseAndExpires`, `…LoginAndResetDoNotEnumerateAccounts` (body/status only), `…NoCredentialMaterialInResponses` (responses only). **Timing-based enumeration and log-line leakage never exercised.** | reset token replay/expiry; reset for another account; user enumeration via response or timing difference on login/reset; token compared non-constant-time; any password or token appearing in a log line or an API response |
| 10 | Read-heat privacy (`reads.go`, `handleHeat`) | **untested** — no test asserts identity-free `/heat` output. The permission half ("heat for a project you can't read") is covered only incidentally by row 2's route sweeps. | any email, device id, or token reaching a client through `/heat`, its errors, or `/api/p/<id>/reads`; heat for a project you can't read |
| 11 | Path handling (`dir.go`, `handleFile/Download/Render/Blob`) | **untested** — `TestSec_Store_KeyEscapesRefused` covers store *keys* only. No test sends `..`, an absolute path, a symlink, an encoded separator or a NUL to a viewer read route. | `..`, absolute paths, symlinks, encoded separators, NUL — reaching a file outside the project root or outside the served folder |
| 12 | Secret leakage (`handleConfig`, `web.go`, error bodies) | **untested** — only the anonymous `/api/config` body is asserted (`TestSec_AuthGate_ConfigLeaksNothingToAnonymous`, `TestSec_Password_NoCredentialMaterialInResponses`). DSN, SMTP password, the hub's own device token, error bodies and stack traces: never exercised. | storage credentials, bucket URL, DB DSN, SMTP password, or the server's own device token reachable by any client; stack traces or internal paths in error responses |
| 13 | Agent hook guard (`internal/agenthooks`) | **untested** — zero `TestSec_*` tests exist in this package. Highest blast radius on the board. | shell injection through a mount path, project name, or file path into the inline hook command — it runs on **every** tool call on the machine, so this is the highest-blast-radius row |
| 14 | Metadata store (`db_sql.go`, `db_file.go`) | **untested** — zero `TestSec_*` tests. `db_conformance_test.go` is a functional test, not an attack. | SQL injection on any repo method; a record write crossing org/project scope; the file backend's atomic-rewrite path corrupting or exposing another tenant's rows |

Round 1 result: 12 holes closed, 43 `TestSec_*` tests green, whole suite green.
Rows 10–14 were never reached by any attacker and are round 2's targets.

Rows 1–3 and 5 are the highest value: they are choke points, so a hole there
is a hole everywhere downstream.

### Known-open, deliberately deferred (round 1)

Neither has a reproducer, and both are one design decision wide. They are
listed here so nobody reads a green board as "nothing left".

- **`projectPerm` returns `PermAdmin` when `s.Dir == nil || s.Auth == nil`.**
  Unreachable on a configured hub (`cmd/bdrive/web.go` sets both together),
  but a provider swap that leaves one nil makes every account admin hub-wide.
  Failing closed here is a two-line change and a large test-fixture change:
  `newHub`, `authHub` and `shareHub` all build servers without a `Dir`, so the
  escape is what makes most of the suite's project routes reachable. Closing
  it means deciding what an auth-without-orgs hub means first.
- **`Op.User`/`UserName` on `/store/*` pushes are whatever the client claims**,
  and now so is `X-Bdrive-Device`: the hub holds one request to one journal,
  but nothing binds that journal to the account or device that owns it. So
  History attribution is client-asserted. The fix is to *verify and reject*
  (compare the pushed ops' `User` against the authenticated account, and the
  device id against the device registry's owner) — never to rewrite the
  journal, which would break replay determinism between a device and its own
  remote copy.

## Out of scope

Do not spend rounds on: DoS/resource exhaustion, dependency CVEs, TLS/deploy
config, physical/social attacks, anything requiring the attacker to already
have shell on the hub, or the `cloud/` private repo. Report them as one line
in `SUSPICIONS.md` and move on.

Do not attack any live or shared host. Everything runs against a hub the test
starts itself, with its own temp `HOME`/`BDRIVE_HOME`.

## Severity (triage order only, never a pass mark)

- **critical** — crosses an org or project boundary, or escalates permission,
  with no victim action needed
- **high** — same, but needs a specific victim action or a stale/leaked token
- **medium** — leaks identity, metadata, or existence of resources
- **low** — hardening; violates a stated invariant with no demonstrated impact

Fix in that order. Severity never substitutes for a test.

## Round protocol

**hacker** — pick the highest-value `untested` or `clean` row. Read the real
code before theorizing. Produce failing tests + a one-line-per-finding list.
Never fixes anything. Never grades. If a round yields nothing, say "dry" and
name which rows you actually exercised.

**ciso** — fixes at the choke point, not the symptom: grep every caller before
editing, and prefer one guard in the shared path over N guards in handlers
(the route table + `requirePerm` exist precisely so handlers don't grow their
own checks). Then updates the scoreboard, and — the part the hacker won't do —
names which rows are still `untested` and which the hacker claimed but never
actually exercised. That list is the next round's target.

**Neither agent grades its own work.** The grader is `go test ./...`.

## Invariants a fix must not break

From `CLAUDE.md` — a fix that violates one of these is rejected, however
secure it looks:

- each device writes only its own journal
- blobs are pushed before the journal
- scan happens before pull in `Cycle`
- `journal.Less` / `Replay` stay deterministic
- materialize never clobbers dirty files
- state files are written atomically
- the agent hook guard stays pure shell and never spawns `bdrive` outside a mount
- pull/push errors degrade to `Result.Offline`, never fail the cycle
- telemetry never fails a request or a sync cycle

## The harness — reuse it, do not rebuild it

`internal/webapp` already has the multi-tenant fixture every attack needs.
Building your own hub from scratch is how a round gets wasted.

- `permHub(t)` (`perms_test.go:29`) → `(h http.Handler, srv *Server, cookies map[string]*http.Cookie, p Project)`.
  **alice** owns the org and the project `p`; **bob** and **carol** are plain
  members; **dave** is in a different org entirely. This is the attacker set:
  dave = outsider, bob/carol = insiders to demote or escalate.
- `doAs(t, h, method, url, body, cookie)` → `*httptest.ResponseRecorder`
- `signupAndSession(t, h, email, name, password)` → session cookie
- `newHub(t, upload, wrap)` (`store_test.go:18`) — bare hub; `wrap` lets you
  intercept the `remote.Backend` to observe the exact keys written
- `shareHub(t)`, `authedShare(t, ...)` (`shares_test.go:27,47`) — share fixtures
- `jsonReq`, `authAs`, `doHTTP` (`shares_test.go:436,450,20`) — bearer-token path
- `gatedAuth(t, tune)` (`gating_test.go:13`) — signup-policy fixture

Rules that keep four attackers from colliding in one package:

- **Never edit an existing test file.** Add your own new file only.
- Every helper you add is prefixed with your file's slug (`gateDo`, `permsDo`)
  so two files can't declare the same name.
- Reuse the helpers above by calling them; do not copy them.
