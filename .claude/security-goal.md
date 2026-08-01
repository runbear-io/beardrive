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

| # | Boundary | State after round 2 | Attacks that must be tried |
|---|----------|---------------------|----------------------------|
| 1 | Auth gate (`auth.go:authGate`) | **clean** — `TestSec_AuthGate_AnonymousPathTricksCannotReadAPI`, `…CannotWrite`, `…ConfigLeaksNothingToAnonymous`, `…ForgedAndTamperedCredentialsRefused`, `…CredentialDiesWithAccountAndMembership`, `TestSec_Path_LogoutRevokesTheTokenNotJustTheCookie`. **No TTL exists to test** — see "nothing expires" below. | reach any `/api/**` with no/expired/forged credential; abuse the `!HasPrefix("/api/")` open-path rule; path tricks (`//api/`, `/api/../`, encoded) that route to a handler but read as "open" |
| 2 | Per-project permission choke point (`perms.go:projectPerm/requirePerm`, `server.go` route table) | **fixed** (r1) — `TestSec_Perms_RemovedOrgMemberLosesProjectAccess`, `…OrgLessProjectIsNotAdminForEveryone`. **clean** — `…ReadOnlyMemberCannotWrite`, `…WriteMemberCannotAdmin`, `…NoneMemberReachesNothing`, `…CorruptGrantFailsClosed`, `…NoneMemberCannotListProjectSharesViaOrg`, `…StoreAndUploadRoutesUnderDeviceToken`. `s.Dir == nil \|\| s.Auth == nil → PermAdmin` **still open, still deliberately deferred**, untested. | `read` member performing any `PermWrite` action; `write` member performing `PermAdmin`; `none`/non-member reaching a project; the fail-open escapes reachable on a configured hub |
| 3 | Routes **outside** `proj()` | **fixed** (r1) — `TestSec_Row3_OrgSharesLeaksDeniedProject`, `…ExpiredShareRevokableByOutsider`. **clean** — `…ShareMutationByOutsider`, `…PermissionRoutes`, `…ProjectLifecycleRoutes`, `…OrgRoutes`, `…InviteAccept`, `…AdminRoutes`. | each one, exercised by a non-member, a read-only member, and a non-owner |
| 4 | Cross-org isolation (`orgs.go`, `projects.go`, `directory.go`) | **clean** — `TestSec_CrossOrg_ProjectRoutesRefuseOutsider`, `…OrgRoutesRefuseOutsiderAndNonOwner`. Round 2 found two cross-org leaks that entered through OTHER surfaces (rows 10 and 11), both now **fixed**. | project id from org B against every route; `/api/projects` and `/api/orgs` leaking names/ids; org rename/member routes on someone else's org |
| 5 | Sync proxy `/store/*` (`store.go`, `remote/http.go`) | **fixed** (r1) — `TestSec_Store_ForeignDeviceJournalWrite`, `…BlobContentMustMatchItsKey`, `…QuotaHonorsUnsizedPut`. **clean** — `…KeyEscapesRefused`. **fixed** (r2) — `TestSec_Leak_ErrorBodiesRevealServerFilesystemPaths` (storage errors relayed verbatim). **Device identity is still client-asserted**, and the journal BODY is still unvalidated apart from `Blob` (see gaps). | write a different device's journal key; key traversal; read another project's blob by sha; `store/sign` minting a URL outside the prefix or for a journal key |
| 6 | Upload (`upload.go`) | **fixed** (r1) — `TestSec_Upload_ReservedDirsRefused` + `TestSec_Sync_PeerJournalCannotMaterializeReservedPaths` (`internal/syncer`), `…QuotaUsesRealSize`. **fixed** (r2) — `TestSec_Path_DirUploadCannotEscapeThroughSymlink` (single-volume upload through a pre-existing symlink). **clean** — `…TargetStaysInProject`, `TestSec_Path_WriteRoutesRefuseTraversal`, `TestSec_Path_RestoreRefusesForeignSHA`. | presigned target outside the project prefix; `upload/commit` journaling `..`/absolute; committing content never uploaded; quota bypass |
| 7 | Share links (`shares.go`, `ratelimit.go`, `server.go:handleShared`) | **fixed** (r1) — `TestSec_Share_OrgAuditLeaksDeniedProjectTokens`, `…RateLimitIgnoresSpoofedForwardedFor`, `…ErrorResponsesKeepSandboxCSP`, `…OutsiderCannotRevokeExpiredShare`. **fixed** (r2) — `TestSec_Share_RemovedOrgMemberLinkStopsServing` (offboarding now ends a link; resolved at read time in `shareCreatorStillBelongs`). **clean** — `…RevokedAndExpiredTokensAreDead`, `…NoAuthCookieOnPublicResponse`, `…LiveShareMutationNeedsWrite`, `…DemotedMinterCannotManageTheirLink`. | revoked/expired token still serves; token guessable; missing CSP `sandbox`; auth cookie on `/s/*`; rate-limit bypass; share by someone who lost access |
| 8 | Invites & signup (`authlocal.go`, `authcli.go`, `orgs.go`) | **clean** — `TestSec_Invite_ForgedExpiredRevokedCannotCreateAccount`, `…RedemptionIsOrgScopedAndRevocable`, `…OnlyOwnersMintAndListLinks`, `…CLIOneTimeCodesAreNotReplayable`, `…SeatCheckCannotBeSkipped`. **fixed** (r2) — `TestSec_Invite_SeatCheckIsAtomic` (check-then-act race on the last seat), `TestSec_DB_RevokedInviteMustNotSurviveAFailedWrite` (revocation that only looked durable). | account created while `allow_signup:false`; invite reused past expiry/revocation; invite for org A joining org B; `signupInvited` skipping gates; seat check skipped or raced; CLI codes replayable |
| 9 | Password & token handling (`authlocal.go`) | **fixed** (r1) — `TestSec_Password_ResetRevokesExistingTokens`. **fixed** (r2) — `TestSec_Path_AuthNextCannotLeaveTheHub` (open redirect off the sign-in page via `/\`, `/<TAB>/`). **clean** — `…ResetGrantIsSingleUseAndExpires`, `…LoginAndResetDoNotEnumerateAccounts` (body/status only), `…NoCredentialMaterialInResponses` (responses only), `…ResetKillsCLIIssuedToken`, `TestSec_Path_VerifyGrantIsSingleUseAndTypeBound`. **Timing-based enumeration and log-line leakage still never exercised.** | reset token replay/expiry; reset for another account; enumeration via response or timing; non-constant-time compare; credentials in a log line |
| 10 | Read-heat privacy (`reads.go`, `handleHeat`) | **fixed** — `TestSec_Heat_ByDeviceLeaksForeignDeviceMetadata`, `TestSec_Heat_ReadReportCannotInjectAnIdentity`, `TestSec_Reads_ReportCannotRewriteAnotherOrgsDevice` (the device id a client reports is now validated against the registry's owner before it becomes an actor, `devices.go:ownsDevice`). **clean** — `…NoQueryShapeLeaksAnActor`, `…RefusedWithoutReadPermission`, `TestSec_Reads_MalformedReportsStayHarmless`. Design conflict resolved in favour of "`?by=device` may report an owned device id"; `reads.go`'s comment and CLAUDE.md now say the same thing. | any email, device id or token reaching a client through `/heat`, its errors, or `/api/p/<id>/reads`; heat for a project you can't read; **the reader-differencing oracle (never tried)** |
| 11 | Path handling (`dir.go`, `handleFile/Download/Render/Blob`) | **fixed** — `TestSec_Path_ViewerBlobEscapesProjectPrefix`, `TestSec_Path_MemberReadsAnotherOrgsBlob` (a journal's `Blob` was an unvalidated storage key: read any file on the hub host, any org's), `TestSec_Path_BlobInlineHTMLIsSandboxed` (stored XSS on the hub origin via history `/blob`). **clean** — `…ShaParamsRejectNonHex`, `…ShaFromAnotherProjectMisses`, `…DirViewerRefusesTraversal`, `…DirSymlinkIsNotServed`, `…SingleVolumeRoutesAreModeScoped`. | `..`, absolute paths, symlinks, encoded separators, NUL — reaching a file outside the project root or the served folder; **any other journal field used as a key or a path (only `Blob` was audited)** |
| 12 | Secret leakage (`handleConfig`, `web.go`, error bodies) | **fixed** — `TestSec_Leak_ErrorBodiesRevealServerFilesystemPaths` (storage paths / bucket+key in `/store/object`, `/file`, `/download`, `/render`). **clean** — `TestSec_Leak_NothingSensitiveForAnOrdinaryMember`, `TestSec_AuthGate_ConfigLeaksNothingToAnonymous`, `TestSec_Password_NoCredentialMaterialInResponses`. **A hub built by `cmd/bdrive/web.go` — the only place a real DSN or SMTP password exists — is still never instantiated by any test.** | storage credentials, bucket URL, DB DSN, SMTP password, the hub's own device token reachable by any client; stack traces or internal paths in errors |
| 13 | Agent hook guard (`internal/agenthooks`) | **fixed** — `TestSec_Hooks_GuardNeverSpawnsBdriveOutsideAMount` (a newline in a directory name split the `grep -F` pattern and matched every mount), `TestSec_Hooks_InstallKeepsItsOwnUserConfig`, `…InstallFromHomeKeepsItsOwnUserConfig` (init silently deleted the hooks it had just written when `$HOME` is a git repo). **clean** — `…MountPathMetacharactersNeverExecute`, `…RegistryContentsNeverExecute`, `…EveryHookCommandIsGuarded`. | shell injection through a mount path, project name, or file path into the inline hook command |
| 14 | Metadata store (`db_sql.go`, `db_file.go`) | **clean** — `TestSec_DB_HostileStringsStayDataOnEveryBackend`, `…NULBytesDoNotTruncateRecords` — **on the file and sqlite backends only**. **fixed** — `…OrgMemberMapDoesNotEscapeTheRegistry`, `…ProjectPermsMapDoesNotEscapeTheRegistry` (live maps handed out — a role/grant writable past every guard, plus a real `concurrent map iteration and map write` crash), `…FailedGrantWriteLeavesRegistryAgreeingWithDisk` (refused writes applied in memory), `…RevokedInviteMustNotSurviveAFailedWrite`, `…FileBackendSecretsDirectoryIsNotWorldReadable`. | SQL injection on any repo method; a record write crossing org/project scope; the file backend's atomic-rewrite path corrupting or exposing another tenant's rows |

Round 1 result: 12 holes closed, 43 `TestSec_*` tests green.
Round 2 result: **17 holes closed** (4 hackers, 19 failing tests), **86 `TestSec_*`
tests green** (85 in `internal/webapp` + `internal/agenthooks`, 1 in
`internal/syncer`), whole suite green. Rows 10–14, untouched by round 1, are
now all exercised — and rows 10, 11, 13 and 14 each held a real hole; the
worst (row 11) crossed every org boundary on the hub with no victim action.

Rows 1–3 and 5 are the highest value: they are choke points, so a hole there
is a hole everywhere downstream.

### Known-open, deliberately deferred

Carried from round 1 (still open, still no reproducer):

- **`projectPerm` returns `PermAdmin` when `s.Dir == nil || s.Auth == nil`.**
  Unreachable on a configured hub (`cmd/bdrive/web.go` sets both together),
  but a provider swap that leaves one nil makes every account admin hub-wide.
  Closing it means deciding what an auth-without-orgs hub means first, and
  rewriting `newHub`/`authHub`/`shareHub`, which rely on the escape.
- **`Op.User`/`UserName` on `/store/*` pushes are whatever the client claims**,
  and so is `X-Bdrive-Device`. Round 2 narrowed the blast radius — a forged
  device id can no longer take over a registry row (`DeviceRegistry.Observe`)
  or become a heat actor (`ownsDevice`) — but History attribution is still
  client-asserted. The fix remains verify-and-reject, never rewrite the
  journal (that would break replay determinism between a device and its
  remote copy).

New from round 2:

- **Nothing expires.** Device tokens and session cookies have no server-side
  TTL and the cookie carries no `Expires`, so a stolen credential is valid
  until a password reset or an explicit logout. "Test an expired session" is
  untestable because the concept does not exist in the code. This is a design
  decision to make, not a bug to patch.
- **Only `Blob` is validated on an incoming journal.** `handleStorePut`
  checks the KEY and (since round 1) a blob's content hash, but a journal is
  arbitrary JSONL: `Path`, `Note`, `Device`, `Lamport`, `Mtime` and `Mode`
  all enter the model unchecked. Round 2 closed `Blob` at the read side
  (`RemoteSource.Files`/`Open`) plus a `localBackend` root check as defence in
  depth; a peer-journal `Path` is filtered at materialize time (row 6's syncer
  test) but nothing validates the rest at ingest.

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

## Coverage gaps after round 2 — the next round's targets

Written by the CISO, verified against the tests that actually exist. A row
being `clean` or `fixed` above means one attack was refused, not that the
boundary is exhausted.

**Never exercised by any round (no test names it at all):**

- `GET /` — `Server.frontend`: the SPA fallback and the embedded-asset
  handler. The only route in `server.go` with zero `TestSec_*` coverage. Path
  handling into `fs.Sub(staticFiles)`, the asset/no-cache header split, and
  whether a crafted path can be served as an asset are all untested.
- **A hub built the way production builds one.** `cmd/bdrive/web.go` is never
  instantiated by a webapp test, so a hub with a real `database` DSN, a real
  SMTP password, or `TrustProxy` on has never been probed for leakage or
  behaviour. Row 12 is `clean` only for hubs the test fixtures build.
- **Postgres.** `metaBackends` skips it unless `BDRIVE_TEST_POSTGRES` is set,
  and it was not set in any round-2 run (the helper logs
  "postgres backend UNTESTED in this run"). Postgres is the only backend where
  `q()`'s `?`→`$N` rewrite is live, so row 14's SQL-injection result covers
  **file and sqlite only**.
- **Timing-based user enumeration** on login/reset. Row 9's test compares
  bodies and status codes, never durations.
- **Credential leakage into log lines.** Row 9 and row 12 assert on responses
  only. Round 2 added `storageErr`, which deliberately logs what it refuses to
  return — nothing checks what else reaches the log.
- **The reader-differencing oracle on `/heat`.** `readers` is a distinct-actor
  count; polling it around a known event can still identify who read what,
  without any identity ever appearing in a response.
- **`/s/*` → `ReadKindShare` recording.** The share read path writes a bucket
  keyed by token+IP; no test drives it.
- **Rate limiting on anything but `/s/*`.** Login has a limiter
  (`authLim`); no `TestSec_*` exercises it.

**Claimed but thin — a test exists, but only for one narrow shape:**

- Row 5's `/store/*`: every route is covered for permission LEVEL (row 2's
  sweeps) and for key shape, but `store/sign` has no test of its own beyond
  the level sweep — no test asserts a signed URL's target, TTL, or that a
  journal key is never signed on a backend that CAN sign (the fixtures use
  `file://`, which cannot).
- Row 11: only `Blob` was audited among journal fields (see above).
- Row 14: `clean` is per-backend, and one backend of three never ran.
- Row 1: the credential tests cover forged/tampered/deleted — never expired,
  because expiry does not exist.

**Fixes made this round that deserve their own next-round attack:**

- `ownsDevice` (reads.go/devices.go) — first-caller-wins ownership of a device
  row. Is there a way to register a device id before its real owner does?
- `shareCreatorStillBelongs` (shares.go) — a read-time membership check on a
  public route. What happens to it when `Dir` is a remote directory whose
  `Role` is cache-backed?
- `safeNext` — verified against tab/CR/LF and backslash. Not against
  percent-encoded or unicode look-alike separators.
