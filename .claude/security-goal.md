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

| # | Boundary | State after round 3 | Attacks that must be tried |
|---|----------|---------------------|----------------------------|
| 1 | Auth gate (`auth.go:authGate`) | **clean** — `TestSec_AuthGate_AnonymousPathTricksCannotReadAPI`, `…CannotWrite`, `…ConfigLeaksNothingToAnonymous`, `…ForgedAndTamperedCredentialsRefused`, `…CredentialDiesWithAccountAndMembership`, `TestSec_Path_LogoutRevokesTheTokenNotJustTheCookie`. **No TTL exists to test** — see "nothing expires" below. | reach any `/api/**` with no/expired/forged credential; abuse the `!HasPrefix("/api/")` open-path rule; path tricks (`//api/`, `/api/../`, encoded) that route to a handler but read as "open" |
| 2 | Per-project permission choke point (`perms.go:projectPerm/requirePerm`, `server.go` route table) | **fixed** (r1) — `TestSec_Perms_RemovedOrgMemberLosesProjectAccess`, `…OrgLessProjectIsNotAdminForEveryone`. **clean** — `…ReadOnlyMemberCannotWrite`, `…WriteMemberCannotAdmin`, `…NoneMemberReachesNothing`, `…CorruptGrantFailsClosed`, `…NoneMemberCannotListProjectSharesViaOrg`, `…StoreAndUploadRoutesUnderDeviceToken`. `s.Dir == nil \|\| s.Auth == nil → PermAdmin` **still open, still deliberately deferred**, untested. | `read` member performing any `PermWrite` action; `write` member performing `PermAdmin`; `none`/non-member reaching a project; the fail-open escapes reachable on a configured hub |
| 3 | Routes **outside** `proj()` | **fixed** (r1) — `TestSec_Row3_OrgSharesLeaksDeniedProject`, `…ExpiredShareRevokableByOutsider`. **clean** — `…ShareMutationByOutsider`, `…PermissionRoutes`, `…ProjectLifecycleRoutes`, `…OrgRoutes`, `…InviteAccept`, `…AdminRoutes`. | each one, exercised by a non-member, a read-only member, and a non-owner |
| 4 | Cross-org isolation (`orgs.go`, `projects.go`, `directory.go`) | **clean** — `TestSec_CrossOrg_ProjectRoutesRefuseOutsider`, `…OrgRoutesRefuseOutsiderAndNonOwner`. Round 2 found two cross-org leaks that entered through OTHER surfaces (rows 10 and 11), both now **fixed**. | project id from org B against every route; `/api/projects` and `/api/orgs` leaking names/ids; org rename/member routes on someone else's org |
| 5 | Sync proxy `/store/*` (`store.go`, `remote/http.go`) | **fixed** (r1) — `TestSec_Store_ForeignDeviceJournalWrite`, `…BlobContentMustMatchItsKey`, `…QuotaHonorsUnsizedPut`. **clean** — `…KeyEscapesRefused`. **fixed** (r2) — `TestSec_Leak_ErrorBodiesRevealServerFilesystemPaths` (storage errors relayed verbatim). **fixed** (r3) — `TestSec_Heat_StoreRouteCannotMintAnArbitraryHeatActor` (this route registered any device id a header named, hub-wide, with no ownership check; registration is now per `(account, id)` and the id must be shaped like one). The journal BODY is still unvalidated at ingest apart from `Blob` — but `Path`, `Mode` and `Lamport` are now all refused on the receiving device (row 15). | write a different device's journal key; key traversal; read another project's blob by sha; `store/sign` minting a URL outside the prefix or for a journal key |
| 6 | Upload (`upload.go`) | **fixed** (r1) — `TestSec_Upload_ReservedDirsRefused` + `TestSec_Sync_PeerJournalCannotMaterializeReservedPaths` (`internal/syncer`), `…QuotaUsesRealSize`. **fixed** (r2) — `TestSec_Path_DirUploadCannotEscapeThroughSymlink` (single-volume upload through a pre-existing symlink). **fixed** (r3) — `TestSec_Path_RefusedUploadCreatesNothingOutsideTheServedFolder` (`os.MkdirAll` ran before `underRoot`, so a *refused* upload had already built the parent chain through the symlink; the check now runs against the deepest existing ancestor, before anything is created). **clean** — `…TargetStaysInProject`, `TestSec_Path_WriteRoutesRefuseTraversal`, `TestSec_Path_RestoreRefusesForeignSHA`, `TestSec_Path_UploadOntoASymlinkedNameDoesNotFollowIt`. | presigned target outside the project prefix; `upload/commit` journaling `..`/absolute; committing content never uploaded; quota bypass |
| 7 | Share links (`shares.go`, `ratelimit.go`, `server.go:handleShared`) | **fixed** (r1) — `TestSec_Share_OrgAuditLeaksDeniedProjectTokens`, `…RateLimitIgnoresSpoofedForwardedFor`, `…ErrorResponsesKeepSandboxCSP`, `…OutsiderCannotRevokeExpiredShare`. **fixed** (r2) — `TestSec_Share_RemovedOrgMemberLinkStopsServing` (offboarding now ends a link; resolved at read time in `shareCreatorStillBelongs`). **fixed** (r3) — `TestSec_Share_CreatorMembershipIsResolvedFailClosed` (round 2's own fix failed OPEN when the project's org was empty or unresolvable: clearing a project's org resurrected every offboarded member's public link). **clean** — `…RevokedAndExpiredTokensAreDead`, `…NoAuthCookieOnPublicResponse`, `…LiveShareMutationNeedsWrite`, `…DemotedMinterCannotManageTheirLink`, `TestSec_Share_PublicHitRecordsShareKindEndToEnd`, `…VisitorCannotInflateOrRedirectTheLedger`, `…DeadLinksRecordNothing`, `TestSec_Path_HostileBlobCannotRepointALiveShare`. **fixed** (r3) — `TestSec_RateLimit_TrustedProxyUsesTheHopItAdded` (with `trust_proxy` on the limiter keyed on the FIRST `X-Forwarded-For` entry, which the client prepends — so turning the flag on disabled the limiter it was added to fix; it now takes the last hop). | revoked/expired token still serves; token guessable; missing CSP `sandbox`; auth cookie on `/s/*`; rate-limit bypass; share by someone who lost access |
| 8 | Invites & signup (`authlocal.go`, `authcli.go`, `orgs.go`) | **clean** — `TestSec_Invite_ForgedExpiredRevokedCannotCreateAccount`, `…RedemptionIsOrgScopedAndRevocable`, `…OnlyOwnersMintAndListLinks`, `…CLIOneTimeCodesAreNotReplayable`, `…SeatCheckCannotBeSkipped`. **fixed** (r2) — `TestSec_Invite_SeatCheckIsAtomic` (check-then-act race on the last seat), `TestSec_DB_RevokedInviteMustNotSurviveAFailedWrite` (revocation that only looked durable). | account created while `allow_signup:false`; invite reused past expiry/revocation; invite for org A joining org B; `signupInvited` skipping gates; seat check skipped or raced; CLI codes replayable |
| 9 | Password & token handling (`authlocal.go`) | **fixed** (r1) — `TestSec_Password_ResetRevokesExistingTokens`. **fixed** (r2) — `TestSec_Path_AuthNextCannotLeaveTheHub` (open redirect off the sign-in page via `/\`, `/<TAB>/`). **clean** — `…ResetGrantIsSingleUseAndExpires`, `…LoginAndResetDoNotEnumerateAccounts` (body/status only), `…NoCredentialMaterialInResponses` (responses only), `…ResetKillsCLIIssuedToken`, `TestSec_Path_VerifyGrantIsSingleUseAndTypeBound`. **fixed** (r3) — `TestSec_Leak_ResetTimingDoesNotEnumerateAccounts` (on a hub with SMTP, `POST /auth/reset` blocked on the mail dial only for addresses that exist, and was not rate limited; mail now goes out off the request path and `/auth/reset` joins `rateLimitAuth`). **clean** (r3) — `TestSec_Password_LoginTimingDoesNotEnumerateAccounts`, `TestSec_Leak_NewLogLinesCarryNoCredential`, `TestSec_Path_NextCannotLeaveTheHubOnAnyAuthRoute` (`safeNext` against 20 hostile values on every auth route). | reset token replay/expiry; reset for another account; enumeration via response or timing; non-constant-time compare; credentials in a log line |
| 10 | Read-heat privacy (`reads.go`, `handleHeat`) | **fixed** — `TestSec_Heat_ByDeviceLeaksForeignDeviceMetadata`, `TestSec_Heat_ReadReportCannotInjectAnIdentity`, `TestSec_Reads_ReportCannotRewriteAnotherOrgsDevice` (the device id a client reports is validated before it becomes an actor, `devices.go:ownsDevice`). **fixed** (r3) — `TestSec_Heat_PlantedIdentityCannotBeSelfRegisteredThenReported`, `TestSec_Heat_StoreRouteCannotMintAnArbitraryHeatActor`, `TestSec_Devices_IdCannotBeSquattedBeforeItsOwnerRegisters`, `TestSec_Devices_SquattedIdStillCountsItsOwnersReads`, `TestSec_Reads_OneUnstorableBucketCannotWedgeTheLedger` (a single NUL-bearing path from a read-only member wedged the whole hub's telemetry forever on Postgres). **clean** — `…NoQueryShapeLeaksAnActor`, `…RefusedWithoutReadPermission`, `TestSec_Reads_MalformedReportsStayHarmless`, `TestSec_Devices_ConcurrentRegistrationLeavesOneConsistentOwner`, `TestSec_Heat_ReaderDifferencingCannotNameAReader` + `…NestedPrefixAndDayWindowsCarryNoActorAxis` (**the reader-differencing oracle does not exist**: 112 query shapes, byte-identical responses), `TestSec_Ledger_ReplicationAndHistoryViewsAreNeverReads`. Design conflict resolved in favour of "`?by=device` may report an owned device id"; `reads.go`'s comment and CLAUDE.md now say the same thing. | any email, device id or token reaching a client through `/heat`, its errors, or `/api/p/<id>/reads`; heat for a project you can't read; the reader-differencing oracle |
| 11 | Path handling (`dir.go`, `handleFile/Download/Render/Blob`) | **fixed** — `TestSec_Path_ViewerBlobEscapesProjectPrefix`, `TestSec_Path_MemberReadsAnotherOrgsBlob` (a journal's `Blob` was an unvalidated storage key: read any file on the hub host, any org's), `TestSec_Path_BlobInlineHTMLIsSandboxed` (stored XSS on the hub origin via history `/blob`). **fixed** (r3) — `TestSec_Journal_HistoryDeviceFieldLeaksForeignDeviceMetadata`, `…IsNotAnExistenceOracle` (History joined the registry on the op's own `Device` field — client-asserted JSON, not the journal KEY round 1 bound; attribution now comes from the journal the op was read from, and the registry join is org-scoped), `TestSec_Journal_SizeFieldCannotForgeContentLength` (`Op.Size` was echoed as `Content-Length` for bytes the hub never measured). **clean** — `…ShaParamsRejectNonHex`, `…ShaFromAnotherProjectMisses`, `…DirViewerRefusesTraversal`, `…DirSymlinkIsNotServed`, `…SingleVolumeRoutesAreModeScoped`, `TestSec_Journal_HostilePathCannotBeLaunderedThroughRestoreOrRemove`, `TestSec_Path_ValidBlobHashStaysInsideItsProject`. | `..`, absolute paths, symlinks, encoded separators, NUL — reaching a file outside the project root or the served folder; every journal field (`Blob`, `Path`, `Device`, `Size` now audited; `Mtime`/`Seq` argued subsumed — see gaps) |
| 12 | Secret leakage (`handleConfig`, `web.go`, error bodies) | **fixed** — `TestSec_Leak_ErrorBodiesRevealServerFilesystemPaths` (storage paths / bucket+key in `/store/object`, `/file`, `/download`, `/render`). **clean** — `TestSec_Leak_NothingSensitiveForAnOrdinaryMember`, `TestSec_AuthGate_ConfigLeaksNothingToAnonymous`, `TestSec_Password_NoCredentialMaterialInResponses`. **fixed** (r3) — `TestSec_Leak_RealConfigPathKeepsSecretsOffTheWire` (the real config path set `srv.Volume` from the storage URL, so anonymous `/api/config` named the bucket — `s3://acme-prod-drive`; it now defaults to a storage-independent name and `--volume`/`volume:` stays the only way a storage string reaches the wire). **clean** (r3) — `TestSec_Admin_PolicyCannotWidenServerOwnedAccess`, `TestSec_Leak_NewLogLinesCarryNoCredential`. A hub built the production way is now instantiated by a test (real DSN, real SMTP password, `--upload`). | storage credentials, bucket URL, DB DSN, SMTP password, the hub's own device token reachable by any client; stack traces or internal paths in errors |
| 13 | Agent hook guard (`internal/agenthooks`) | **fixed** — `TestSec_Hooks_GuardNeverSpawnsBdriveOutsideAMount` (a newline in a directory name split the `grep -F` pattern and matched every mount), `TestSec_Hooks_InstallKeepsItsOwnUserConfig`, `…InstallFromHomeKeepsItsOwnUserConfig` (init silently deleted the hooks it had just written when `$HOME` is a git repo). **clean** — `…MountPathMetacharactersNeverExecute`, `…RegistryContentsNeverExecute`, `…EveryHookCommandIsGuarded`, and **new in r3** `…GuardStaysClosedForEveryControlCharacterInPWD`, `…GuardDoesNotTrustAnInheritedPWD`, `…GuardIsStillPureShell`, `…GuardStillFiresInsideARealMount` — 17 `$PWD` shapes across all three command builders including `hookPullCommand`, which round 2's regression test never exercised. **Round 3 came back dry here: this row's first dry result.** | shell injection through a mount path, project name, or file path into the inline hook command |
| 14 | Metadata store (`db_sql.go`, `db_file.go`) | **clean** — `TestSec_DB_HostileStringsStayDataOnEveryBackend`, `TestSec_DB_QueryRewriteOnlyEverSeesStaticSQL`, `…PlaceholderRewriteIsPositional`, `…QuestionMarksInValuesDoNotShiftPlaceholders` — **now verified on file, sqlite AND a real Postgres 16** (`BDRIVE_TEST_POSTGRES`, run this round). `…NULBytesDoNotTruncateRecords` is clean on file+sqlite and **RED on Postgres** — see "known-open". **fixed** (r3) — `TestSec_DB_EveryRegistryAccessorHandsOutACopy`, `…RollbackHoldsUnderConcurrentMutators`. **fixed** — `…OrgMemberMapDoesNotEscapeTheRegistry`, `…ProjectPermsMapDoesNotEscapeTheRegistry` (live maps handed out — a role/grant writable past every guard, plus a real `concurrent map iteration and map write` crash), `…FailedGrantWriteLeavesRegistryAgreeingWithDisk` (refused writes applied in memory), `…RevokedInviteMustNotSurviveAFailedWrite`, `…FileBackendSecretsDirectoryIsNotWorldReadable`. | SQL injection on any repo method; a record write crossing org/project scope; the file backend's atomic-rewrite path corrupting or exposing another tenant's rows |
| 15 | Peer journal on the RECEIVING device (`internal/syncer`: `materialize`, `Cycle`) | **fixed** (r3) — `TestSec_SyncJournal_PeerCannotMaterializeOutsideTheMount` (`..` in `Op.Path` resolved above the mount root: one pushed JSONL line wrote `~/.ssh/authorized_keys` on every teammate's machine), `…ReservedDirGuardIsCaseInsensitive` (`.GIT/hooks/pre-commit` cleared an exact-match guard and APFS/NTFS resolved it into the real `.git/hooks`), `…PeerCannotSetSetuidOrSetgidMode` (`Op.Mode` went to `os.Chmod` verbatim, setuid bits included), `…ExtremeLamportCannotFreezeADevice` (`Lamport: MaxInt64` wrapped a victim's clock negative and silently reverted its own edits forever). **clean** — `…HostileDeviceKindAndSizeStayInert`, `TestSec_Sync_PeerJournalCannotMaterializeReservedPaths`. | every field of an op a peer pushes, applied to a victim's disk: path escape, reserved dirs, mode bits, clock values |
| 16 | Frontend shell + embedded assets (`server.go:Server.frontend`) | **fixed** (r3) — `TestSec_Frontend_ShellCarriesFramingAndSniffingDefenses` (the one page carrying the session cookie had no `X-Frame-Options`, no `frame-ancestors`, no `nosniff`), `…ImmutableCacheOnlyOnRealAssets` (a miss under `assets/` returned the app shell marked immutable for a year). **clean** — `…FallbackServesOnlyEmbeddedAssets`. | frame the signed-in UI; MIME-sniff the shell; poison a shared cache at an asset URL; serve something outside the embedded FS |

Round 1 result: 12 holes closed, 43 `TestSec_*` tests green.
Round 2 result: **17 holes closed** (4 hackers, 19 failing tests), **86 `TestSec_*`
tests green** (85 in `internal/webapp` + `internal/agenthooks`, 1 in
`internal/syncer`), whole suite green. Rows 10–14, untouched by round 1, are
now all exercised — and rows 10, 11, 13 and 14 each held a real hole; the
worst (row 11) crossed every org boundary on the hub with no victim action.

Round 3 result: **17 holes closed** (4 hackers, 18 failing tests), **133
`TestSec_*` tests green** (124 in `internal/webapp`, 9 in `internal/agenthooks`,
5 in `internal/syncer`; 141 counting sub-tests), whole suite green, `-race`
clean on `webapp`/`syncer`/`store`/`daemon`. Round 3 aimed one hacker at the
PREVIOUS ROUNDS' FIXES and **two of them broke**: `ownsDevice` (round 2) was a
one-request speed bump — the very request it refused registered the refused id
to the caller — and `shareCreatorStillBelongs` (round 2) failed open on an
org-less project. The two worst findings of the round were not on the hub at
all: a peer's journal could write **anywhere on every teammate's filesystem**
(`..` in `Op.Path`) and could plant an executable `.git` hook through a
case-sensitive reserved-dir guard. Rows 15 and 16 are new: the receiving
device, and the frontend shell that had zero coverage after round 2.

Rows 1–3 and 5 are the highest value: they are choke points, so a hole there
is a hole everywhere downstream.

### Loop status after round 3 — NOT done

Both conditions are stated in "What counts as done". Neither is met.

1. **Every row `clean` or `fixed`, backed by a named test** — *not met*. Row 14
   is not closed: `TestSec_DB_NULBytesDoNotTruncateRecords/postgres` fails on a
   real Postgres and I refused to weaken it (see known-open). Rows 2 and 1 also
   carry named, still-open items (`Dir == nil → PermAdmin`; no expiry).
2. **Two consecutive dry hacker rounds** — *not met*. Round 3 produced 18
   failing tests across 17 holes, so it was not dry. The counter is at zero.
   For the record: row 13 (agent hooks) came back dry this round, its first
   dry result — that is one row, not one round.

### Known-open, deliberately deferred

Carried from round 1 (still open, still no reproducer):

- **`projectPerm` returns `PermAdmin` when `s.Dir == nil || s.Auth == nil`.**
  Unreachable on a configured hub (`cmd/bdrive/web.go` sets both together),
  but a provider swap that leaves one nil makes every account admin hub-wide.
  Closing it means deciding what an auth-without-orgs hub means first, and
  rewriting `newHub`/`authHub`/`shareHub`, which rely on the escape.
- **`Op.User`/`UserName` on `/store/*` pushes are whatever the client claims.**
  `X-Bdrive-Device` no longer is, in the ways that mattered: round 3 keyed the
  registry on `(account, id)`, so naming another account's id claims nothing
  and cannot lock its owner out, and History now attributes an op to the
  journal it was READ from rather than to the op's own `Device` field. The
  email fields are still unverified. The fix remains verify-and-reject, never
  rewrite the journal (that would break replay determinism between a device
  and its remote copy).

New from round 2 (still open):

- **Nothing expires.** Device tokens and session cookies have no server-side
  TTL and the cookie carries no `Expires`, so a stolen credential is valid
  until a password reset or an explicit logout. "Test an expired session" is
  untestable because the concept does not exist in the code. This is a design
  decision to make, not a bug to patch.
- **A journal is still not validated at INGEST.** `handleStorePut` checks the
  key and a blob's content hash; everything else is refused where it would do
  damage instead — `Path`/`Mode`/`Lamport` on the receiving device (row 15),
  `Blob`/`Device`/`Size` on the hub's read side (row 11). That is defence at
  the right place, not a gap, but it means a hostile journal sits in storage
  until each reader rejects it.

New from round 3:

- **`TestSec_DB_NULBytesDoNotTruncateRecords/postgres` is RED and I did not
  close it.** Verified against a real Postgres 16 this round. The test demands
  that `"laptop\x00-of-eve"` round-trip byte-exact on every backend; a
  Postgres `text` column cannot hold a NUL at all, so the row simply never
  persists. Making it green means either encoding every text value in
  `db_sql.go` (invisible mangling of ~15 write sites and ~10 `Scan` sites, for
  a byte that is never legitimate data) or changing the test to assert
  *consistent refusal* on all three backends. That is a design decision, and I
  do not weaken a hacker's test to end a round. What I did do is stop the
  divergence being reachable through the API: `observeDevice` strips control
  characters from the name/OS headers, `handleReadReport` refuses a path
  carrying one, and `DeviceRegistry.Observe` no longer swallows the repo error
  (it logs once and retries).
- **An unclaimed, well-formed device id can be named by any member.**
  `MayActAs` allows an id nobody else is syncing under, so a member can report
  agent reads in any project they can read under an invented id like
  `dev-a1b2c3d4e5f6`, and `/heat?by=device` will list it. No identity leaks
  (the id is opaque, the registry join is org-scoped) and it cannot displace a
  real device's row. Closing it means refusing an unregistered device's very
  first report — which round 3's own
  `TestSec_Reads_OneUnstorableBucketCannotWedgeTheLedger` fixture depends on
  (its read-only member never touches `/store/*`, so every one of its reports
  would stop counting). Needs the test's premise revisited first.
- **One `Lamport: MaxInt64` op still pins the file it names.** The clamp stops
  a peer wrecking a victim's clock, but `journal.Less` still orders that op
  above everything, so the attacker keeps last-writer-wins on that one path
  forever. Fixing it means rewriting or dropping a pulled op, which breaks
  replay agreement between a device and its remote copy — a stated invariant.
  Ingest-side validation on the hub is the only place this can be closed.

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

## Coverage gaps after round 3 — the next round's targets

Written by the CISO, verified against the tests that actually exist. A row
being `clean` or `fixed` above means one attack was refused, not that the
boundary is exhausted.

**Closed this round (was on round 2's gap list, now has an asserting test):**

- `GET /` — `Server.frontend` now has three tests (row 16), and two of them
  found real holes.
- A hub built the production way — `TestSec_Leak_RealConfigPathKeepsSecretsOffTheWire`
  boots `cmd/bdrive/web.go`'s real path with a real DSN, SMTP password and
  admin password, and found the bucket name on anonymous `/api/config`.
- Postgres — run for real this round (`postgres:16-alpine`,
  `BDRIVE_TEST_POSTGRES`). `q()`'s `?`→`$N` rewrite is **proven to only ever
  see static SQL, with a negative control**, behaviourally green on
  file+sqlite+postgres.
- Timing enumeration (login and reset), credential leakage into log lines,
  `/s/*` → `ReadKindShare` end to end, `/store/*` and history `/blob` proven
  never-a-read end to end, `safeNext` against 20 hostile values on every auth
  route, and login rate limiting under both `trust_proxy` postures.
- The **reader-differencing oracle on `/heat` does not exist**: 112 query
  shapes returned byte-identical responses. The hacker's judgment, which I
  endorse, is that the residual small-population inference (one reader, one
  file) is a property of publishing aggregates at all, not a code defect.
- The agent hook guard (row 13) came back **dry** — its first dry result — and
  the round-2 gap it closed was real: round 2's regression test never
  exercised `hookPullCommand`, the hook that fires on every turn.

**Still never reached by any round (no test names it at all):**

- **`store/sign` on a backend that can actually presign.** Every fixture uses
  `file://`, which cannot sign, so no test has ever observed a signed URL's
  target, TTL, or that a journal key is never signed. Three rounds have now
  listed this and three rounds have skipped it: it needs a fake `PutSigner`
  backend, roughly 30 lines, and it is the last unexercised capability on the
  highest-value row (5).
- **`Op.Mtime` and `Op.Seq` at extremes.** Round 3's hacker argued they are
  subsumed by the Lamport finding, and the argument is: `journal.Less` reads
  `(Lamport, Time, Device, Seq)` in that order, so `Lamport` dominates both —
  an attacker who can win the ordering does it with `Lamport` and never needs
  the other two. That is sound for *ordering*. It says nothing about `Mtime`
  reaching `os.Chtimes` or a display formatter, or `Seq` sizing a slice, and
  neither was checked. Record it as "argued, not tested".
- **`Dir == nil || Auth == nil → PermAdmin`** (row 2) — still no reproducer,
  still deferred by decision.
- **Expiry** (row 1) — untestable while the concept does not exist.

**Claimed but thin — a test exists, but only for one narrow shape:**

- Row 14's `NULBytesDoNotTruncateRecords` is `clean` on file+sqlite and RED on
  Postgres. That is now a known, documented divergence rather than an
  unverified claim, but the row is not closed.
- Row 15 covers `Path`, `Mode`, `Lamport`, `Device`, `Kind`, `Size` on the
  receiving device. `Note` reaches the CLI's own output (`bdrive log`) and no
  test asks what a terminal escape sequence in it does.

**Fixes made this round that deserve their own next-round attack:**

- **The `(account, id)` device registry** (`devices.go`) — the whole identity
  model changed. `MayActAs` allows an unclaimed id (see known-open);
  `LookupIn`'s org predicate is what stops one org reading another's device
  metadata through History and heat; `Get` is now "most recently observed",
  which is a display lookup nothing in production should call. Attack all
  three.
- **`unsafeRel`** (`internal/syncer`) — the new mount-boundary guard. It
  refuses anything that is not already a clean relative slash path. It does
  NOT reject a backslash (a legitimate unix filename), which matters the day
  `GOOS=windows` builds.
- **`safeMode`** strips group/other write as well as setuid/setgid, so a
  legitimately `0777` file materializes `0755` on every peer. That is a
  deliberate posture change, not a bug — but nothing tests the round trip.
- **`setContentLength`** — the header now goes out only when the reader can be
  measured (`*os.File`, or anything with `Size() int64`). On S3 it is absent.
  Nobody has checked what that does to a range request or a browser download.
- **`persistLocked`'s split retry** — on a batch failure it retries per bucket
  and drops the ones that fail alone. Is there a partial-failure shape that
  drops a bucket the store WOULD have accepted?
