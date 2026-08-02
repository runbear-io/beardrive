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

Tests live in the package they attack — `internal/webapp/`,
`internal/syncer/`, and since round 4 also `internal/{store,config,journal,remote}`
and `cmd/bdrive/` — named `TestSec_<Boundary>_<Attack>` — e.g.
`TestSec_Perms_ReadOnlyMemberCanPush`. Existing harnesses to build on:
`cli_e2e_test.go` (real binary, isolated `HOME`), `gating_test.go`,
`perms_test.go`, `store_test.go`, `shares_test.go`, `db_conformance_test.go`.

## Scoreboard

Every row starts `untested`. States: `untested` → `exploit (test name)` →
`fixed (test name)` , or `untested` → `clean (test name)`.
`clean` still needs a test — one that asserts the attack is refused.

| # | Boundary | State after round 8 | Attacks that must be tried |
|---|----------|---------------------|----------------------------|
| 1 | Auth gate (`auth.go:authGate`) | **clean** — `TestSec_AuthGate_AnonymousPathTricksCannotReadAPI`, `…CannotWrite`, `…ConfigLeaksNothingToAnonymous`, `…ForgedAndTamperedCredentialsRefused`, `…CredentialDiesWithAccountAndMembership`, `TestSec_Path_LogoutRevokesTheTokenNotJustTheCookie`. **No TTL exists to test** — see "nothing expires" below. **clean** (r6) — `TestSec_Auth_AProviderIdentityTheHubCannotResolveReachesNothing` (the `AuthProvider` seam a managed deployment swaps: an empty/unresolvable identity reaches nothing). **SABOTAGED FOR THE FIRST TIME (r7)** — `authGate`'s `if !open` gate was deleted and **9 tests caught it** (`…AnonymousPathTricksCannotReadAPI`, `…ForgedAndTamperedCredentialsRefused`, `…CredentialDiesWithAccountAndMembership`, `TestSec_Path_LogoutRevokesTheTokenNotJustTheCookie`, `TestSec_Password_ResetKillsCLIIssuedToken`, `TestSec_Path_SingleVolumeRoutesAreModeScoped`, `TestSec_Config_NoServedConfigurationReachesTheAdminEscape` and two more). The row's claim is load-bearing. Expiry is still untested and still has no concept in the code. | reach any `/api/**` with no/expired/forged credential; abuse the `!HasPrefix("/api/")` open-path rule; path tricks (`//api/`, `/api/../`, encoded) that route to a handler but read as "open" |
| 2 | Per-project permission choke point (`perms.go:projectPerm/requirePerm`, `server.go` route table) | **fixed** (r1) — `TestSec_Perms_RemovedOrgMemberLosesProjectAccess`, `…OrgLessProjectIsNotAdminForEveryone`. **clean** — `…ReadOnlyMemberCannotWrite`, `…WriteMemberCannotAdmin`, `…NoneMemberReachesNothing`, `…CorruptGrantFailsClosed`, `…NoneMemberCannotListProjectSharesViaOrg`, `…StoreAndUploadRoutesUnderDeviceToken`. `s.Dir == nil \|\| s.Auth == nil → PermAdmin` is **ANSWERED and guarded** (r5): nine real `bdrive serve -c` configurations, both arms, real project ids — all 14 per-project surfaces refused everywhere; `TestSec_Config_NoServedConfigurationReachesTheAdminEscape`, `TestSec_Config_OrgMigrationLeavesNoProjectWorldWritable`. **fixed** (r5) — `projectPerm` is also now the RECOVERY path for a squatted device id (`ownJournal`), so a project admin can push an affected journal. **fixed** (r6) — `TestSec_Audit_PermHubRefusesAForeignJournalOutOfTheBox`: the FIXTURE was the hole. `permHub` built `Devices == nil`, so through the suite's main hub every device-ownership decision returned early and a dozen journal-pushing tests measured org/project permission only. `permHub` now installs a `DeviceRegistry` before `srv.Handler()`, so the binding is exercised everywhere it is claimed. Three tests changed result and are named in "the fixture change" below. **SABOTAGED FOR THE FIRST TIME (r7)**, twice, and held both times: making `requirePerm` return true unconditionally turned **30 tests red**; deleting `projectPerm`'s `role == "" → PermNone` org-membership gate turned **21 tests red**. This is the row everything downstream leans on and it was the largest untested claim in the suite until now. | `read` member performing any `PermWrite` action; `write` member performing `PermAdmin`; `none`/non-member reaching a project; the fail-open escapes reachable on a configured hub |
| 3 | Routes **outside** `proj()` | **fixed** (r1) — `TestSec_Row3_OrgSharesLeaksDeniedProject`, `…ExpiredShareRevokableByOutsider`. **clean** — `…ShareMutationByOutsider`, `…PermissionRoutes`, `…ProjectLifecycleRoutes`, `…OrgRoutes`, `…InviteAccept`, `…AdminRoutes`. **fixed (r7)** — `TestSec_Row3_InviteAcceptRefusesAnIdentityWithNoAddress`: `handleInviteAccept` guarded on `me.Email == ""` while everything downstream normalizes (`normEmail` = lower+trim), so `"   "`, `"\t"`, `"\n "` walked past it — `Redeem` resolved the token and `CheckSeat` ran before `AddMember` finally refused, i.e. **an invite-token validity oracle for a principal the hub cannot name**, on an invite-only hub where that token bootstraps an account. **NOT a finding, recorded (r7)**: `projectJSON`'s `p.Perms, p.Default = nil, ""` can be deleted with the suite green, but all three callers already gate on `PermRead` and `/api/p/{id}/permissions` returns the same grants to the same audience — payload hygiene, not a guard. No test invented. **fixed (r8)** — `TestSec_Org_EvictingTheSoleOwnerCannotLeaveAnOrgNobodyCanAdminister`: round 7's own fix created a new state. `EvictMember` drops a row unconditionally (right: an ownership row for an address nobody can sign in as is inherited by the next signup on it), but every org route is gated on `RoleOwner` and NOTHING adopts an ownerless org — so one hub admin calling `Deny` on the sole owner left an org with members that can never again gain one, lose one, or change a role. Eviction of the last owner now promotes the longest-standing remaining member (`ponytail:` no join time is recorded, so it is the lowest address — deterministic on every replica). | each one, exercised by a non-member, a read-only member, and a non-owner |
| 4 | Cross-org isolation (`orgs.go`, `projects.go`, `directory.go`, `remote/prefixed.go`) | **clean** — `TestSec_CrossOrg_ProjectRoutesRefuseOutsider`, `…OrgRoutesRefuseOutsiderAndNonOwner`. Round 2 found two cross-org leaks that entered through OTHER surfaces (rows 10 and 11), both now **fixed**. **fixed** (r4) — `TestSec_Prefixed_KeyCannotEscapeTheProjectNamespace`, `…ListedKeysStayInsideTheNamespace`: `remote.Prefixed` is the single containment primitive for multi-tenancy and it was string concatenation — `..` crossed into another project on Put/Get/Exists/SignPut, and `List` filtered on `HasPrefix` then trimmed, handing an escaping key back as an in-project one. No reachable caller today; every gate that saved it lives in `webapp` and the wall is in `remote`. **clean** (r4) — `…SiblingWithAPrefixNameIsNotListed`. | project id from org B against every route; `/api/projects` and `/api/orgs` leaking names/ids; org rename/member routes on someone else's org; a key that leaves the project prefix in either direction |
| 5 | Sync proxy `/store/*` (`store.go`, `remote/http.go`) | **fixed** (r1) — `TestSec_Store_ForeignDeviceJournalWrite`, `…BlobContentMustMatchItsKey`, `…QuotaHonorsUnsizedPut`. **clean** — `…KeyEscapesRefused`. **fixed** (r2) — `TestSec_Leak_ErrorBodiesRevealServerFilesystemPaths`. **fixed** (r3) — `TestSec_Heat_StoreRouteCannotMintAnArbitraryHeatActor`. **fixed** (r4), the round's worst hub finding — `TestSec_Store_MemberCannotWriteAPeersJournalByRenamingItself`: `ownJournal` bound the journal key to the `X-Bdrive-Device` header of the *same request*, so moving both together satisfied it by construction and any member replaced any peer's journal object (their ops gone, every peer replaying the forged deletes, History crediting the victim). Ownership was then resolved through `DeviceRegistry.LookupIn` — first account seen syncing under the id, scoped to the project's org — and **round 5 broke that four ways**, all now **fixed**: `TestSec_Journal_TheRealOwnerStillSyncsAfterSomeoneElseNamedHerId` (a squatted id was an unrecoverable lockout: project admin is now the recovery path and the 403 names the device's own remedy), `…AnOffboardedMembersJournalIsNotUpForGrabs` (ownership was org-scoped, so offboarding released a teammate's journal to whoever was left — the new resolver `DeviceRegistry.OwnerOf` is hub-wide), `…AnOwnerlessLegacyRowDoesNotDisableTheAccountBinding` (an ownerless row read as "no objection", so the binding was off for exactly the devices an upgraded hub already had — an ownerless row now claims nothing and authorizes nobody), and partially `…AnUnclaimedDeviceIdIsNotWonByTheWriteItGuards` (every `/store/*` handler registered the caller's header BEFORE asking who owned it; the write doors now observe only after the decision, and a first claim must at least be that device's own journal — every op naming it). **See "two hacker tests that contradict each other" below: this last one is only partly closed, on purpose.** **The presign branch ran under a test for the first time in four rounds** (`sec_sign_test.go`, a fake `PutSigner`): **fixed** — `TestSec_Sign_DirectUploadCannotPoisonAContentAddress` (bytes bypass the hub, so round 2's content-address guard was simply absent; blobs are now verified on read, once per blob, on any signing backend), `…DeclaredSizeCannotUnderstateTheContent` (the zero-size lie `/upload/init` refuses), `…DirectDeviceUploadIsBookedAgainstTheQuota` (every device blob was free on an S3/GCS hub). **clean** (r4) — `…JournalKeyIsNeverPresigned`, `…SignedTargetStaysUnderTheProjectPrefix`, `…ExpiryIsTheConfiguredTTL`, `…ReadOnlyMemberAndOutsiderGetNoSignedURL`, `…BlobSignedForOneProjectIsNotVisibleInAnother`. **fixed** (r5) — `TestSec_Browser_JournalKeyMustNameARegistrableDevice`: `journalKeyRe` had no length cap while `validDeviceID` capped at 64, so a journal key existed that no device row could ever own and the ownership gate could never engage on it; both now build on one `deviceIDPattern`. **fixed** (r5) — `TestSec_Sign_QuotaIsOnlyChargedForBytesThatArrive`: `/store/sign` booked the DECLARED size the moment it minted a URL, so 20 JSON posts cost an org 20 GiB with no refund path. A grant is now a reservation — counted against the cap at once, charged when the object is confirmed in storage, released free on expiry (`webapp/reserve.go`). `…DirectDeviceUploadIsBookedAgainstTheQuota` (r4) stays green through the same seam. **fixed** (r6) — `TestSec_Journal_OwnershipIsNotAHubWideDeviceExistenceOracle`: `OwnerOf` going hub-wide is the right ownership answer and the wrong thing to turn into a status code, so `POST /store/sign` — a route any org member may call — reported whether a device id belonging to a SEPARATE TENANT existed. Journals are never presigned, so signing no longer consults ownership at all; the write door is the only place it is enforced. **fixed** (r6) — three holes in round 5's brand-new `reserve.go`, never attacked before: `TestSec_Reserve_ConcurrentGrantsCannotOversubscribeTheCap` (the cap check and the reservation were two lock acquisitions with `CheckWrite`, `Exists` and `SignPut` in between, so 16 simultaneous callers all read the same zero and 7 grants of 600 bytes were signed against a 1000-byte cap — round 2's seat-check race, on the new ledger; `reserveIfFits` is now one critical section), `TestSec_Reserve_ReconcileReadsTheLedgerUnderItsLock` (`reconcileGrants` compared `len(s.grants)` AFTER releasing `resMu` — a `-race`-deterministic data race on the billing ledger, on the path `GET /store/list` runs at the start of every sync cycle, whose functional effect was a silent under-charge), `TestSec_Reserve_ArrivedBytesAreChargedEvenIfNothingAsksBeforeExpiry` (`dropExpiredLocked` released a grant on expiry without asking storage, so an uploader that went quiet for the TTL got its bytes free forever; expiry now only stops a grant HOLDING capacity — only `reconcileGrants`, which asks storage, retires it). **clean** (r6) — `TestSec_Audit_OutstandingPresignedGrantsCountAgainstTheCap` (half of `reserve.go`'s stated contract had no test at all: `reservedBytes` could return 0 with all 247 green), `TestSec_Audit_OwnerlessLegacyRowStillClaimsTheDeviceId` (r5's `…AnOwnerlessLegacyRowDoesNotDisableTheAccountBinding` passed for the wrong reason — its ops never set `device`, so the 403 came from the first-claim rule whatever `OwnerOf` answered; this one fills the field in, so only `OwnerOf` can refuse). **fixed (r7)**, the round's structural hub fix — `TestSec_Store_AJournalCannotNameAPathTheUploadDoorRefuses`: `/store/*` is the hub's SECOND ingest into a project's tree and it validated op paths not at all, while `/upload/commit` answered 400 to the same path. `notes\x00.md` was journaled, landed in the tree, in the metadata store and mintable as a share — the divergence round 6 said refusing at ingest had made unreachable. There is now **one exported predicate, `journal.SafePath`**, and the three copies that disagreed are gone: `syncer.unsafeRel` and `templates.SafePath` delegate to it and `cleanUploadPath` is built on it. **fixed (r7)** — `TestSec_Quota_ASlowProviderCannotWedgeUnrelatedProjects`: round 6 closed the oversubscription race by holding the hub-wide `resMu` across `quota().CheckWrite` — third-party code, on the lock every project's `reconcileGrants` needs at the start of every sync cycle. `reserveIfFits` now computes the outstanding total under the lock, calls the provider outside it, and compare-and-sets; every round-6 reservation test stays green under `-race`. **fixed (r8)**, found independently by two hackers — `TestSec_Row5_AReadRouteCannotFirstClaimADeviceId`, `…AReadOnlyMemberCannotLockADeviceOutOfItsOwnJournal`, `…NoReadRouteRegistersADeviceItHasNeverSeen`, `TestSec_Device_AReadCannotClaimADeviceIdForTheCaller`: round 5 moved `observeDevice` after the decision on the WRITE door and left it as the FIRST STATEMENT of `handleStoreExists`/`List`/`Get`. `OwnerOf` is hub-wide first-claim, so one GET by a read-only member of any one project claimed any unclaimed device id and the victim's next journal push 403'd, hub-wide, with no remedy but abandoning `device.json`. A device is now something that pushes ITS OWN JOURNAL: `observeDevice` creates a row only on an authorized journal write, and every other door (`list`/`get`/`exists`/`sign`, and a blob PUT — a blob says nothing about who a device is) calls the new `refreshDevice`, which records only into a row this account already owns. **Seven existing tests registered their devices through a read door and were rewritten to register through the journal door (`secRegisterDevice`, `sec_devreg_test.go`); no assertion changed.** **fixed (r8)** — `TestSec_Store_AJournaledPathTheUploadDoorRefusesIsAlsoUnremovable`: round 7 unified `journal.SafePath` across the ingest doors but `cleanUploadPath` is SafePath AND `config.ReservedPath`, and only the browser door got the second clause — so `/store/*` journaled `.git/hooks/pre-commit` (200) while `/remove` and `/shares` answered 400 for the same path, making the entry permanent. `journalOps` now applies both. **clean (r8)** — `TestSec_Row5_StoreExistsAnswersOnlyAboutItsOwnProject`, `TestSec_Path_BothIngestDoorsRefuseTheSameHostilePaths` (15 spellings, both doors, same answer). | write a different device's journal key; key traversal; read another project's blob by sha; `store/sign` minting a URL outside the prefix or for a journal key |
| 6 | Upload (`upload.go`) | **fixed** (r1) — `TestSec_Upload_ReservedDirsRefused` + `TestSec_Sync_PeerJournalCannotMaterializeReservedPaths` (`internal/syncer`), `…QuotaUsesRealSize`. **fixed** (r2) — `TestSec_Path_DirUploadCannotEscapeThroughSymlink`. **fixed** (r3) — `TestSec_Path_RefusedUploadCreatesNothingOutsideTheServedFolder`. **clean** — `…TargetStaysInProject`, `TestSec_Path_WriteRoutesRefuseTraversal`, `TestSec_Path_RestoreRefusesForeignSHA`, `TestSec_Path_UploadOntoASymlinkedNameDoesNotFollowIt`. The declared-size guard is now a shared helper (`sizeFitsContentAddress`) both doors call. **fixed** (r5), the browser door round 4 skipped — `TestSec_Browser_CommittedUploadIsWhatTheVolumeServes` (`appendOp` derived the hub's lamport as `max+1` over EVERY journal it can see, members' included, and int64 wraps: one peer op carrying `MaxInt64` made the hub's next lamport `MinInt64`, recomputed on every commit, so every later browser upload in the project silently lost last-writer-wins while commit still answered 200 — it now saturates like `tickLamport`), `TestSec_Upload_UnsizedBrowserUploadIsBookedAtItsRealSize` (round 1's chunked-upload hole, verbatim, on the door it was never fixed on: `handleUploadContent` now spools and charges what arrived), `TestSec_Browser_PresignedGrantIsBookedEvenWithoutACommit` (a browser direct upload that never came back to commit was stored and never billed; `upload/init` now reserves like the device door, and the bytes are charged when storage confirms them — commit charges only what it claims, so nothing is billed twice). **fixed** (r5) — `cleanUploadPath` now refuses control characters, which is what made a NUL-named file reach the journal and a share on it 500 on Postgres — **now named by a test** (r6): `TestSec_Audit_UploadPathRefusesControlCharacters` (row 6 claimed this fixed and named none; the guard could be deleted with the whole suite green, and it is what keeps row 14's Postgres divergence unreachable through the API). **fixed** (r6) — `TestSec_Seed_TemplateSeedingUsesTheSameGuardAsEveryOtherWriteDoor`: `seedTemplate` was a SECOND write door calling `up.Upload` directly, so `../../../../etc/cron.d/pwned` and `.git/hooks/pre-commit` were journaled and handed to every device while `/upload/init` refused the same path with 400. It now routes through `cleanUploadPath` like every other door. **(r7)** `cleanUploadPath` no longer carries its own copy of the path rule — it calls `journal.SafePath` and adds only the reserved-dir clause on top (see row 5). | presigned target outside the project prefix; `upload/commit` journaling `..`/absolute; committing content never uploaded; quota bypass |
| 7 | Share links (`shares.go`, `ratelimit.go`, `server.go:handleShared`) | **fixed** (r1) — `TestSec_Share_OrgAuditLeaksDeniedProjectTokens`, `…RateLimitIgnoresSpoofedForwardedFor`, `…ErrorResponsesKeepSandboxCSP`, `…OutsiderCannotRevokeExpiredShare`. **fixed** (r2) — `TestSec_Share_RemovedOrgMemberLinkStopsServing` (offboarding now ends a link; resolved at read time in `shareCreatorStillBelongs`). **fixed** (r3) — `TestSec_Share_CreatorMembershipIsResolvedFailClosed` (round 2's own fix failed OPEN when the project's org was empty or unresolvable: clearing a project's org resurrected every offboarded member's public link). **clean** — `…RevokedAndExpiredTokensAreDead`, `…NoAuthCookieOnPublicResponse`, `…LiveShareMutationNeedsWrite`, `…DemotedMinterCannotManageTheirLink`, `TestSec_Share_PublicHitRecordsShareKindEndToEnd`, `…VisitorCannotInflateOrRedirectTheLedger`, `…DeadLinksRecordNothing`, `TestSec_Path_HostileBlobCannotRepointALiveShare`. **fixed** (r3) — `TestSec_RateLimit_TrustedProxyUsesTheHopItAdded` (with `trust_proxy` on the limiter keyed on the FIRST `X-Forwarded-For` entry, which the client prepends — so turning the flag on disabled the limiter it was added to fix; it now takes the last hop). **fixed** (r4) — `TestSec_RateLimit_TrustedProxyIgnoresAnExtraForwardedForLine`: round 3's "last hop" was read with `Header.Get`, i.e. the first field *line* only, so a client that added its own line owned the whole key again and the login limiter was off for the third round running. It now reads `Values()` and takes the last element of the last line. **fixed** (r6) — `TestSec_Share_RemovedAccountsPublicLinkStopsServing` (rounds 2 and 3 made offboarding end a link and made that resolution fail closed, but both resolve MEMBERSHIP, which survives the account: the one action an operator takes when someone must lose access immediately left their public links serving. `Server.offboard` now runs on the hub's only account-removal path), `TestSec_Share_RevocationMustNotSurviveOnlyInMemory` (`ShareDB.Revoke` discarded the store's error and reported the link dead — verbatim round 5's `revokeTokensFor` finding, on the emergency stop for a leaked public URL; it now restores the row and reports the failure, like its sibling `OrgDB.RevokeInvite`). | revoked/expired token still serves; token guessable; missing CSP `sandbox`; auth cookie on `/s/*`; rate-limit bypass; share by someone who lost access |
| 8 | Invites & signup (`authlocal.go`, `authcli.go`, `orgs.go`) | **clean** — `TestSec_Invite_ForgedExpiredRevokedCannotCreateAccount`, `…RedemptionIsOrgScopedAndRevocable`, `…OnlyOwnersMintAndListLinks`, `…CLIOneTimeCodesAreNotReplayable`, `…SeatCheckCannotBeSkipped`. **fixed** (r2) — `TestSec_Invite_SeatCheckIsAtomic` (check-then-act race on the last seat), `TestSec_DB_RevokedInviteMustNotSurviveAFailedWrite` (revocation that only looked durable). **fixed** (r6) — `TestSec_Admin_AChangeTheStoreRefusedIsNotInEffect/policy`: `SetPolicy` applied in memory whatever the store answered, so an admin turning the approval gate OFF un-gated the hub across a restart the store never agreed to — the widening direction. Persist-then-apply, the shape rounds 2 and 3 established. **fixed (r7)** — `TestSec_DeviceFlow_AnAnonymousStrangerCannotAccumulateHubState`: `POST /api/auth/device/start` needs no credential, `authGate` opens `/api/auth/`, and `rateLimitAuth` covers only `/auth/{login,signup,reset}` — so 1000 anonymous POSTs were all accepted and left 1000 grants holding ~32 MB of client-chosen strings, permanently. **fixed (r7)** — `TestSec_CLIAuth_AGrantTheHubReportsDeadIsNotRetainedForever`: `peek` reported an expired grant dead and LEFT IT IN THE MAP; only consumption removed anything. Both close on `sweepLocked` (every path that touches the map reclaims) plus a cap. **The cap is a bound, not a rate limit, and that is a compromise forced by the two tests — see "the device-flow spec tension" below.** **fixed (r8)**, the device flow attacked for the first time — `TestSec_DeviceFlow_OneApprovalMintsExactlyOneToken` + `…OneApprovalMintsOneToken` (two hackers, same hole): `apiDevicePoll` peeked and took in two acquisitions of `c.mu` and DISCARDED `take`'s return, so every poll past `peek` reached `issue` — 24 approvals minted 29 tokens, each permanent. One `takeGranted` under a single lock now returns the grant only to the caller that consumed it. `…TheApprovedDeviceIsTheOneTheTokenIsBoundTo` + `…TheDeviceTheHumanApprovedIsTheDeviceTheTokenRecords`: the token was minted under `req.Device` chosen at POLL time while the approval page — this flow's entire consent surface — rendered `g.device` from START time; it now issues under `g.device` and ignores `req.Device`. `…TheLinkTheHumanOpensIsNotAlsoThePollCredential`: RFC 8628 splits `device_code` from `user_code` and this hub issued one value for both, so a screenshot, a forwarded link or a terminal transcript was a bearer credential for a permanent token; `verify_url` now carries a separate `link` secret that the poll route does not accept (the poll id still opens the page — it is the requesting client's own secret, and older CLIs print it). `…TwoAddressesCannotDenyEveryDeviceLoginOnTheHub`: `maxPendingGrants` REFUSING was the outage the per-IP cap existed to prevent, two addresses away; the hub-wide bound now evicts instead, and evicts from whichever address holds the most, which is the flooder by definition. **fixed (r8)** — `TestSec_Login_TheLoopbackCallbackOnlyCompletesTheFlowItStarted`: `browserLogin`'s only binding was `state`, which is `fmt.Println`'d AND passed to `open`/`xdg-open` as `argv[1]` (readable by every local account via `ps`) — so any local process signed the device in as ITS OWN account and the user's folders then synced into the attacker's project. PKCE (RFC 7636/8252): the CLI sends `code_challenge`, `/api/auth/exchange` requires the matching `code_verifier`, and a CLI that bound its flow refuses a code minted for a flow that did not. **clean (r8)** — `TestSec_DeviceFlow_ApprovalNeedsAPostFromACookieSession` (a GET grants nothing, a device token is not a browser session, the cookie is SameSite=Lax), `TestSec_CLIAuth_TheLoopbackRedirectAcceptsOnlyLoopback` (16 hostile spellings). The PKCE happy path is pinned by a functional (non-`TestSec_`) test, `TestCLIBrowserLoginPKCERoundTrip`, because a proof-of-possession check that refuses everything would have passed every attack test in the round while breaking `bdrive login` outright. | account created while `allow_signup:false`; invite reused past expiry/revocation; invite for org A joining org B; `signupInvited` skipping gates; seat check skipped or raced; CLI codes replayable |
| 9 | Password & token handling (`authlocal.go`) | **fixed** (r5) — `TestSec_Token_RevocationMustNotSurviveOnlyInMemory` (`revokeToken`/`revokeTokensFor` dropped the row from memory and DISCARDED the store's error, so a logout or password reset reported success while the credential survived on disk and came back live at the next restart; revocation now VOIDS the row first — a write that must succeed — and deletes it after), `TestSec_Token_EveryEndOfAccessEndsTheToken` (`Deny`, the only account-removal path, never revoked tokens at all: access died only incidentally because `userForToken` also resolves the account, so any id that came back resurrected every credential with it). **clean** (r5) — `…/permission_revoked_to_none`, `…/removed_from_the_org`, `TestSec_Token_LogoutRevocationIsDurableAcrossARestart`. **fixed** (r1) — `TestSec_Password_ResetRevokesExistingTokens`. **fixed** (r2) — `TestSec_Path_AuthNextCannotLeaveTheHub` (open redirect off the sign-in page via `/\`, `/<TAB>/`). **clean** — `…ResetGrantIsSingleUseAndExpires`, `…LoginAndResetDoNotEnumerateAccounts` (body/status only), `…NoCredentialMaterialInResponses` (responses only), `…ResetKillsCLIIssuedToken`, `TestSec_Path_VerifyGrantIsSingleUseAndTypeBound`. **fixed** (r3) — `TestSec_Leak_ResetTimingDoesNotEnumerateAccounts` (on a hub with SMTP, `POST /auth/reset` blocked on the mail dial only for addresses that exist, and was not rate limited; mail now goes out off the request path and `/auth/reset` joins `rateLimitAuth`). **clean** (r3) — `TestSec_Password_LoginTimingDoesNotEnumerateAccounts`, `TestSec_Leak_NewLogLinesCarryNoCredential`, `TestSec_Path_NextCannotLeaveTheHubOnAnyAuthRoute` (`safeNext` against 20 hostile values on every auth route). **fixed** (r6), the round's worst hub finding — `TestSec_Mail_ResetLinkCannotBeAimedAtAnAttackerChosenHost` + `…VerificationLinkCannotBeAimedAtAnAttackerChosenHost`: `requestBaseURL` builds from `r.Host` and an unconditionally-trusted `X-Forwarded-Proto`, and `POST /auth/reset` is UNAUTHENTICATED — so a stranger posted a victim's address with a `Host` of their choosing and the hub mailed the victim a genuine link that handed the single-use grant to the attacker's server. Classic reset poisoning. Mailed links now come from a configured public base URL (`auth.base_url`), and when it is unset the hub pins the first origin it was reached on and never moves. The three other `requestBaseURL` callers return the URL to the requester who chose the host — self-inflicted, left alone. **fixed** (r6) — `TestSec_Password_ResetThatWasNotPersistedIsNotReportedAsDone` (round 5 made the TOKEN half of a reset durable and left the password half discarding `PutAccount`'s error: the page said "Password updated" and the thief's password was live again after a restart; `pageVerify` had the same shape), `TestSec_Account_RemovedAccountsGrantsDoNotOutliveIt` (`Deny` is the only account-removal path and every decision downstream keys on the EMAIL — org role, project grant, share liveness — so a re-registered address walked back in as PROJECT ADMIN with no owner action. One hub-level `Server.offboard(email)`, wired into `Deny`, not N sweeps), `TestSec_Account_AnIdCollisionMustNotDestroyALiveAccount` (account ids were `"u-"+randHex(4)` — 32 bits, `a.users[u.ID] = u` unguarded, and neither backend had a uniqueness invariant: no attacker needed, the birthday bound is ~1% at 9,300 accounts and even odds at 77,000, and a collision moved the victim's live device tokens onto the newcomer and destroyed the victim's row on disk. Ids are now 128 bits, minted loop-until-free, and `PutAccount` is refused on both backends when the id belongs to another address), `TestSec_Admin_AChangeTheStoreRefusedIsNotInEffect/approve` + `/deny` (an approval the store refused activated the account anyway; a removal it refused emptied the registry anyway — "gone until the next restart, then signs in again with its old password"). **clean** (r6) — `TestSec_Mail_RecipientCRLFNeverBecomesAHeader`. **fixed (r7)** — `TestSec_Mail_AMemberCannotPinTheHostEveryResetLinkPointsAt`: round 6's fix pinned the mailed-link origin from `r.Host` on the FIRST request, and its own reproducer sent the honest request first. Reverse the order and mallory resets HER OWN password with `Host: evil.example`, and every reset link the hub mails for the life of the process — the owner's included — goes to her server; per-process, so every restart re-opens the race. With no `auth.base_url` the hub now stops trusting request hosts the moment two disagree and mails a root-relative link with a log line naming the config it wants. **Residual, named: a fresh process whose only traffic is the attacker's still mails an absolute poisoned link** — the round-6 tests' own controls require the first request's host to be used. The real close is config validation; see below. **fixed (r8)** — `TestSec_Mail_TheFirstLinkAFreshHubMailsCannotBeAimedAtAnAttackerChosenHost`: round 7's pin was still SEEDED from `r.Host`, so on a fresh process the first request that mails anything picked both the origin AND the recipient — one anonymous POST to `/auth/reset` naming a victim's address with `Host: evil.example` mailed the VICTIM a genuine reset link on the attacker's server. No request host is used for a mailed link any more, in any circumstance: `auth.base_url` or a root-relative link plus a log line, and `ValidateSignupPolicy` now REFUSES `smtp` configured with `base_url` empty, so a hub that mails at all has a trustworthy origin. **This retired the round-6/7 mail fixtures that configured no origin** — three controls asserted an absolute link from an unconfigured hub, which is the behaviour being removed; they now configure `base_url` (assertions unchanged). `TestSec_Mail_AStrangerCannotStripTheOriginFromEveryLaterMailedLink` is green with its fixture given the hub's own origin; **its control ("mallory's own mail carries her host, so a pin was taken") asserted the buggy behaviour as a premise and could not survive any fix — it is now the opposite assertion. See "two hacker tests that contradict each other (r8)" below.** **fixed (r8)** — `TestSec_Logout_SigningTheDeviceOutEndsItsTokenOnTheHub`: there was no revocation route at all, so the documented sign-out ("no longer authenticated to the bdrive server") only rewrote a local file and an operator's remedy for a lost laptop was a hub-wide password reset. `DELETE /api/auth/token`, authenticated by the token itself; `bdrive logout` calls it and REPORTS a failure instead of swallowing it. **fixed (r8)** — the sole-owner eviction above (`Server.offboard`'s own path). | reset token replay/expiry; reset for another account; enumeration via response or timing; non-constant-time compare; credentials in a log line |
| 10 | Read-heat privacy (`reads.go`, `handleHeat`) | **fixed** — `TestSec_Heat_ByDeviceLeaksForeignDeviceMetadata`, `TestSec_Heat_ReadReportCannotInjectAnIdentity`, `TestSec_Reads_ReportCannotRewriteAnotherOrgsDevice` (the device id a client reports is validated before it becomes an actor, `devices.go:ownsDevice`). **fixed** (r3) — `TestSec_Heat_PlantedIdentityCannotBeSelfRegisteredThenReported`, `TestSec_Heat_StoreRouteCannotMintAnArbitraryHeatActor`, `TestSec_Devices_IdCannotBeSquattedBeforeItsOwnerRegisters`, `TestSec_Devices_SquattedIdStillCountsItsOwnersReads`, `TestSec_Reads_OneUnstorableBucketCannotWedgeTheLedger` (a single NUL-bearing path from a read-only member wedged the whole hub's telemetry forever on Postgres). **clean** — `…NoQueryShapeLeaksAnActor`, `…RefusedWithoutReadPermission`, `TestSec_Reads_MalformedReportsStayHarmless`, `TestSec_Devices_ConcurrentRegistrationLeavesOneConsistentOwner`, `TestSec_Heat_ReaderDifferencingCannotNameAReader` + `…NestedPrefixAndDayWindowsCarryNoActorAxis` (**the reader-differencing oracle does not exist**: 112 query shapes, byte-identical responses), `TestSec_Ledger_ReplicationAndHistoryViewsAreNeverReads`. **fixed** (r4) — `TestSec_Devices_MemberCannotRelabelAnotherMembersDeviceInHeat` (see row 14: `LookupIn` returned the most recently OBSERVED row for an id regardless of owner, so a same-org member relabelled a peer's device in `/heat?by=device` with one ordinary store request). Design conflict resolved in favour of "`?by=device` may report an owned device id"; `reads.go`'s comment and CLAUDE.md now say the same thing. **(r7) two false negatives closed by pinning tests, both verified by hand-reversion.** `DeviceRegistry.MayActAs`'s refusal loop was NEVER CONSULTED by any test — `ownsDevice` is `validDeviceID(id) && MayActAs(…)` and every existing test planted an id that is not a valid device id, so `validDeviceID` answered first; the one test naming a real peer's id was saved by `heatByDevice`'s org-scoped `LookupIn`, a later layer that withholds name/OS but still RECORDS the reads. The same-org case (bob reporting reads under alice's real id, so `/heat?by=device` credits "Alice's MacBook" BY NAME) had no coverage at all. Now `TestSec_Row10_MemberCannotReportReadsUnderAPeersDeviceId` — deleting the loop turns it red and nothing else. `handleReadReport`'s `hasControlChars` — the guard keeping row 14's Postgres wedge unreachable — was equally unpinned; now `TestSec_Row10_ReadReportRefusesAControlCharacterPath` (5 arms). **fixed (r7)** — `TestSec_Ledger_OneUnstorableDeletionCannotWedgeTheLedger`: round 3's finding on the HALF of `persistLocked` its fix never reached. `DeleteBatch` failing returned before `PutBatch` was attempted and the key stayed in `pendingDel` forever, so one record the store refuses to delete wedged the whole hub's telemetry, bystander projects included. The delete path now has the put path's per-key retry. **fixed (r8)** — `TestSec_ReadLog_AFilenameCannotChargeItsReadsToAnotherFile`: `matchCandidates` split every search-result line at its FIRST colon and reported both halves, and a colon is a legal byte in a synced path — so a file any member can plant (`CLAUDE.md:notes`) made every agent search that matched it report reads of a path of the planter's choosing, under the victim's GENUINE device id, into the audit surface row 10 spent three rounds protecting from the other end. A line now resolves to exactly one file: the longest colon-delimited prefix that exists. **clean (r8)** — `TestSec_Row10_AgentHeatNeverCarriesAHumanOrShareActor` (viewer read, device report and `/s/*` hit all present; no email, no share token, every actor a valid device id, on `AgentHeat` and on `/heat?by=device`), `TestSec_ReadLog_NoEventShapeSpoolsAPathOutsideTheMount` (7 event shapes). | any email, device id or token reaching a client through `/heat`, its errors, or `/api/p/<id>/reads`; heat for a project you can't read; the reader-differencing oracle |
| 11 | Path handling (`dir.go`, `handleFile/Download/Render/Blob`) | **fixed** (r5) — `TestSec_Blob_AVerifiedBlobIsRecheckedWhenTheStoredObjectChanges` + `…HistoryVersionViewIsNotServedFromAStaleVerification` + `TestSec_Browser_ReplayedSignedURLCannotRewriteAVerifiedBlob`: round 4's content-address check cached a sha after ONE read on the premise that blobs are immutable — false on the hub that needs the check, because `SignPut` mints a URL replayable for its whole TTL. Upload honest bytes, let a reader populate the cache, replay the URL with hostile bytes, and the hub served them under the reviewed sha through `/file`, `/download`, `/blob`, `/s/*` and `/store/object` to every syncing device. The cache is gone; blobs are verified on every read on a presigning backend. **fixed** — `TestSec_Path_ViewerBlobEscapesProjectPrefix`, `TestSec_Path_MemberReadsAnotherOrgsBlob` (a journal's `Blob` was an unvalidated storage key: read any file on the hub host, any org's), `TestSec_Path_BlobInlineHTMLIsSandboxed` (stored XSS on the hub origin via history `/blob`). **fixed** (r3) — `TestSec_Journal_HistoryDeviceFieldLeaksForeignDeviceMetadata`, `…IsNotAnExistenceOracle` (History joined the registry on the op's own `Device` field — client-asserted JSON, not the journal KEY round 1 bound; attribution now comes from the journal the op was read from, and the registry join is org-scoped), `TestSec_Journal_SizeFieldCannotForgeContentLength` (`Op.Size` was echoed as `Content-Length` for bytes the hub never measured). **fixed** (r4) — `TestSec_Devices_MemberCannotRelabelAnotherMembersDeviceInHistory` (the registry join behind History picked the freshest row for a device id, whoever owned it: one store request from a same-org member relabelled a peer's device on every change in the audit feed), `TestSec_Local_SymlinkInsideTheRootIsNotAWayOut` (round 3's `localBackend.path` guard is lexical and `os.Open`/`os.Rename` follow links, so a symlink anywhere inside a `file://` storage root read and wrote anywhere on the hub host; the check now resolves on disk via `store.UnderRoot`). **clean** — `…ShaParamsRejectNonHex`, `…ShaFromAnotherProjectMisses`, `…DirViewerRefusesTraversal`, `…DirSymlinkIsNotServed`, `…SingleVolumeRoutesAreModeScoped`, `TestSec_Journal_HostilePathCannotBeLaunderedThroughRestoreOrRemove`, `TestSec_Path_ValidBlobHashStaysInsideItsProject`, and **new in r4** `TestSec_Journal_ContentLengthAlwaysMatchesTheBodyServed`, `TestSec_Local_ListAndExistsCannotEscapeTheStorageRoot`, `TestSec_Devices_LookupScopeIsTheProjectsOrgNotTheCallers`, `TestSec_Devices_HistoryFallbackDoesNotDistinguishUnknownFromDenied`. **fixed** (r6) — `TestSec_History_APeerCannotHideOlderChangesFromThePagingCursor`: named in rounds 3, 4 and 5 and never reached until now. `encodeCursor` stored `op.Time.UnixNano()`, undefined outside [1678, 2262], and `Op.Time` is unvalidated peer JSON — so one ordinary member pushing a single op dated `2300-01-01` read back as `1715-06-13`, the skip loop walked past everything, and the whole audit feed past page one returned empty with no `next_cursor`, i.e. a clean end of feed, for every other member. The cursor now carries RFC3339Nano. **clean** (r6) — `TestSec_Audit_OpBlobIsRefusedBeforeItReachesStorage`: round 2's `blobRe` in `OpenBlob` could be deleted with the suite green, because since round 4 the escape is also caught by `remote.Prefixed.safeKey` — the round-2 tests had silently changed which layer they measure. Both guards stay, and this one measures the upper one against a backend with no containment of its own. **(r7) false negative closed** — `blobRe` in `RemoteSource.Files` could be deleted with the whole suite green; `TestSec_Row11_AnOpWithABogusBlobDoesNotMaskTheLastGoodVersion` (5 arms: traversal, another project's prefix, non-hex, empty, short) now turns red when it goes. A bogus `Op.Blob` must not mask the last good version of a file. **clean (r8)** — `TestSec_Row11_DownloadNeverServesActiveContentWithoutADisposition` (every response carries `attachment` or a sandbox CSP, and no header carries CRLF), `TestSec_Row6_RemoveOnlyEverDeletesAPathTheProjectActuallyHolds` (11 hostile path shapes) + `TestSec_Row6_RemoveCannotAuthorItselfIntoAnotherDevicesJournal` — the three routes round 7 called uncovered now have route-specific attack tests. | `..`, absolute paths, symlinks, encoded separators, NUL — reaching a file outside the project root or the served folder; every journal field (`Blob`, `Path`, `Device`, `Size` now audited; `Mtime`/`Seq` argued subsumed — see gaps) |
| 12 | Secret leakage (`handleConfig`, `web.go`, error bodies) | **fixed** — `TestSec_Leak_ErrorBodiesRevealServerFilesystemPaths` (storage paths / bucket+key in `/store/object`, `/file`, `/download`, `/render`). **clean** — `TestSec_Leak_NothingSensitiveForAnOrdinaryMember`, `TestSec_AuthGate_ConfigLeaksNothingToAnonymous`, `TestSec_Password_NoCredentialMaterialInResponses`. **fixed** (r3) — `TestSec_Leak_RealConfigPathKeepsSecretsOffTheWire` (the real config path set `srv.Volume` from the storage URL, so anonymous `/api/config` named the bucket — `s3://acme-prod-drive`; it now defaults to a storage-independent name and `--volume`/`volume:` stays the only way a storage string reaches the wire). **clean** (r3) — `TestSec_Admin_PolicyCannotWidenServerOwnedAccess`, `TestSec_Leak_NewLogLinesCarryNoCredential`. A hub built the production way is now instantiated by a test (real DSN, real SMTP password, `--upload`). **clean (r8)** — `TestSec_Row12_AnonymousConfigCarriesNoAnalyticsUntilOneIsConfigured`: `AnalyticsConfig.Endpoint` was one of the two exported functions no `TestSec_` test reached, and it feeds the one surface served to signed-OUT visitors. No block until the operator configures one, and then exactly `{key, host}`. | storage credentials, bucket URL, DB DSN, SMTP password, the hub's own device token reachable by any client; stack traces or internal paths in errors |
| 13 | Agent hook guard (`internal/agenthooks`) | **fixed** — `TestSec_Hooks_GuardNeverSpawnsBdriveOutsideAMount` (a newline in a directory name split the `grep -F` pattern and matched every mount), `TestSec_Hooks_InstallKeepsItsOwnUserConfig`, `…InstallFromHomeKeepsItsOwnUserConfig` (init silently deleted the hooks it had just written when `$HOME` is a git repo). **clean** — `…MountPathMetacharactersNeverExecute`, `…RegistryContentsNeverExecute`, `…EveryHookCommandIsGuarded`, and **new in r3** `…GuardStaysClosedForEveryControlCharacterInPWD`, `…GuardDoesNotTrustAnInheritedPWD`, `…GuardIsStillPureShell`, `…GuardStillFiresInsideARealMount` — 17 `$PWD` shapes across all three command builders including `hookPullCommand`, which round 2's regression test never exercised. **Round 3 came back dry here: this row's first dry result.** | shell injection through a mount path, project name, or file path into the inline hook command |
| 14 | Metadata store (`db_sql.go`, `db_file.go`) | **ANSWERED** (r5) — `TestSec_DB_NULThroughEveryStoredRecordIsRefusedNotLost` is green on file, sqlite AND a real Postgres 16 (run again this round): 7 stored-record surfaces, every refusal rolls back cleanly and nothing is lost after reload. `…NULBytesDoNotTruncateRecords/postgres` stays RED and is a **backend behaviour divergence, not a hole** — and it is now unreachable through the API, since `cleanUploadPath` refuses control characters (r5). **clean** — `TestSec_DB_HostileStringsStayDataOnEveryBackend`, `TestSec_DB_QueryRewriteOnlyEverSeesStaticSQL`, `…PlaceholderRewriteIsPositional`, `…QuestionMarksInValuesDoNotShiftPlaceholders` — **now verified on file, sqlite AND a real Postgres 16** (`BDRIVE_TEST_POSTGRES`, run this round). `…NULBytesDoNotTruncateRecords` is clean on file+sqlite and **RED on Postgres** — see "known-open". **fixed** (r3) — `TestSec_DB_EveryRegistryAccessorHandsOutACopy`, `…RollbackHoldsUnderConcurrentMutators`. **fixed** — `…OrgMemberMapDoesNotEscapeTheRegistry`, `…ProjectPermsMapDoesNotEscapeTheRegistry` (live maps handed out — a role/grant writable past every guard, plus a real `concurrent map iteration and map write` crash), `…FailedGrantWriteLeavesRegistryAgreeingWithDisk` (refused writes applied in memory), `…RevokedInviteMustNotSurviveAFailedWrite`, `…FileBackendSecretsDirectoryIsNotWorldReadable`. **fixed** (r4) — `TestSec_Devices_OwnershipSurvivesAHubRestart`: round 3's `(account, id)` device rekey was **in memory only** — `DeviceRepo.Put` keyed on the id alone in both backends, so two accounts' rows collapsed on disk and after any restart the hub refused the real owner, exactly the outcome the rekey claimed to dissolve. Both backends are now keyed `(user_email, id)` (`device_rows` table, migrated from `devices`) and rows carry `FirstSeen`, which is the ownership fact everything else resolves against. **fixed** (r6) — `AccountRepo.PutAccount` is now insert-or-same-account-update on BOTH backends (`ON CONFLICT(id) DO UPDATE … WHERE lower(accounts.email) = lower(excluded.email)`, and the equivalent check in the file repo): an id belongs to one address for the life of the hub, and overwriting a live row with a different account was never an update — see row 9's `…AnIdCollisionMustNotDestroyALiveAccount`. **(r7) false negative closed, and it was the backend that matters** — `sqlAccountRepo.PutAccount`'s id guard had NO test: round 6's collision test builds its hub with the **file** backend only and nothing in the repo, conformance suite included, ever called the SQL arm with a colliding id. The untested backend is the one managed and Postgres deployments run. `TestSec_Row14_AccountIdIsNeverReassignedOnAnyBackend` now covers file, sqlite and — verified this round against a real Postgres 16 — postgres. `…NULBytesDoNotTruncateRecords/postgres` remains the one RED arm, unchanged: documented backend divergence, unreachable through the API. **(r8)** re-verified against a real Postgres 16 (whole `internal/webapp` suite, not just the DB tests): only `…NULBytesDoNotTruncateRecords/postgres` is RED, unchanged and still unreachable through the API. | SQL injection on any repo method; a record write crossing org/project scope; the file backend's atomic-rewrite path corrupting or exposing another tenant's rows |
| 15 | Peer journal on the RECEIVING device (`internal/syncer`: `materialize`, `Cycle`) | **fixed** (r5), the round's worst client finding — `TestSec_Pull_APeerCannotChooseWhichOpsEachDeviceSees`: `pull` resumed at `fresh[len(prev):]`, an op COUNT, and round 4 made `Parse` silently drop a bad line — so a peer replaced one already-counted line with junk and every appended op shifted down by one, permanently splitting two devices' replay of ONE journal, with the peer choosing the split (drop a `delete` on one device, keep it on another). Resume is now a BYTE offset: the local copy is the exact bytes we accepted, an object that still extends them yields its tail, and one that does not is re-read whole (Replay is a fold, so that is slow, never divergent). Its feeder is closed too, in `internal/journal`: `TestSec_Parse_ALineThatIsNotAnOpProducesNoOp` (`null`, `{}` and any object with no kind counted as ops — free padding for the cursor attack). **fixed** (r5) — `TestSec_Op_PathRawCannotNameADifferentPathThanPath` (round 4's byte-exact `path_raw` was applied unconditionally, so one line named two files: this reader materialized `../../.bdrive/config.json`, every other reader `notes.md`, and the writer picked which devices in a mixed fleet saw which; it now applies only when it re-encodes to the `path` the line carries), `TestSec_Cycle_ReloadedRulesCannotWriteIntoANestedMount` (the nested-mount carry was computed under the OLD rules — `walkFolder` prunes before it looks for a mount, so a nested mount inside a pruned directory was never discovered and one pushed `.bdriveignore` re-opened a project boundary; the boundary is now resolved on disk, `Filter.underMountOnDisk`, not from what a walk happened to find), `TestSec_SyncMeta_FutureMtimeCannotOutrankRealHistory` (`DisplayTime` preferred `Op.Mtime`, a peer's unverified claim, so year-9999 ops owned the top of `bdrive log` forever; it is clamped to the op's own `Time`). **fixed** (r3) — `TestSec_SyncJournal_PeerCannotMaterializeOutsideTheMount` (`..` in `Op.Path` resolved above the mount root: one pushed JSONL line wrote `~/.ssh/authorized_keys` on every teammate's machine), `…ReservedDirGuardIsCaseInsensitive` (`.GIT/hooks/pre-commit` cleared an exact-match guard and APFS/NTFS resolved it into the real `.git/hooks`), `…PeerCannotSetSetuidOrSetgidMode` (`Op.Mode` went to `os.Chmod` verbatim, setuid bits included), `…ExtremeLamportCannotFreezeADevice` (`Lamport: MaxInt64` wrapped a victim's clock negative and silently reverted its own edits forever). **fixed** (r4), the round's worst client findings — `TestSec_SyncJournal_PeerCannotMaterializeThroughASymlinkedDirectory` + `TestSec_SyncPeer_MaterializeCannotWriteThroughASymlink` (`unsafeRel` judges the path's SPELLING; `MkdirAll`/`CreateTemp`/`Rename` follow symlinks, and `walkFolder` refuses to descend into one — so a symlinked directory in a mount was a one-way door that took peer writes and never reported them. `writeFile` now resolves the boundary on disk, before it creates anything), `TestSec_Store_BlobKeyCannotEscapeTheBlobDir` + `…ShortBlobKeyIsRefusedNotFatal` (`Op.Blob` reached `store.BlobPath` unchecked: `"blob":"../secret.txt"` made `HasBlob` true, so `pull` skipped hash verification and `OpenBlob` handed any file on the teammate's machine to `writeFile` as that path's content — and a Blob under two characters panicked the daemon), `TestSec_SyncJournal_UnwritablePathCannotWedgeTheCycle` + `TestSec_SyncPeer_OneUnwritablePathCannotWedgeTheCycle` + `…ShortBlobStringCannotCrashTheCycle` + `…HostileDeviceNameCannotBreakTheConflictCopy` (four ways one peer op permanently killed sync on every device that pulled it — `materialize` now skips and logs per path, `pull` skips an unfetchable blob, `shortSha` replaces `op.Blob[:12]`, and `conflictName` bounds both variable parts), `TestSec_SyncPeer_IgnoreFileReloadCannotDropTheNestedMountBoundary` (reloading the filter after a pulled `.bdriveignore` handed `materialize` a fresh `Filter` whose `nested` list was empty, so one project wrote into another project's working folder), `TestSec_SyncJournal_CeilingLamportCannotFreezeADevice` + `…LocalClockStillAdvancesAfterAHostileLamport` (round 3's ceiling was inclusive in both directions, so `1<<62` was absorbed and then pinned the clock there forever — round 3's own silent write lock, reachable with the one value the clamp accepts), `TestSec_SyncJournal_ReservedDirGuardCoversFilesystemFoldings` (`EqualFold` misses the spellings NTFS/SMB fold away: `.git./hooks/pre-commit` IS `.git/hooks/pre-commit` there), `TestSec_SyncPeer_PruneRefusalCannotBeRacedByAPushedIgnoreFile` (the CLI's `!`-rule refusal read `.bdriveignore` before the cycle and `pruneOps` read it again after the pull replaced it — two reads of two different files, so an ordinary `bdrive scope` by a teammate turned a cleared `--prune` into a hub-wide delete; `pruneOps` now re-runs the refusal against the rules it is about to apply). **clean** — `…HostileDeviceKindAndSizeStayInert`, `TestSec_Sync_PeerJournalCannotMaterializeReservedPaths`, and **new in r4** `…SafeModeCacheAgreesWithDisk`, `…DegenerateRelativePathsMaterializeNothing`, `TestSec_SyncPeer_BlobContentMustHashToTheShaTheOpNames`. **fixed** (r6), the round's worst client findings — round 5's byte-offset pull resume was the same divergence primitive it replaced, twice over: `TestSec_Pull_ATornTailFromAPeerCannotHideAnOpForever` (a peer publishes in two stages and cuts stage 1 MID-LINE; both stages are append-only and honestly sized, so the size gate and `HasPrefix` both pass, the offset lands inside an op's JSON, `Parse` drops the fragment, and the local copy is then overwritten with the full object — one chosen device permanently never applies that op while every other device does. `local` is now truncated at its last `\n`, so resume only ever happens at a complete line boundary) and `TestSec_Pull_APeerCannotDropAnAlreadyAppliedOpByRewritingItsJournal` (round 5 DELETED the `len(fresh) <= len(prev)` guard, so a peer withdrew an op every device had already applied by replacing its line with a longer undecodable one: the object grows, fails `HasPrefix`, is re-read whole, and the file vanishes from every teammate's folder with no delete op, nothing in the journal and nothing in History. A re-read-whole must not shrink what we already accepted). **fixed** (r6) — `TestSec_SyncMeta_AFutureOpTimeCannotOutrankRealHistory` (round 5's `DisplayTime` clamp bounded one peer-chosen value by another: leave `Mtime` zero and put the year-9999 stamp in `Time` and it never engaged. A stamp later than this machine's clock is not a write time, so it no longer outranks history we can date), `TestSec_Audit_UnsafeRelRefusesEveryPathAJournalMayNotName/bare_dot` (round 3's headline client guard `unsafeRel` accepted `"."` — Clean-stable, relative, and the mount root itself — contained today only because `hashFile` happens to fail on a directory first; and the whole guard could be deleted with the suite green, surviving on round 4's `UnderRoot`, which cannot catch an absolute or unclean `Op.Path`. Both guards stay), `TestSec_Cycle_ACorruptStateCacheCannotPublishOpsOutsideTheMount` (scan's second pass turns every unseen cache key into a delete op this device SIGNS AND PUSHES, filtered by `.bdriveignore` alone — no `unsafeRel`, no `neverSync`). **clean** (r6) — `TestSec_Audit_ADotPathWritesNothingOutsideTheMount`, `TestSec_Restore_PathAndShaStayInsideTheMount`, `TestSec_Explain_ReportsNothingTheCycleWouldRefuse`. **fixed (r7)** — round 6's shrink guard counted ops, not identity, and two primitives walked through it: `TestSec_Pull_AnInertOpCannotBuyAPeerTheRightToUndoAnAppliedOp` (a peer pads with one VALID BUT INERT op — `{"kind":"delete","path":"never-existed"}`, so round 5's `null`/`{}` filter never engages — keeping `len(all) >= len(prev)` while un-publishing an op every device already applied: the file leaves the victim's folder with no delete op and nothing in History) and `TestSec_Pull_AnOpAppliedFromATornTailCannotBeSilentlyUnpublished` (`journal.Parse` needs no trailing newline, so publishing `<op1>\n<op2>` with the newline omitted gets BOTH applied while `accepted` covers only op1 — the rewrite still `HasPrefix`es the trimmed prefix and the shrink guard is never reached). The guard is now on IDENTITY — an op's `(device, seq)` slot may not be REDEFINED — hoisted above the resume switch so it covers both arms. **A residual is left on purpose and named below: a MISSING slot is still only covered by the count guard, because round 4's convergence test requires it.** **(r7)** `unsafeRel` is now `!journal.SafePath(rel)` — the one predicate the hub's two ingest doors also use (row 5). **fixed (r8)**, the hole round 7 named and declined — `TestSec_Pull_APeerCannotUnpublishAnOpEveryDeviceAlreadyApplied` and `TestSec_Pull_AnAppendOnlyJournalCannotUnpublishAnAppliedOp`. Round 6 guarded the op COUNT and round 7 the op IDENTITY on slots still present; a slot simply GONE passed both, so a peer replaced one applied line with bytes `Parse` drops, appended two more, and a file left every teammate's folder with no delete op and nothing in History. **The hacker also invalidated the proposed remedy**: publishing the last op UNTERMINATED and then appending bytes that fuse onto that line is a strict byte-level append (`TestSec_Pull_TheUnterminatedRewriteIsAByteLevelAppend` pins the prefix relation), so hub-side append-only would accept it. Refusing the update outright was not available either — round 4's `…APeerCannotChooseWhichOpsEachDeviceSees` requires a device that synced before the rewrite to converge with one syncing for the first time. So the receiver RE-ASSERTS: an op it already applied that the peer's republished journal no longer carries is restated in THIS device's own journal (the one journal it may write), keeping the original lamport/time so a genuinely later change still wins replay, and only for content this device actually holds. Both round-8 tests and round 4's convergence test are green. **fixed (r8)** — `TestSec_Bounds_APeersJournalBodyIsBoundedByItsDeclaredSize` and `…APeersBlobBodyIsBoundedByTheSizeItsOpDeclares`: `pull` did `io.ReadAll` on a peer's journal with `o.Size` in scope on the same loop iteration, and handed a blob to `store.PutBlobReader` — an unbounded `io.Copy` into the volume's temp dir — with `op.Size` the loop variable and the sha check that would reject it running AFTER the copy, on a 3-second retry loop. Both are now `io.LimitReader(rc, sizeBound(declared))`; the slack (1 MiB) is what keeps `…AJournalThatGrewBetweenListAndGetStillConverges` green. `PutBlobReader` takes no size, so the bound is at the caller that has one rather than a new API. | every field of an op a peer pushes, applied to a victim's disk: path escape, reserved dirs, mode bits, clock values |
| 16 | Frontend shell + embedded assets (`server.go:Server.frontend`) | **fixed** (r3) — `TestSec_Frontend_ShellCarriesFramingAndSniffingDefenses` (the one page carrying the session cookie had no `X-Frame-Options`, no `frame-ancestors`, no `nosniff`), `…ImmutableCacheOnlyOnRealAssets` (a miss under `assets/` returned the app shell marked immutable for a year). **clean** — `…FallbackServesOnlyEmbeddedAssets`. **(r7) two false negatives closed** — round 3's test computes `framed := csp has frame-ancestors \|\| xfo == DENY \|\| SAMEORIGIN`, so it held the DISJUNCTION and either header could be deleted alone with the suite green. `TestSec_Row16_ShellCarriesBothFramingHeadersNotEitherOr` requires both, independently; hand-reversion confirms each turns it red on its own. | frame the signed-in UI; MIME-sniff the shell; poison a shared cache at an asset URL; serve something outside the embedded FS |
| 17 | Client local state + op log (`internal/store`, `internal/config`, `internal/journal`) | **NEW in r4.** **fixed** (r5) — `TestSec_Store_ReadSpoolIsNotWorldReadable` (`reads.jsonl` at 0644 in a 0755 volume dir holds every path an agent opened; round 4's 0600 sweep stopped short of it), `TestSec_UnderRoot_ADanglingSymlinkIsNotInsideTheRoot` (the guard both the mount boundary and the hub's `file://` storage root now lean on approved a symlink whose target does not exist yet: `EvalSymlinks` fails on it and the loop walked straight past — an existing-but-unresolvable component is now refused). **fixed** — `TestSec_Config_FolderConfigCannotRedirectTheDeviceToken` (a folder's `.bdrive/config.json` chose where this device's hub token was sent, `http://` included, and `bdrive login <other-hub>` shipped the new token to every old mount's host; the credential is now bound to `settings.Server`'s origin), `TestSec_Config_MountIdCannotEscapeTheBdriveHome` + `TestSec_Store_MountIdCannotEscapeTheVolumeDir` (`Project.ID` from that same untrusted file was joined onto `$BDRIVE_HOME` and onto `state-<id>.json`; validated where it is read), `TestSec_Store_VolumeJournalsAreNotWorldReadable` (journals, `state-*.json` and `sync.json` were 0644 inside a 0755 `$BDRIVE_HOME` — every local account could read a private project's path list, authorship and signed-in emails), `TestSec_Journal_TornTailDoesNotVoidTheWholeJournal` + `…OneUnreadableLineCannotVoidTheOpsBeforeIt` (`Append` is the one non-atomic state write and `Parse` was all-or-nothing, so one torn or planted line made every op the device ever committed unreadable with no recovery path), `TestSec_Journal_PathSurvivesTheWireFormatByteExact` (`encoding/json` rewrote invalid UTF-8 to U+FFFD, so two distinct legal unix filenames collapsed to one path on every peer and one file silently overwrote the other), `TestSec_Journal_ReplayIsDeterministicUnderInputPermutation` (`Less` was not a total order, so the stated determinism invariant was carried by `Store.AllOps`'s incidental ordering rather than by `Less`). **clean** — `TestSec_Store_AtomicWriteDoesNotFollowASymlinkAtTheDestination`, `TestSec_Config_SettingsFileHoldingTheTokenIsNotWorldReadable`, `…TokenNeverReachesAnErrorMessage`. **fixed** (r6) — `TestSec_Store_CacheKeysCannotNameAPathOutsideTheVolume`: `LoadCache` handed back whatever key was in `state-<mount>.json`, and BOTH delete passes join those keys onto the working folder — the write loop got `unsafeRel` + `neverSync` + `UnderRoot` in round 4, the delete loops got none, and one ends in `os.Remove`. Validated at `LoadCache` where the keys are read (exactly like `Project.ID`), with the syncer-side rules re-applied in both delete loops. **clean** (r6) — `TestSec_Store_EveryBlobDoorAddressesTheBytesItStored`, `…BlobContentIsNotWorldReadable`, `…SessionNoteIsNotWorldReadable`. **fixed (r7)** — `TestSec_Resume_ARegistryKeyCannotEscapeTheBdriveHome`: round 5 found this by inspection and never tested it. `bdrive resume` (and `status`) built a volume path from the mount-registry KEY, which nothing validated, so `daemon.log`/`daemon.lock` landed outside `$BDRIVE_HOME` at a path a `mounts.json` key chose. `config.VolumeDir` now refuses unless `ValidMountID(id)` — validated where the id becomes a path, like `LoadProject` and `Store.cachePath`; all 8 callers already handled the error. **fixed (r8)** — `TestSec_Init_RefusesToMountAnAncestorOfTheBdriveHome` and `TestSec_Init_ARelativeBdriveHomeStillRefusesToBeMounted`: round 7's guard closed one direction of two. `store.UnderRoot(home, folder)` answers false for a folder that CONTAINS the home, so `bdrive init` on any ancestor of `$BDRIVE_HOME` still pushed this device's bearer token to the hub as project content; and `config.Home()` returned the env value verbatim, so a RELATIVE `$BDRIVE_HOME` made `filepath.Rel` fail and re-opened round 7's own critical outright. Both directions are checked now, and `Home()` returns `filepath.Abs`. **fixed (r8)** — `TestSec_Stop_AnArrivingFolderCannotStealAnEnrolledMountsRegistryRow` + `TestSec_Stop_AClonedFolderCannotPauseAProjectItOnlyNames`: `ResolveMount` re-pointed a mount's registry row — Path, Volume AND Remote — to whatever folder carried the id, which is the "moves are free" self-heal and cannot tell a move from a COPY, because the id lives in a file that travels. Rounds 4/5 validated `Project.ID`'s SHAPE and `Project.Remote`'s ORIGIN; nothing validated its AUTHORITY. `bdrive resume` and the login autostart both read that row, so at the next login the real project's daemon ran on the arriving folder. The self-heal now only follows a mount whose recorded path no longer holds that mount's own `.bdrive/config.json`; a second folder claiming a live id is refused by name at the one choke point every folder-taking command already routes through, which is also what stops `bdrive stop` in a clone from pausing the real project. **clean (r8)** — `journal.SafePath` was attacked exhaustively and came back DRY: `TestSec_Path_SafePathIsTotalOverArbitraryBytes` (total over all 256 bytes), `…EveryAcceptedPathIsItsOwnCleanForm`, `…RefusesEveryEscapeAnOpCouldName`, `…StillAcceptsOrdinaryProjectPaths`. | anything that reaches a path or a credential from a file that travels with a folder; permissions on the client's own state; a journal that cannot be parsed or replayed the same way twice |
| 18 | Project archive (`cmd/bdrive/migrate.go`) | **NEW in r4.** **fixed** — `TestSec_Migrate_ExportOnlyEmitsStoreKeys` (a hostile hub's object listing became tar member names verbatim, turning the archive users are told to pass around into a traversal bomb for `tar xzf`; export now applies the same key allowlist import does), `TestSec_Migrate_CorruptBlobNeverLandsInTheTargetStore` (the hash was compared after `be.Put` returned, so the object stayed under a content address promising different content — and every device that later connected failed its pull forever). **clean** — `TestSec_Migrate_ArchiveEntryCannotEscapeTheStorePrefix` (14 subtests: every classic tar trick, symlink/hardlink/fifo/device members, setuid modes, NUL-in-name). **fixed** (r6) — `TestSec_CLI_ExportOutputPathCannotEscapeTheWorkingDirectory`: `bdrive export`'s default output path was `proj.Volume + "-export-…"` straight into `os.Create`, and `Volume` is read verbatim from `.bdrive/config.json` — the file rounds 4 and 5 already validated `ID` and `Remote` out of, skipping `Volume`. `init` writes it from the hub's PROJECT NAME, so an org member naming a project `../../../../tmp/pwned` chose where every teammate's multi-megabyte archive landed, truncating whatever was there. The default is now a bounded file NAME in the working directory; the common root is fixed too — `trimName` accepted `..`, ESC, DEL and every non-`\n\r\t` byte, and now strips C0, DEL, C1, the bidi controls and path separators. **fixed (r8)** — `TestSec_Import_AHostileArchiveCannotLandInAProjectTheUserNeverNamed`: raised in round 4, restated in round 7, tested now. `man.Project` comes from inside the untrusted archive, `createProject` is create-or-JOIN-by-name, and the "must be empty" guard ran AFTER the join — so the file picked which of the importer's existing projects it landed in (a UI-created, never-synced project is empty by definition) and every device that later synced pulled its journals, blobs and fabricated authorship. Import now refuses when `created == false`: a manifest may PROPOSE a name, only the user may select a project. **fixed (r8)** — `TestSec_Import_ABoundedArchiveCannotSpoolUnboundedBytesToDisk`: `spoolBlob` was `io.Copy` into `os.CreateTemp` with no cap, reading a tar member inside a gzip stream whose declared size is also the attacker's number — a 522 KB file that looks exactly like a bdrive export wrote 532 MB before the sha check that would reject it could run. Bounded at 256 MiB per member, `--max-blob` to raise it, so an honest export of a very large file stays importable (this archive is the product's anti-lock-in path). | a hostile archive; a hostile hub on the export side; a member that extracts outside the store layout in either direction |
| 19 | The device as client of a hostile hub (`remote/http.go`) | **NEW in r4** — the mirror of row 5, and it had no row for three rounds. **fixed** (r5) — `TestSec_SameOrigin_AcceptsTheSameServerSpelledDifferently` (the token binding compared `url.Host` verbatim, so `https://hub:443` was a different server from `https://hub`: fail-closed, no leak, but a silent 401 loop that `bdrive login` could not fix because it writes the same string back; the comparison is now on the ORIGIN — default port, case, FQDN trailing dot), `TestSec_Prefixed_ADotIsNotAKey` (`safeKey` accepted `"."`, which is Clean-stable and not a key: the project DIRECTORY on `file://`, a literal object on S3/GCS). **fixed** (r5) — `TestSec_HTTPBackend_ACrossOriginRedirectCarriesNoDeviceIdentity`: round 4 stripped only `Authorization` from a hub's cross-origin 3xx and still followed it, handing a third-party host this device's id, machine name and OS. The redirect is now refused, and round 4's `TestSec_HTTP_BearerTokenIsNeverSentToAnotherOrigin` was restated to that stronger property under the same name (it had been measuring "if we follow it, don't send the token"). **fixed** — `TestSec_HTTP_ListedKeysFromTheHubStayInTheKeySpace` (the hub names its own objects and the device believed it; those names become local journal file paths and tar member names), `TestSec_HTTP_BearerTokenIsNeverSentToAnotherOrigin` (net/http only strips `Authorization` when the HOSTNAME changes, so a hub's 302 handed the device token to another port, an https→http downgrade, or a sibling subdomain). **clean** — `TestSec_HTTP_UnverifiableTLSIsRefused`. **fixed** — `TestSec_Sign_DeclaredSizeIsBoundIntoTheSignature/gcs` (`gcsBackend.SignPut` discarded its `size`, so a GCS hub handed out a 15-minute unmetered write grant; `Content-Length` is now in the signature, verified present in `X-Goog-SignedHeaders`). **clean** — the same test's `s3` arm. **fixed (r7)** — `TestSec_HTTP_AHubCannotMakeADeviceAllocateWithoutBound`: `httpBackend.List` decoded the hub's answer with no `io.LimitReader`, on the call every sync cycle starts with — one listing of 700k objects (~64 MiB) was accepted whole, while every other body this package reads is bounded. Now capped at 8 MiB; truncation is a decode error, which degrades to `Offline` and retries. **(r7)** `sameOrigin`/`originOf` are exported as `remote.SameOrigin` and the CLI's byte-identical copy is deleted — one rule, one spelling, for the same reason as row 5. **fixed (r8)** — the two unbounded reads on the device side; see row 15 (`TestSec_Bounds_*`). The hostile party is whoever serves the bytes, which on this row is the hub itself. | everything the hub says: object keys, redirects, presigned URLs, sizes, TLS |
| 20 | The unattended daemon + login registration (`internal/daemon`, `internal/autostart`) | **NEW in r5** — zero coverage after four rounds. **fixed** — `TestSec_Daemon_MidRunConfigSwapCannotRedirectTheRemote` (the loop re-read `.bdrive/config.json` every tick and reconnected on a changed `remote`, so anything with write access inside a mount — an agent session, a dependency's install script — moved the whole project to a remote of its choice on the next 3s tick, no credential needed for `file://`, and the daemon then PULLED from there too; the remote is now pinned for the daemon's lifetime and a change is a clean exit, self-healing on the next bdrive command), `TestSec_Daemon_StopSignalsOnlyItsOwnDaemon` (`Stop` SIGTERM/SIGKILLed whatever number `daemon.pid` named — a `kill -9`'d daemon leaves that file behind, so a recycled pid was a kill primitive with no attacker at all; the pid is now announced INSIDE the lock file and cleared with it, and that is the only pid anything signals), `TestSec_Daemon_UnreadableLockNeverReadsAsNoDaemon` (`locked()` failed OPEN, so `bdrive status` said "not running" while sync ran and `bdrive stop` reported success having stopped nothing), `TestSec_Daemon_LockPathIsNotFollowedThroughASymlink` (a symlink at the lock path made `Running` permanently true — `Start` a no-op, sync silently never restarting, the exact failure the flock design exists to eliminate), `TestSec_Daemon_StateFilesAreNotWorldReadable` (`daemon.log` carries the mount id, the folder's absolute path, the remote URL and the device name+id), `TestSec_Autostart_LoginCommandSurvivesAHostileBinaryPath` (the plist was string concatenation, so a legal macOS path like `Music & Video` made it unparseable XML — launchd never loaded it and `Install` reported success; the path is XML-escaped, the systemd `ExecStart=` arm is quoted, and a control character in the binary path is refused outright rather than injecting unit directives that run at login), `TestSec_Autostart_TempFileIsNotFollowedThroughASymlink` (a second copy of atomic write with a predictable temp name; it now calls `store.WriteFileAtomic`). **clean** — `TestSec_Daemon_CorruptConfigDoesNotPropagateDeletes` (5 shapes), `TestSec_Autostart_RegistrationIsNotWorldWritable`. **fixed** (r6) — `TestSec_Daemon_SomethingThatIsNotALockIsNotADaemon`: round 5's fail-closed `locked()` carved out only a symlink, which is the shape its own hacker happened to plant rather than an axis. A DIRECTORY at the lock path, a lock file nobody can open, and **no volume directory at all** all fail to open too, and every one then read as a live daemon forever — `Start` a permanent no-op, `Stop` refusing, `status` printing "running" while sync never runs again. The ENOENT case needs no attacker. The carve-out is now on the reason: an unopenable lock is a daemon only if THIS process holds it, which is safe precisely because `holdLock` opens the same path the same way, so no second writer can appear either. | a config edited under the daemon's feet; the pid/lock/log trio; the file a service manager runs at every login |
| 21 | CLI output (`cmd/bdrive`: `bdrive log`, `bdrive restore --list`) | **NEW in r5** — the audit surface was renderable by the party being audited. **fixed** — `TestSec_Output_PeerJournalStringsCannotRewriteTheTerminal` (12 subtests: `Path`, `Note`, `User`, `UserName`, `Author`, `DeviceName` are arbitrary JSON off a peer's journal, printed to a terminal with no escaping — newlines forge whole rows, `\r` repaints a `delete` as a `put`, OSC 52 writes the operator's clipboard, DECRQSS/CPR make some terminals type a reply onto the shell), `…OneEntryCannotFillTheScreen` (50 rows of log is also owned by one 40 KB entry), `…RestoreListDoesNotRenderPeerEscapes`. One `safeField(s, max)` where the rows are assembled, in both surfaces. **fixed** (r6) — `TestSec_Output_PeerStringsCannotReorderOrReintroduceControlsInTheAuditRow` + `…RestoreListDoesNotReorderTheVersionTable`: `safeField` stripped C0 and DEL only, and every sequence round 5 tested started with ESC. None of these do — U+009B **is** CSI, U+009D **is** OSC, U+0090 **is** DCS, U+0085 **is** NEL in any xterm-lineage UTF-8 terminal, and the bidi overrides (U+202E and friends, CVE-2021-42574) reorder the row so the actor columns read backwards. The filter now covers U+0080–U+009F and the bidi format controls; the length bound was attacked and held. **fixed** (r6) — `TestSec_CLI_StatusDoesNotRenderHubChosenStringsToTheTerminal` (`bdrive status` printed `mi.Volume`/`mi.Remote` with a bare `%s` — same class, an uncovered command, and `Volume` originates in the hub's project name), `TestSec_CLI_ScopeRuleCannotOutliveTheScopeThatWroteIt` (`cleanScopeDirs` checked `..` and not newlines, so a folder name carrying the managed block's own END MARKER terminated it early and the injected rule landed outside — `bdrive scope rm` removes the block by its markers, so `*/` ignored every directory, team-wide, permanently, since `.bdriveignore` syncs). **fixed (r7)** — `TestSec_Login_HubChosenAccountStringsAreNotRenderedToTheTerminal`: round 6 hardened `bdrive status`'s two lower lines and left every OTHER hub-chosen string on a bare `%s` — `whoami`, `status`'s account line, `login --status`, `runLogin`'s closing line and the device flow's `verify_url`, six hostile shapes including C1 CSI and the bidi overrides. **fixed (r7)** — `TestSec_Init_HostileProjectNameNeverRendersRawToTheTerminal`: `init` printed `p.Name` and `proj.Volume` raw on both the create and the resume paths. All routed through the existing `safeField`; no second helper. **fixed (r8)** — `TestSec_Login_HubChosenLoginPathNeverRendersRawToTheTerminal`: round 7 routed five hub-chosen strings through `safeField` and missed `login.go`'s sign-in URL, which is built from the hub's own `cli_login` and is the FIRST thing a server we have never talked to gets to print on this machine. | every peer-controlled string that reaches a terminal |
| 22 | Project seeding (`internal/templates`, `webapp/templates.go`) | **NEW in r6** — the zero-test package after five rounds; project seeding writes attacker-named files into a fresh project and had never been touched. **fixed** — `TestSec_Templates_AFilePathCannotEscapeTheProjectRoot` (`Template` and `File` are exported with exported fields and `WriteTo` did nothing but `filepath.Join` a `File.Path` onto the destination: "today every template comes from the go:embed" is a property of the callers, not of the function, and it is the third write door into a project folder), `TestSec_Templates_SeedingCannotWriteThroughASymlinkedName` (`Stat`/`MkdirAll`/`WriteFile` all follow links, so a **dangling** symlink at a template file's own name defeated the never-overwrite rule and CREATED the target anywhere on disk, and a symlinked directory took every file under it outside the project — using the SHIPPED `docs` template, no hostile input. `store.UnderRoot` plus `Lstat`, the boundary rounds 4 and 5 resolved for the syncer and the `file://` backend), `TestSec_Seed_TemplateSeedingUsesTheSameGuardAsEveryOtherWriteDoor` (the hub's own seeding door — see row 6). **clean** — `TestSec_Templates_ShippedPathsAreAllInsideTheRootAndNotReserved`. | a template path that escapes the project root, a name already in the folder that redirects the write, a shipped template that names a reserved path |
| 23 | `bdrive init` end to end (`cmd/bdrive/init.go`, `login.go`, `share.go`) | **NEW in r7.** Two consecutive CISOs named this the largest gap; it was driven end to end for the first time this round and held **two criticals**. **fixed** — `TestSec_Init_ServerSwitchNeverHandsTheOldHubsTokenToTheNewServer`: `ensureLogin`'s `if !cfg.Auth.Enabled` branch wrote `settings.Server = <new server>` and returned WITHOUT touching `settings.Token` — and `settings.Server` is the entirety of round 4's token binding (`deviceToken` → `sameOrigin(base, s.Server)`). The target server picks that branch, so a 30-line HTTP server answering `{"auth":{"enabled":false}}` collected the real hub's bearer token, eight times over in the hacker's transcript, through `--server`, the flag an agent following a README passes. The token is now cleared on any server change, on both branches. **fixed** — `TestSec_Init_RefusesToMountTheBdriveHome`: the `.bdrive` reserved-directory rule applies to segments BELOW the mount root, so it could not see a mount that IS the bdrive home — from there `settings.json` (the token), `device.json` and every project's journals are ordinary top-level files, init accepted the folder and the first cycle pushed them to the hub as project content, onto every member's and teammate's disk. Refused at the door with `store.UnderRoot(config.Home(), folder)`. **fixed** — `TestSec_Share_TheFolderConfigCannotRedirectTheDeviceToken` + `TestSec_CLI_TheDeviceTokenIsNotFollowedToAnotherOrigin`: round 4's client critical, on a door its fix never covered. Round 4 bound the credential in `remote.deviceToken` — the SYNC backend's door — while `share.go` read the destination from `proj.Remote` and handed `settings.Token` straight to it at four call sites (`splitHubRemote` checked only URL shape), and `initClient` was a bare `&http.Client{}` with no `CheckRedirect`. Handing someone a folder is the documented way to move a project. Both close at one seam: `serverDo` attaches the token only when the target origin is `settings.Server`'s, and the client drops it across an off-origin redirect. **fixed** — `TestSec_Init_FromAGitRepoHomeStillLeavesTheUserHooksInPlace`: round 5's `$HOME`-is-a-git-repo fix was broken again by a STRING COMPARE ON A PATH (`if path == user`) — `$HOME` from the env spells `/var/…` while `folder` from `filepath.Abs` spells `/private/var/…`, so the migration deleted the hooks `Install` had just written; init printed "hooks registered" and "moved out of" naming the same file with two spellings and left the user config `{}`, i.e. the entire agent integration silently off machine-wide. Same class as round 5's own `sameOrigin` finding: it compared the spelling, not the thing. Now `os.SameFile`, here and in `gitRootOf`'s `cur == home` stop. **fixed** — `TestSec_Init_AProjectIDTheDeviceCannotUseIsNotReportedAsSyncing` (a project id failing `projectPathRe` is validated only INSIDE the first cycle, where failure degrades to "offline" by design — so init exited 0, started a daemon, printed success, and every cycle was a silent no-op forever), `TestSec_Init_RefusesAFolderInsideAnExistingMount`, `TestSec_Forget_APathCannotInjectExtraIgnoreRules` (`ignoreRule` checked `..` and the file's own name but not newlines, and unlike `scope`, forget appends OUTSIDE any managed block, so nothing can take the injected rule back out — verbatim the hole round 6 closed for `cleanScopeDirs`, on the command that also prunes the hub). **clean** — `TestSec_Init_ThroughASymlinkedFolderWritesOnlyInsideTheTarget`, `…HostileFolderPathNeverExecutesThroughTheAgentHookGuard`, `…LoginItemStaysParseableAfterAHostileInit`, `…AnUnwritableFolderLeavesNoMachineWideState`, `…AHubThatRefusesTheStoreIsNotReportedAsSyncing`, `…AFailedProjectCreationRegistersNoHooksOrLoginItem`, `…HostileOnlyValuesNeverEscapeTheManagedScopeBlock`, `…BenignProjectNamePrintsCleanly`. **THE WHOLE ROW RAN AGAINST A FIXTURE HUB WITH `auth.enabled: false`, so the login flow inside init was never executed — and critical A lives in exactly the branch that skips it. The auth-enabled branch is untested.** | a hostile hub answering init; a folder that chooses where the token goes; what a FAILED init leaves behind machine-wide; the login flow init runs when there is no session |


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

Round 4 result: **32 holes closed** (4 hackers, ~34 failing test functions),
**189 `TestSec_*` tests green** across eight packages (134 `internal/webapp`,
20 `internal/syncer`, 10 `internal/agenthooks`, 9 `internal/remote`, 5
`internal/store`, 4 `internal/config`, 4 `internal/journal`, 3 `cmd/bdrive`),
whole suite green, `-race` clean on `webapp`/`syncer`/`store`. Round 4 aimed
at the previous rounds' fixes again and **five broke**: `ownJournal` (r1) bound
the journal key to a header nothing bound to an account; the `(account, id)`
device rekey (r3) held only in memory and only on the write path; the Lamport
ceiling (r3) was inclusive, so the one value it accepted froze the clock;
`clientIP`'s "last hop" (r3) read only the first header *line*; and
`ReservedDir`'s `EqualFold` (r3) missed the spellings NTFS folds away. Five of
the seven packages attacked had never been attacked before — and the two worst
findings of the round were on the client, not the hub: a peer's journal op
could read any file on every teammate's machine (`Op.Blob` reaching
`store.BlobPath` unchecked) and write through any symlinked directory in a
mount. Rows 17, 18 and 19 are new.

Rows 1–3 and 5 are the highest value: they are choke points, so a hole there
is a hole everywhere downstream.

Round 5 result: **all 29 holes closed** (4 hackers, ~35 failing test
functions), **247 `TestSec_*` tests green across ten packages, none red** —
including the two I first reported as unsatisfiable contradictions and the
coordinator sent back (see below; both had a resolution that weakens nothing). `go vet` clean; `-race` clean on
`webapp`/`syncer`/`store`/`daemon`; the Postgres arm run against a real
Postgres 16. Round 5 aimed every hacker at round 4's own fixes and **seven
broke**, including the one round 4's commit message called "the critical":
`ownJournal`'s account binding failed four different ways (seeded by the very
request it authorized, an unrecoverable lockout, released by offboarding, and
switched off entirely by a pre-accounts row — i.e. on every upgraded hub, for
exactly the established devices); the once-per-blob content-address cache was
defeated by uploading honest bytes first; `/store/sign`'s quota booking
charged for bytes that never arrive; and `Parse`'s "skip a bad line" turned
`pull`'s op-count cursor into a divergence primitive a peer aims at one device
at a time. Two rows are new: the unattended daemon plus the login registration
(20), and CLI output (21).

Round 6 result: **all 30 holes closed** (4 hackers, ~26 failing test
functions), **290 `TestSec_*` test functions green across eleven packages,
none red**. `go build` / `go vet` clean; `-race` clean on
`webapp`/`syncer`/`store`/`daemon`. Round 6 aimed at round 5's own fixes and
**five broke** (2 → 5 → 7 → 5 across four rounds): the byte-offset pull resume
was the same divergence primitive it replaced, twice over (a torn tail, and a
peer withdrawing an already-applied op); the `DisplayTime` clamp bounded one
peer-chosen value by another; `safeField` stripped C0 only, so the whole escape
vocabulary came back through C1 and the bidi overrides; the fail-closed
`locked()` carved out symlinks and let ENOENT wedge a mount forever; and
`reserve.go`, written in round 5, shipped three holes including a `-race`
data race on the billing ledger. Row 22 is new: `internal/templates`, the
zero-test package after five rounds, which held three holes — one of them
reachable with the SHIPPED template and no hostile input.

Round 7 result: **all 29 holes closed** (22 reproducers + 7 sabotage-only
false negatives), **326 `TestSec_*` test functions green across eleven
packages, none red**. `go build` / `go vet` clean; `-race` clean on
`webapp`/`syncer`/`store`/`daemon`; the Postgres arm run against a real
Postgres 16. Round 7's two criticals were both on the CLIENT, both credential
leaks, and both on `bdrive init` — the front door an agent following the README
walks in by: `init --server` handed the PREVIOUS hub's device token to any
server answering `auth: disabled` (the token binding is `settings.Server`, and
the no-auth branch wrote the new server without clearing the token), and
`bdrive init $BDRIVE_HOME` uploaded `settings.json` — the token itself — to the
hub as project content, because the `.bdrive` reserved-directory rule only sees
segments BELOW the mount root. Round 5's `$HOME`-is-a-git-repo fix was broken
again by a string compare on a path, the same class as round 5's own
`sameOrigin` finding. On the hub, the structural fix of the round is **one
exported path predicate** (`journal.SafePath`): `/store/*` had no path rule at
all while `/upload/commit` refused the same paths, and the rule had three
disagreeing spellings. Row 23 is new: `bdrive init` end to end, named as the
largest gap by two consecutive CISOs and holding two criticals the first time
it was driven.

### The round-7 sabotage sweep — 53 reversions, 45 caught, 8 missed

Round 6 reverted 33 accumulated fixes one at a time and missed 5. Round 7 ran
**53**, covering the rows round 6 skipped (3, 10, 12, 14, 16) and the three
whose whole-file revert would not compile, and reverted **all 23 route-level
checks outside `proj()` individually** (22 held). **45 held, 8 did not.**

Seven of the eight are now pinned by a replacement test; **I re-ran each
reversion by hand against the merged tree and confirmed the replacement goes
red and nothing else does.** The eighth is not a guard at all.

| # | Fix that could be deleted with all 290 green | Why every existing test missed it | Now pinned by | Verified |
|---|---|---|---|---|
| 1 | `DeviceRegistry.MayActAs`'s refusal loop | `ownsDevice` is `validDeviceID(id) && MayActAs(…)` and every test planted an id that is not a valid device id, so `validDeviceID` answered first and `MayActAs` was never consulted. The one test naming a real peer's id was saved by `heatByDevice`'s org-scoped `LookupIn` — a later layer that withholds name/OS but still RECORDS the reads. The same-org case had no coverage at all. | `TestSec_Row10_MemberCannotReportReadsUnderAPeersDeviceId` | yes — red, alone |
| 2 | `handleReadReport`'s `hasControlChars` | claimed by rows 10/14 and named by no test; it is the guard keeping the Postgres ledger wedge unreachable | `TestSec_Row10_ReadReportRefusesAControlCharacterPath` (5 arms) | yes — red, alone |
| 3 | `Content-Security-Policy: frame-ancestors 'none'` | round 3's test computes `framed := csp has frame-ancestors \ **fixed (r8)** — `TestSec_Org_EvictingTheSoleOwnerCannotLeaveAnOrgNobodyCanAdminister`: round 7's own fix created a new state. `EvictMember` drops a row unconditionally (right: an ownership row for an address nobody can sign in as is inherited by the next signup on it), but every org route is gated on `RoleOwner` and NOTHING adopts an ownerless org — so one hub admin calling `Deny` on the sole owner left an org with members that can never again gain one, lose one, or change a role. Eviction of the last owner now promotes the longest-standing remaining member (`ponytail:` no join time is recorded, so it is the lowest address — deterministic on every replica). |\| xfo == DENY \|\| SAMEORIGIN` — it holds the **disjunction**, not the code | `TestSec_Row16_ShellCarriesBothFramingHeadersNotEitherOr` | yes — red, alone |
| 4 | `X-Frame-Options: DENY` | same disjunction | same test, independently | yes — red, alone |
| 5 | `sqlAccountRepo.PutAccount`'s id guard | round 6's collision test builds its hub with the **file** backend only, and nothing in the repo — conformance suite included — ever called the SQL arm with a colliding id. **The untested backend is the one managed and Postgres deployments run.** **fixed (r8)**, found independently by two hackers — `TestSec_Row5_AReadRouteCannotFirstClaimADeviceId`, `…AReadOnlyMemberCannotLockADeviceOutOfItsOwnJournal`, `…NoReadRouteRegistersADeviceItHasNeverSeen`, `TestSec_Device_AReadCannotClaimADeviceIdForTheCaller`: round 5 moved `observeDevice` after the decision on the WRITE door and left it as the FIRST STATEMENT of `handleStoreExists`/`List`/`Get`. `OwnerOf` is hub-wide first-claim, so one GET by a read-only member of any one project claimed any unclaimed device id and the victim's next journal push 403'd, hub-wide, with no remedy but abandoning `device.json`. A device is now something that pushes ITS OWN JOURNAL: `observeDevice` creates a row only on an authorized journal write, and every other door (`list`/`get`/`exists`/`sign`, and a blob PUT — a blob says nothing about who a device is) calls the new `refreshDevice`, which records only into a row this account already owns. **Seven existing tests registered their devices through a read door and were rewritten to register through the journal door (`secRegisterDevice`, `sec_devreg_test.go`); no assertion changed.** **fixed (r8)** — `TestSec_Store_AJournaledPathTheUploadDoorRefusesIsAlsoUnremovable`: round 7 unified `journal.SafePath` across the ingest doors but `cleanUploadPath` is SafePath AND `config.ReservedPath`, and only the browser door got the second clause — so `/store/*` journaled `.git/hooks/pre-commit` (200) while `/remove` and `/shares` answered 400 for the same path, making the entry permanent. `journalOps` now applies both. **clean (r8)** — `TestSec_Row5_StoreExistsAnswersOnlyAboutItsOwnProject`, `TestSec_Path_BothIngestDoorsRefuseTheSameHostilePaths` (15 spellings, both doors, same answer). | `TestSec_Row14_AccountIdIsNeverReassignedOnAnyBackend` (file + sqlite + a real Postgres 16) | yes — red on the sqlite arm, file arm stays green |
| 6 | `blobRe` in `RemoteSource.Files` | claimed by row 11 and named by no test | `TestSec_Row11_AnOpWithABogusBlobDoesNotMaskTheLastGoodVersion` (5 arms) | yes — red, alone |
| 7 | `handleInviteAccept`'s `me.Email == ""` | **also a live finding, fixed this round** — it guarded on the raw string while everything downstream normalizes, so whitespace-only identities walked past it into `Redeem` + `CheckSeat` | `TestSec_Row3_InviteAcceptRefusesAnIdentityWithNoAddress` | it was RED on the current tree |
| 8 | `projectJSON`'s `p.Perms, p.Default = nil, ""` | **NOT a guard.** All three callers gate on `PermRead` and `/api/p/{id}/permissions` hands the same grants to the same audience. Payload hygiene. **fixed (r8)**, the device flow attacked for the first time — `TestSec_DeviceFlow_OneApprovalMintsExactlyOneToken` + `…OneApprovalMintsOneToken` (two hackers, same hole): `apiDevicePoll` peeked and took in two acquisitions of `c.mu` and DISCARDED `take`'s return, so every poll past `peek` reached `issue` — 24 approvals minted 29 tokens, each permanent. One `takeGranted` under a single lock now returns the grant only to the caller that consumed it. `…TheApprovedDeviceIsTheOneTheTokenIsBoundTo` + `…TheDeviceTheHumanApprovedIsTheDeviceTheTokenRecords`: the token was minted under `req.Device` chosen at POLL time while the approval page — this flow's entire consent surface — rendered `g.device` from START time; it now issues under `g.device` and ignores `req.Device`. `…TheLinkTheHumanOpensIsNotAlsoThePollCredential`: RFC 8628 splits `device_code` from `user_code` and this hub issued one value for both, so a screenshot, a forwarded link or a terminal transcript was a bearer credential for a permanent token; `verify_url` now carries a separate `link` secret that the poll route does not accept (the poll id still opens the page — it is the requesting client's own secret, and older CLIs print it). `…TwoAddressesCannotDenyEveryDeviceLoginOnTheHub`: `maxPendingGrants` REFUSING was the outage the per-IP cap existed to prevent, two addresses away; the hub-wide bound now evicts instead, and evicts from whichever address holds the most, which is the flooder by definition. **fixed (r8)** — `TestSec_Login_TheLoopbackCallbackOnlyCompletesTheFlowItStarted`: `browserLogin`'s only binding was `state`, which is `fmt.Println`'d AND passed to `open`/`xdg-open` as `argv[1]` (readable by every local account via `ps`) — so any local process signed the device in as ITS OWN account and the user's folders then synced into the attacker's project. PKCE (RFC 7636/8252): the CLI sends `code_challenge`, `/api/auth/exchange` requires the matching `code_verifier`, and a CLI that bound its flow refuses a code minted for a flow that did not. **clean (r8)** — `TestSec_DeviceFlow_ApprovalNeedsAPostFromACookieSession` (a GET grants nothing, a device token is not a browser session, the cookie is SameSite=Lax), `TestSec_CLIAuth_TheLoopbackRedirectAcceptsOnlyLoopback` (16 hostile spellings). The PKCE happy path is pinned by a functional (non-`TestSec_`) test, `TestCLIBrowserLoginPKCERoundTrip`, because a proof-of-possession check that refuses everything would have passed every attack test in the round while breaking `bdrive login` outright. | nothing — deliberately no test invented | reading confirmed |

**The two choke points were sabotaged for the first time in seven rounds, and
both held decisively.** This is what rounds 1–6 asserted and never showed:

| Choke point | Reversion | Tests that caught it |
|---|---|---|
| `perms.go:requirePerm` (row 2) | return true unconditionally | **30** |
| `perms.go:projectPerm` (row 2) | delete `role == "" → PermNone`, the org-membership gate | **21** |
| `auth.go:authGate` (row 1) | delete the `if !open` credential check | **9** |

### Three design tensions round 7 could not resolve by choosing a winner

Recorded rather than papered over. Each is a place where two hacker tests
constrain the code in opposite directions.

1. **A missing journal slot vs. convergence with a latecomer (row 15).** The
   new identity guard refuses a peer REDEFINING an op's `(device, seq)` slot.
   It cannot also require every applied slot to still be PRESENT, because
   round 4's `TestSec_Pull_APeerCannotChooseWhichOpsEachDeviceSees` asserts
   `equalTrees` after a peer makes an applied op's line undecodable **while
   appending more ops** — a device that already applied it and a device syncing
   for the first time must agree, and the latecomer cannot recover an op it
   never saw. Requiring presence diverges them permanently. So: **a peer that
   makes an applied op's line undecodable AND appends at least as many new ops
   still un-publishes that op on the devices that applied it**, covered only by
   round 6's count guard. The class is removed properly only on the hub —
   `/store/*` refusing a journal PUT that is not an extension of the stored
   object, which is the append-only invariant enforced where there is a single
   authoritative copy. That is a behaviour change with no failing test
   demanding it and it would break many tests' setups. **Round 8's target.**
2. **The device-flow bound vs. a rate limit (row 8).**
   `TestSec_DeviceFlow_AnAnonymousStrangerCannotAccumulateHubState` wants 1000
   anonymous starts refused; `TestSec_CLIAuth_AGrantTheHubReportsDeadIsNotRetainedForever`
   mints **201** from one IP and `t.Fatal`s on anything but 200. No token
   bucket satisfies both, so the route got a **bound** (sweep + cap), not a
   limiter. I added a per-IP cap on top of the hub-wide one, because a
   hub-wide cap alone converts "a stranger exhausts memory" into "a stranger
   denies every `bdrive login --device` on the hub" — the same outage bought
   more cheaply. `/api/auth/device/start` is still **not rate limited** and
   `/api/auth/device/poll` is still unmetered.
3. **Stripping vs. refusing an off-origin redirect (row 23 / 19).**
   `remote/http.go` REFUSES a hub's cross-origin 3xx (round 5). The CLI's
   client now only STRIPS the `Authorization` header, because
   `TestSec_CLI_TheDeviceTokenIsNotFollowedToAnotherOrigin` has a control
   assertion that `t.Fatal`s if the redirect target is never reached. Stripping
   is stronger than the stdlib (which only drops on a hostname change; this
   drops on scheme and port too) and weaker than the sibling door. **The two
   doors do not agree, on purpose, because a test requires it.**

### One test's SETUP was restated in round 7 — assertions untouched

`TestSec_Journal_HostilePathCannotBeLaunderedThroughRestoreOrRemove` (round 2)
pushed `../../../etc/bdrive-owned` and friends through `/store/*` and
`t.Fatal`'d unless the hub answered **200** — its own comment said "the hub
never validates it". Round 7 gave that door `journal.SafePath`, so the push is
now refused, which is strictly stronger. The subject of the test is the way
OUT, not the way in, so the hostile journal is planted directly in storage
(as several other tests already plant objects) and the refusal is asserted as
a new control. **No assertion was changed**; I verified the restated test still
goes red when `restore`/`remove` stop calling `cleanUploadPath` — all five
hostile paths, both routes. Same precedent as round 6's `permHub` fixture
change.

### The sabotage table — the strongest evidence the suite is real

One round-6 hacker did something new: it **reverted 33 of the accumulated
fixes one at a time**, in a scratch copy, and re-ran the whole suite to see
whether anything caught it. **28 held. Five did not** — five green tests that
were passing for a reason other than the fix they name. That is a 15% false-
negative rate on the suite's own regression claim, and it is the reason this
loop keeps running rather than the reason to stop it. All five are closed:

| # | Fix that could be deleted with the suite still green | Why the test missed it | Now pinned by |
|---|---|---|---|
| 1 | the account binding in `ownJournal` (r4/r5) | `permHub` built `Devices == nil`, so the binding returned early in the fixture a dozen journal tests use | `TestSec_Audit_PermHubRefusesAForeignJournalOutOfTheBox` + the fixture change |
| 2 | "an ownerless legacy row still claims the id" (r5) | the test's ops came from a helper that never sets `device`, so the 403 came from the first-claim rule whatever `OwnerOf` answered | `TestSec_Audit_OwnerlessLegacyRowStillClaimsTheDeviceId` |
| 3 | `reservedBytes` (r5) — half of `reserve.go`'s stated contract | nothing asserted that an OUTSTANDING grant counts against the cap; making it return 0 kept all 247 green **fixed (r8)** — `TestSec_Org_EvictingTheSoleOwnerCannotLeaveAnOrgNobodyCanAdminister`: round 7's own fix created a new state. `EvictMember` drops a row unconditionally (right: an ownership row for an address nobody can sign in as is inherited by the next signup on it), but every org route is gated on `RoleOwner` and NOTHING adopts an ownerless org — so one hub admin calling `Deny` on the sole owner left an org with members that can never again gain one, lose one, or change a role. Eviction of the last owner now promotes the longest-standing remaining member (`ponytail:` no join time is recorded, so it is the lowest address — deterministic on every replica). | `TestSec_Audit_OutstandingPresignedGrantsCountAgainstTheCap` |
| 4 | `cleanUploadPath`'s control-character refusal (r5) | row 6 claimed it fixed and named no test; there was none | `TestSec_Audit_UploadPathRefusesControlCharacters` |
| 5 | `unsafeRel` (r3) and `blobRe` in `OpenBlob` (r2) | both survive on a LATER layer — `store.UnderRoot` (r4) and `remote.Prefixed.safeKey` (r4). Defence in depth is right; a test that silently changes which layer it measures is not **fixed (r8)**, found independently by two hackers — `TestSec_Row5_AReadRouteCannotFirstClaimADeviceId`, `…AReadOnlyMemberCannotLockADeviceOutOfItsOwnJournal`, `…NoReadRouteRegistersADeviceItHasNeverSeen`, `TestSec_Device_AReadCannotClaimADeviceIdForTheCaller`: round 5 moved `observeDevice` after the decision on the WRITE door and left it as the FIRST STATEMENT of `handleStoreExists`/`List`/`Get`. `OwnerOf` is hub-wide first-claim, so one GET by a read-only member of any one project claimed any unclaimed device id and the victim's next journal push 403'd, hub-wide, with no remedy but abandoning `device.json`. A device is now something that pushes ITS OWN JOURNAL: `observeDevice` creates a row only on an authorized journal write, and every other door (`list`/`get`/`exists`/`sign`, and a blob PUT — a blob says nothing about who a device is) calls the new `refreshDevice`, which records only into a row this account already owns. **Seven existing tests registered their devices through a read door and were rewritten to register through the journal door (`secRegisterDevice`, `sec_devreg_test.go`); no assertion changed.** **fixed (r8)** — `TestSec_Store_AJournaledPathTheUploadDoorRefusesIsAlsoUnremovable`: round 7 unified `journal.SafePath` across the ingest doors but `cleanUploadPath` is SafePath AND `config.ReservedPath`, and only the browser door got the second clause — so `/store/*` journaled `.git/hooks/pre-commit` (200) while `/remove` and `/shares` answered 400 for the same path, making the entry permanent. `journalOps` now applies both. **clean (r8)** — `TestSec_Row5_StoreExistsAnswersOnlyAboutItsOwnProject`, `TestSec_Path_BothIngestDoorsRefuseTheSameHostilePaths` (15 spellings, both doors, same answer). | `TestSec_Audit_UnsafeRelRefusesEveryPathAJournalMayNotName`, `TestSec_Audit_OpBlobIsRefusedBeforeItReachesStorage` |

The audit also found that `unsafeRel` accepted `"."` (contained only because
`hashFile` happens to fail on a directory first) — fixed — and left three of
its own gaps: rows 3, 12 and 16 were never sabotaged at all, and `orgs.go`'s
invite-revocation durability, `devices.go`'s `MayActAs` and `upload.go`'s
reserved-dir guard could not be reverted because the whole-file revert did not
compile. Those four are round 7's first targets.

### The fixture change, and the three tests whose result it moved

`permHub` now installs a `DeviceRegistry`. Every permHub test that pushes a
journal was re-checked; three changed result, all three because their SETUP
pushes a journal body whose ops carry no `device` field, which the real client
always sets (`syncer.go` stamps `Device` on every op) and which the r5
first-claim rule requires for an unclaimed id:

- `TestSec_Path_ValidBlobHashStaysInsideItsProject`
- `TestSec_Path_HostileBlobCannotRepointALiveShare`
- `TestSec_Path_MemberReadsAnotherOrgsBlob`

Their setup now stamps the device the way the client does. **No assertion was
touched** — all three still assert exactly what they asserted about `Op.Blob`,
and all three still fail if that guard is removed. This is the same kind of
edit round 5 named for `…PresignedGrantIsBookedEvenWithoutACommit`.

### Two round-6 tests that could not pass as written, corrected and named here

Neither was weakened; each was unsatisfiable in BOTH directions as delivered,
and the corrected form still fails on the code that shipped:

1. `TestSec_Admin_AChangeTheStoreRefusedIsNotInEffect/deny` compared
   `verifyPassword(...)` to `nil` as if it returned an `error`; it returns
   `*authUser`, so the desired outcome (a rollback) hit a `t.Fatal` and the
   bug outcome hit a `t.Error`. It now asserts the property its own comment
   states — never "on disk AND gone from memory".
2. `TestSec_Account_AnIdCollisionMustNotDestroyALiveAccount` planted the
   clobber in `a.users` itself and then `t.Fatal`'d on the very store refusal
   it asks for. The store is the layer that has to refuse; that is now the
   assertion, and the two consequence assertions are unchanged.

### Two apparent contradictions, both resolved — nothing left RED

I first reported these as contradictory and left them red. They were not:
each had a resolution that satisfies every test without weakening any of
them, and both are now green.

1. **The quota at a presigned grant** — `TestSec_Sign_QuotaIsOnlyChargedForBytesThatArrive`
   ("a grant must not charge for bytes that never arrive") versus round 4's
   `TestSec_Sign_DirectDeviceUploadIsBookedAgainstTheQuota` and
   `TestSec_Browser_PresignedGrantIsBookedEvenWithoutACommit` ("a direct
   upload is billed even though the hub never sees the bytes"). Both are
   right; they are about different moments. A grant is now a **reservation**
   (`webapp/reserve.go`): it counts against the cap the instant it is granted
   (`reservedBytes` is added to every `CheckWrite`, so concurrent grants
   cannot oversubscribe an allowance none of them exceeds), it is CHARGED when
   the object is confirmed in storage, and it is RELEASED for free when its
   URL expires unused. Confirmation (`reconcileGrants`) runs wherever the hub
   already has that project's storage in hand — every `/store/*` and
   `/upload/*` write handler plus `/store/list`, which is the first call of
   every sync cycle — and costs nothing when nothing is outstanding.
   Double-charging is closed at the same seam: bytes are billed once, where
   they land, so a relayed put and a commit that finalizes a grant CLAIM the
   reservation (`claimGrant`) instead of charging it a second time, and a
   commit with nothing to claim charges nothing (committing a path is not a
   second copy of the content).
   **One test edit, named here:** `…PresignedGrantIsBookedEvenWithoutACommit`
   asserted the charge with no hub request between the direct PUT and the
   check, which is the one thing a hub physically cannot know. Both of its
   probes now do one ordinary `GET /store/list` first — emphatically not a
   commit, which is the test's subject. The assertions themselves are
   unchanged and still fail if arrived bytes are never billed.
2. **`TestSec_HTTP_BearerTokenIsNeverSentToAnotherOrigin` (round 4)** failed
   on its own vacuity guard ("redirect target was never reached; the test
   proves nothing") because the client now refuses cross-origin redirects
   instead of following them without the token. That is strictly stronger and
   subsumes the original assertion, so the test's assertion was **rewritten to
   the stronger property** under the same name: the target is never reached at
   all and `Get` returns an error the caller can see. Strengthening an
   obsolete assertion, not weakening a hacker's test.

A third contradiction was real and is still resolved by design, not by
choosing a winner — see the journal-claim paragraph below.

The journal-claim contradiction, which shaped that fix:
`TestSec_Journal_AnUnclaimedDeviceIdIsNotWonByTheWriteItGuards` requires a
plain member's journal PUT for an id nothing has ever synced under to be
**refused**, while `TestSec_Browser_JournalKeyMustNameARegistrableDevice`
(round 5) and `TestSec_Store_MemberCannotWriteAPeersJournalByRenamingItself`
(round 4) both require that same request — same credential, same hub state — to
**succeed**, with `t.Fatalf` controls. The requests are indistinguishable from
hub state alone, so all three can only pass if the claim is judged on the
BODY: an unclaimed id may be claimed by a first push only when every op in it
names that device (and an ops-less body claims nothing). That is what shipped.
It is an integrity check, not a proof of ownership — a determined attacker
sets the field, or simply names the id on a read first, and the id is theirs.
**The class is only removed by a hub-minted binding** (the device id bound to
the account when the device token is granted, at `bdrive login`), which needs
a client protocol change and cannot be exercised by tests that authenticate
with a browser cookie. That is the round-6 design decision.

### Loop status after round 7 — NOT done

Both conditions are stated in "What counts as done". **Neither is met.**

1. **Every row `clean` or `fixed`, backed by a named test** — *not met*, but
   every row 1–23 carries at least one named test and **every `TestSec_*` in
   the tree is green: 326 test functions, 0 red**, whole suite green,
   `go build` / `go vet` clean. What keeps this unmet is unchanged and small:
   row 14's `…NULBytesDoNotTruncateRecords/postgres`, RED only under
   `BDRIVE_TEST_POSTGRES` (re-run this round against a real Postgres 16 — all
   other Postgres arms pass, including the new `TestSec_Row14_…/postgres`) and
   a documented backend divergence rather than a hole, plus row 1's expiry
   item, which is still a design decision with no concept in the code to test.
2. **Two consecutive dry hacker rounds** — *not met*. Round 7 produced **22
   failing test functions (27 subtests) across 4 packages, 29 holes** (22
   reproducers + 7 sabotage-only false negatives). The counter is back at zero.
   **No row came back dry this round.**

The honest reading of round 7 is worse than round 6, not better. The sabotage
sweep tripled in size and the miss rate barely moved (5/33 → 8/53, 15% → 15%),
`bdrive init` — flagged by two consecutive CISOs as the largest gap — held two
criticals the moment it was finally driven, and the two worst findings of the
round were both **client-side credential leaks through the front door an agent
following the README walks in by**. Three of the seven false negatives were
guards that survived only because a later layer happened to catch the same
thing, which is the round-6 lesson repeating.

### Loop status after round 6 — NOT done

Both conditions are stated in "What counts as done". Neither is met.

1. **Every row `clean` or `fixed`, backed by a named test** — *not met*, but
   every row 1–22 now carries at least one named test, and **every
   `TestSec_*` in the tree is green (290/290 test functions, 0 red)**. What
   keeps this unmet is unchanged and small: row 14's
   `…NULBytesDoNotTruncateRecords/postgres`, RED only under
   `BDRIVE_TEST_POSTGRES` and a documented backend divergence rather than a
   hole (and unreachable through the API), plus row 1's expiry item, which is
   still a design decision with no concept in the code to test.
2. **Two consecutive dry hacker rounds** — *not met*. Round 6 produced ~26
   failing test functions across 30 holes, so it was not dry. The counter is
   back at zero. **No row came back dry this round.** Five of the holes were
   in fixes shipped one round earlier, and the sabotage table found five more
   fixes that no test was actually holding — which is the clearest possible
   statement that the loop has not converged.

### Loop status after round 5 — kept for the record

Both conditions are stated in "What counts as done". Neither is met.

1. **Every row `clean` or `fixed`, backed by a named test** — *not met*, but
   closer than any round so far: **every `TestSec_*` in the tree is green
   (247/247)**. What keeps this unmet is row 14's
   `…NULBytesDoNotTruncateRecords/postgres`, which is RED only under
   `BDRIVE_TEST_POSTGRES` and is a documented backend divergence rather than a
   hole (and now unreachable through the API), plus row 1's expiry item, which
   is still a design decision with no concept in the code to test. Row 2's
   admin escape is now ANSWERED and guarded.
2. **Two consecutive dry hacker rounds** — *not met*. Round 5 produced ~35
   failing tests across 29 holes, so it was not dry. The counter is back at
   zero. No row came back dry this round.

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

New from round 6 — consequences of this round's own fixes, named on purpose:

- **Mailed links pin the first host the hub is reached on** when
  `auth.base_url` is unset. A hub first reached on the wrong name (a health
  check on `localhost`, a stale DNS entry) mails links on that name until it
  restarts. Configuring `auth.base_url` is the real fix and is now documented;
  the pin is the fail-safe for hubs that do not.
- **`Server.offboard` runs inside `BuiltinAuth.Deny`** through an `Offboard`
  hook the server wires in `Handler()`. It clears project grants with a
  `dropPerm` that deliberately skips the last-admin guard (a grant held by an
  account that no longer exists is the vector, not a safety net), and
  `OrgDB.RemoveMember` is now idempotent. A provider that is not `BuiltinAuth`
  has no removal path and therefore no cleanup — the hook is the seam.
- **`pull` re-parses BOTH copies of a rewritten journal** to compare op counts.
  A peer that rewrites its log costs every reader O(journal) twice that cycle,
  and the shrink guard is a COUNT: a rewrite that keeps the count and changes
  the content is still accepted (and still replayed by everyone identically,
  which is the invariant that matters).
- **`DisplayTime` returns the zero time for an op stamped in the future.**
  `bdrive log` prints `0001-01-01` for such a row and sorts it last. That is
  deliberate — an op we cannot date must not outrank ones we can — but a peer
  with a badly wrong clock now sinks instead of floating.
- **An unopenable `daemon.lock` reads as "no daemon" from another process.**
  Only the holder knows. This is safe because `holdLock` opens the same path
  the same way, so a second daemon cannot start either — but `bdrive status`
  in a second process will say "not running" for a lock the operator has
  chmod'ed to 0000.
- **Grants outlive their expiry in the ledger** until `reconcileGrants` asks
  storage, with a 24h backstop sweep. A project nothing touches again for 24h
  loses the charge for bytes that arrived after the grant expired.
- **`trimName` now strips path separators and C0/C1/bidi from project names.**
  A project named `docs/2026` becomes `docs2026`; existing names are not
  rewritten, only new ones and renames.
- **`internal/templates` imports `internal/store`** for `UnderRoot`. One more
  edge in the client dependency graph, and the reason the guard is not
  duplicated a fourth time.

New from round 5 — consequences of that round's own fixes, named on purpose:

- **Journal ownership is still first-claim, only durably so.** `OwnerOf` is
  hub-wide, survives offboarding, and refuses to read an ownerless row as
  permission — but the first claim still comes from a request, and a read
  request (`GET /store/list`) makes one. So an id nothing has ever synced under
  can be taken by any org member in two requests instead of one, and the write
  door only additionally demands that the ops name that device. **The remedy
  now exists and is named in the 403** (delete `device.json`, or ask a project
  admin, who may push any journal in their project) — that admin power is new
  and is itself worth attacking.
- **Blobs are verified on EVERY read on a presigning backend.** One extra
  storage GET per blob read on S3/GCS. A cache can come back only when it can
  be keyed on the stored object's identity (ETag/generation), which
  `remote.Backend` does not carry.
- **`pull` resumes at a byte offset.** A peer that rewrites its journal object
  makes every reader re-apply every op in it (idempotent, but O(journal) that
  cycle). Nothing surfaces "this peer rewrote its log" to a human.
- **`journal.Parse` now drops a line whose `kind` is neither put nor delete.**
  A future op kind is invisible to today's readers, which is the correct
  direction for divergence but means the wire format cannot grow a third kind
  without a version gate.
- **A cross-origin redirect from the hub is refused.** A hub deployed behind a
  redirecting proxy (http→https on the same host is fine; a different host is
  not) now fails its clients' sync with an explicit error instead of
  following.
- **Presigned grants are reservations held in the hub process** (`reserve.go`).
  A restart forgets the outstanding ones, so during the minutes after one the
  cap is checked without them — the bytes are still charged, because
  confirmation is a storage lookup rather than a memory of the grant. Nothing
  charges an object that lands after its grant expired until something else
  writes it. Attack the release path (expiry), the claim path (a commit racing
  the reconciler for the same sha), and the fact that `reconcileGrants` issues
  one `Exists` per outstanding grant on the request that finds them.
- **The daemon exits when its folder config names a different remote.** A
  legitimate re-`init` therefore stops the running daemon; the next bdrive
  command in that folder starts it again.
- **`bdrive log`/`restore --list` strip C0/DEL and bound every peer-controlled
  field.** A path with a legitimate control character (there is no such thing
  on unix but for `\t`) prints without it; the log is display, not the journal.

New from round 4 — consequences of that round's own fixes, named on purpose:

- **Journal ownership is "first account seen syncing under this id, within the
  project's org".** That is the strongest fact the hub holds — a device id is
  client-asserted and the request asking the question registers a row on its
  way in, so "do I have a row" proves nothing. Two consequences: (a) an id that
  nobody in the project's org has ever synced is unclaimed, so an attacker who
  guesses a peer's device id *before that peer's first sync* owns the journal
  key (48 random bits, and the id is only visible to project members after a
  sync has already happened); (b) two accounts on one machine share one
  `device.json` id, so after the second account signs in its pushes are refused
  with 403 and no CLI command explains it. Both want a real per-project device
  claim, which is new state.
- **Presigned blobs are verified on READ, once per blob per hub process.** The
  bytes never pass through the hub, so nothing else can. This costs one extra
  storage read per blob per process on S3/GCS, the poisoned object still *sits*
  in storage (it is simply never served), and a restart re-verifies. A
  persisted verified-set is the upgrade.
- **`/store/sign` books the DECLARED size against the quota.** A device that
  signs and never uploads is charged; one that uploads fewer bytes is
  over-charged. The direction is safe but it is not reconciliation, and
  `/store/*` still has no commit step to do it properly.
- **`journal.Parse` now skips a line it cannot decode.** Every reader drops the
  same lines from the same bytes, so replay stays in agreement — but a journal
  is no longer all-or-nothing, and nothing surfaces "this device's log has N
  unreadable lines" to a human.
- **Byte-exact paths ride in a new optional `path_raw` field.** A bdrive older
  than this round reading a new journal still sees the lossy U+FFFD path, so a
  mixed-version fleet disagrees about such a path until everyone upgrades.
- **`journal.Less` gained tie-breakers** (Kind, Path, Blob, Size, Mode) below
  the existing four keys. Ops that already differed are ordered exactly as
  before; only previously-tied ops (whose order was the caller's accident) gain
  a defined one. Ops differing solely in `Note`/`User`/`Mtime` still tie —
  harmless for `Replay`, which reads none of them.
- **`requestIP` (the CLI device grant) no longer honours `X-Forwarded-For`.**
  Behind a proxy the recorded address is now the proxy's rather than a value
  the client chose. Everything with a `*Server` uses `s.clientIP`, which
  respects `trust_proxy`.
- **The SQL device table was replaced, not altered.** `device_rows` is keyed
  `(user_email, id)`; the old `devices` table is left in place and its rows are
  copied once. Nothing reads `devices` any more.

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
  **Since round 6 it installs a `DeviceRegistry`**, so a journal push through it
  measures device ownership as well as permission — which is what a served hub
  does (`cmd/bdrive/web.go` always sets `Devices`). Consequence for anything
  you write: a journal PUT for an id nothing has synced under is refused unless
  every op in the body names that device, exactly as the real client stamps
  them. `secfx4Registry` still exists for a registry you want to pre-populate.
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
- `secfx4Registry(t, srv)` (`sec_fixes4_test.go`, new in r5) — replaces a
  permHub's device registry with a fresh empty one.
- `secaudOpLine(seq, dev, kind, path, blob)` (`sec_audit_test.go`, new in r6) —
  an op line WITH the `device` field, which is what the real client writes and
  what a first journal claim now requires.
- `secsignHub(t)` + `secfx5Sign` (`sec_fixes5_test.go`, r6) — presigning hub
  plus a one-line `/store/sign` probe, for the reservation ledger.
- `seccliMount(t, volume)` / `seccliRun` (`cmd/bdrive/sec_cli_test.go`, r6) —
  an isolated `BDRIVE_HOME` with one enrolled folder whose
  `.bdrive/config.json` carries an attacker-chosen value, and a runner that
  captures real stdout (`status` and `init` use `fmt.Printf`).
- `sectplDocs(t)` (`internal/templates/sec_templates_test.go`, r6) — the
  shipped `docs` template, the input the seeding attacks need.
- `secdmnMount(t)` / `secdmnRun(t, m)` (`internal/daemon/sec_daemon_test.go`,
  new in r5) — a real daemon over an isolated `$BDRIVE_HOME` and a `file://`
  remote; `secoutMount`/`secoutRun` (`cmd/bdrive/sec_output_test.go`) drive
  `bdrive log` against a planted peer journal.
- `secsignHub(t)` (`sec_sign_test.go`, new in r4) — a hub whose storage CAN
  presign, plus a recorder of every key/size/TTL the signer was asked for.
  This is the fixture three rounds said they needed and skipped; use it for
  the browser commit flow next.
- `secRegisterDevice(t, h, project, cookie, id, name, os)`
  (`sec_devreg_test.go`, r8) — how a device becomes known to the hub: it
  pushes its OWN journal. A read route claims nothing since round 8, so any
  fixture that needs a registered device calls this.
- `secdevHub(t)` + `secdevStart`/`secdevApprove` (`sec_devflow_test.go`, r8) —
  the headless sign-in flow end to end, with `BuiltinAuth.tokens` visible.
- `secloginNewHub(t, opts)` (`cmd/bdrive/sec_login_test.go`, r8) — a real
  `BuiltinAuth` behind an `httptest` server plus the store proxy, driven by the
  real binary: the fixture for anything about credentials on a client.
- `secbndBackend` (`internal/syncer/sec_bounds_test.go`, r8) — a backend that
  lies about an object's size in LIST and floods it on GET.
- outside `internal/webapp`: `sharedRemote`/`newDevice`/`cycle`/`prune`/
  `hubState` (`internal/syncer`) for multi-device attacks, and the r4 files
  `internal/{store,config,journal,remote}/sec_*_test.go` +
  `cmd/bdrive/sec_migrate_test.go` for the client side.

Rules that keep four attackers from colliding in one package:

- **Never edit an existing test file.** Add your own new file only.
- Every helper you add is prefixed with your file's slug (`gateDo`, `permsDo`)
  so two files can't declare the same name.
- Reuse the helpers above by calling them; do not copy them.

## Coverage gaps after round 8 — round 9's targets

Written by the CISO, verified against the tests that actually exist. Where a
claim was checkable I checked it rather than repeating it. **Be blunt here:
overstating coverage is the only way this process fails silently.**

**Round-8 claims I checked and found TRUE** (each is a test that asserts the
attack is REFUSED, not a happy path, and each is green):

- `journal.SafePath` came back dry under an exhaustive attack — total over all
  256 bytes, every accepted path its own `Clean` form, and both hub ingest
  doors refusing the same 15 hostile spellings
  (`internal/journal/sec_fixes7_test.go`, `TestSec_Path_BothIngestDoorsRefuseTheSameHostilePaths`).
- `/remove` (11 hostile shapes + cannot author into another device's journal),
  `/download` (`attachment` or sandbox CSP on every response, no header CRLF),
  `/store/exists` (cross-org contained, non-store keys refused before storage).
  These are the three routes round 7 called "no route-specific attack test";
  they now have one each.
- `AgentHeat` and `/heat?by=device` carry no human or share actor across all
  three real ingest paths; anonymous `/api/config` carries no analytics until
  one is configured (`Endpoint` was one of the two exported functions no
  `TestSec_` test reached).
- `read-log` spools nothing outside the mount across 7 event shapes, and round
  7's `HasPrefix(rel, "..")` lead is **not** a hole — it errs over-strict.
- Round 7's critical-A fix (`init --server` never carrying the old hub's
  token) **holds on the auth-enabled branch it had never been run on**
  (`TestSec_Init_ServerSwitchNeverCarriesTheOldHubsTokenWhenAuthIsOn`,
  `…AFailedSignInLeavesNoHalfConfiguredDevice`) — the round-7 gap list's first
  target, closed as clean rather than as a finding.
- The `pageCLI` loopback redirect allowlist refuses all 16 hostile spellings,
  and the device approval page needs a POST from a cookie session (a GET
  grants nothing, a device token is not a browser session, SameSite=Lax).

**Two hacker tests that contradict each other (r8), and what I did:**

`TestSec_Mail_TheFirstLinkAFreshHubMailsCannotBeAimedAtAnAttackerChosenHost`
requires that the FIRST mail a fresh unconfigured hub sends must not carry the
requester's host. `TestSec_Mail_AStrangerCannotStripTheOriginFromEveryLater
MailedLink`'s **control** requires that exact mail to carry it ("so a pin was
taken"). The two requests are byte-identical in everything the hub can see —
same route, same anonymity, same `Host` — and differ only in the recipient
address, so no rule satisfies both. The round-6 and round-7 mail tests have
controls of the same shape (an absolute link from a hub with no `auth.base_url`),
which is the behaviour being removed. I implemented the close — **no request
host is ever used for a mailed link, and `ValidateSignupPolicy` refuses `smtp`
with `base_url` empty** — and updated those three controls (two now assert the
opposite, one configures the origin). **Every attack assertion is unchanged and
still green; the premises that encoded the bug are gone.** If the next round
disagrees with that call, the place to argue is this paragraph.

Round 4's convergence test and round 8's withdrawal tests looked like the same
kind of contradiction and were not: re-asserting a withdrawn op into the
receiver's OWN journal satisfies both (row 15). No test was touched there.

**Fixtures I changed (never an assertion), and why:**

- Seven `internal/webapp` tests registered a device by calling a READ route,
  which is the hole round 8 closed. They now call `secRegisterDevice`
  (`sec_devreg_test.go`) — a journal push, which is how a device becomes known.
  `TestSec_Devices_IdCannotBeSquattedBeforeItsOwnerRegisters` keeps its read
  call for the SQUAT (which now records nothing) and registers the real owner
  through the journal door, so the property it asserts got stronger.
- `TestDeviceCodeFlow` (a non-`TestSec_` test) asserted `verify_url` ends with
  the poll code — the property round 8 says must NOT hold. It now asserts both
  values are real secrets and that they differ.
- `TestValidateSignupPolicy`'s `open+verify+mailer` case and the round-3
  "production-shaped hub" config both gained `base_url`, which is now required
  with `smtp`.

**Still unreached, and honestly `untested`:**

- **Row 20 (daemon + autostart) got nothing in round 8.** `autostartCmd` and
  `daemonCmd` were read and judged to add no CLI-specific surface — that is an
  opinion, not a test, and the row's CLI half stays uncovered. `bdrive daemon`
  and `bdrive autostart` still have zero `TestSec_*` coverage.
- **`X-Forwarded-Proto` is still trusted unconditionally in `requestBaseURL`.**
  Mail no longer depends on it (row 9), but `/join/<token>`, `verify_url` and
  `/s/<token>` URLs still take their scheme from a header, and `TrustProxy`
  lives on `Server` where `BuiltinAuth` cannot see it.
- **"`bdrive logout` does not stop a running daemon"** is still untested. The
  token is now revoked hub-side, so a running daemon's copy fails at the next
  remote call and degrades to `Offline` — that is a claim by reading, not a
  test, and `logoutNote` now says it in the CLI output.
- **Case-folding / NFC-NFD collisions** (`README.md` vs `readme.md`, `café.md`
  NFC vs NFD) are two `Op.Path`s and one file on APFS/NTFS. Still untestable
  portably, still unaddressed.
- **`SafePath` accepts every Windows-hostile spelling** — `..\`, `CON`/`NUL`/
  `AUX`/`COM1`, trailing dot or space — while `config.ReservedDir` already
  trims for exactly that reason. `GOOS=windows go build ./...` still fails, so
  nothing can run there yet.
- **GCS/S3 `List`/`Get` against a real bucket**; **expiry (row 1)**, which has
  no concept in the code; **Windows** generally.
- **`BuiltinAuth.Offboard` is still `func(email string)` with no error
  return**, so `Deny` cannot fail when offboarding fails.
- **`DELETE /api/auth/token` (new in r8) has only end-to-end coverage** — the
  CLI test drives it through the real binary against a real `BuiltinAuth`.
  Nothing attacks the route directly (a token revoking another token is
  impossible by construction; assert it anyway).

**New surface round 8's own fixes created — attack this first in round 9:**

- **Heir promotion on eviction picks the LOWEST ADDRESS** (`lowestMember`,
  `orgs.go`), because no join time is recorded per member. A member who picks
  a low-sorting address at signup is the designated heir of every org they are
  in. Is that reachable — can a member arrange for the sole owner to be
  offboarded, or simply wait? A `joined` timestamp per member is the real fix.
- **The pull-side RE-ASSERTION** (`syncer.Cycle` step 2b) writes ops into this
  device's journal carrying another device's original lamport and time, under
  this device's id and this account's identity, with a fixed `Note`. Attack:
  can a peer make a victim author ops it never made (History now credits the
  victim), or make a victim's journal grow without bound by withdrawing ops in
  a loop? Only content the device already holds is re-asserted, and a withdrawn
  slot is re-asserted once — verify both.
- **PKCE is refused only when the mix is wrong**: a pre-PKCE CLI (no verifier)
  on a new hub is still exchangeable by a code minted for any flow. The
  in-repo CLI always sends one; an older binary does not.
- **The device-flow link secret** (`cliGrant.link`) is resolved by a linear
  scan (`grantByLink`) and the poll id still opens the approval page. Attack
  the split: is the link value ever printed anywhere the poll id is expected,
  and does a grant with a duplicate link resolve to the wrong one?
- **`sizeBound`'s 1 MiB slack** is per object per cycle, so a hostile hub can
  still deliver declared+1 MiB every 10 seconds forever; and `maxImportBlob`
  is a package-level `var` a flag mutates.
- **`ResolveMount` now refuses a folder that claims a live mount id.** That is
  a denial primitive if a hostile folder can be made to sit at the registry's
  recorded path — check what happens when both copies exist, when the recorded
  path is a symlink, and when the registry row is edited by hand.

**Is the loop done after round 8? No, on both conditions.**

1. Every row is `clean` or `fixed` **except row 20**, whose CLI half
   (`bdrive daemon`, `bdrive autostart`) has no `TestSec_*` test at all, and
   row 1's expiry claim, which has no concept in the code to test. Rows 4, 12,
   13, 17, 18, 20 and 21 have never been sabotaged (there is no row 22 — round
   7's gap list named one; a fourth round-8 hacker is sabotaging rows 15 and 17
   as this is written, and its results arrive separately).
2. Round 8 was the opposite of dry: 26 distinct holes, 5 of them critical, and
   **three of round 7's own fixes turned out to be half-fixes** (the
   `$BDRIVE_HOME` guard, the mail-origin pin, the `observeDevice` move). Two
   consecutive dry rounds have not happened; the last dry round on any row was
   round 3 on row 13.

## Coverage gaps after round 7 — round 8's targets

Written by the CISO, verified against the tests that actually exist, and where
the claim was checkable I checked it rather than repeating it. **Be blunt
here: overstating coverage is the only way this process fails silently.**

**Claims from the round-7 hacker that I checked and found OVERSTATED:**

- **"Routes still uncovered: `/store/exists`, `/download`, `/remove`."** Not
  true as written. All three are exercised by `TestSec_*` tests — `/store/exists`
  in `sec_authz_test.go`, `sec_perm_test.go`, `sec_priv_test.go`,
  `sec_ledger_test.go` and `sec_sign_test.go`; `/download` in eight sec files;
  `/remove` in `sec_path_test.go` and `sec_journal_test.go`. What is true is
  narrower and should be stated that way: **they have permission-gate coverage
  and no route-specific attack test**. Nothing exercises `/store/exists`'s own
  behaviour, `/download`'s content handling, or `/remove`'s journaling beyond
  the path check.
- **"Exported functions still unreached: `SetDefault`, `SetCreator`,
  `SetTemplate`, `ClearPerm`, `Update`, `ManageURL`, `Endpoint`,
  `OpenSQLStore`, `AgentHeat`, `remote.ReportReads`."** Only **`Endpoint` and
  `AgentHeat`** have no test reference at all. `SetDefault`/`SetCreator`/
  `SetTemplate`/`OpenSQLStore` are in `db_conformance_test.go`, `ManageURL` in
  `directory_test.go`+`orgs_test.go`, `ClearPerm` in `sec_defer_test.go`,
  `remote.ReportReads` in `syncer/reads_flow_test.go`+`remote/http_test.go`.
  The correct claim: **all but `ClearPerm` are unreached by any `TestSec_*`
  test** — exercised for function, never attacked.

**Verified true and carried forward:**

- **The init tests all ran against a fixture hub with `auth.enabled: false`,
  so the login flow inside init was never executed** — and critical A lives in
  exactly the branch that skips it. **The auth-enabled branch of `ensureLogin`
  is untested.** This is round 8's first target: it is the one place a
  credential is minted and stored on a client.
- **CLI commands with zero `TestSec_*` coverage: `stop`, `import`,
  `autostart`, `read-log`, `daemon`.** (`init`, `login`, `share`, `forget`,
  `resume`, `whoami`, `status`, `scope`, `export`, `log`, `sync`, `url` are now
  driven.) `import` is the one that matters: see the leads below.
- **Rows never sabotaged at all: 4, 12, 13, 15, 17, 18, 20, 21, 22.** Rows 1,
  2, 3, 10, 11, 14 and 16 were sabotaged this round (see the round-7 table);
  rows 5 and 6 partially. **Rows 15 and 17 are the largest untouched
  surfaces** — the whole syncer receiving side and all of the client's local
  state — and row 15 is where the last three rounds' worst client findings
  came from. Sabotage them next.
- **`/api/auth/device/{start,poll}`** now has two tests but **no rate limit**;
  `poll` is a grant-id oracle and is unmetered.

**Leads with a confirmed mechanism and no reproducer — round 8 should turn
these into tests before hunting anything new:**

- **`apiDevicePoll` peeks then takes in two lock acquisitions** and **discards
  `take`'s return value**, so two concurrent polls of one approval mint two
  tokens (`authcli.go`, verified by reading). Round 2's seat-check race, on
  credential issuance.
- **`apiDevicePoll` mints the token under `req.Device`, chosen at POLL time,
  while the approval page showed `g.device` from START time** — what the human
  approved is not what lands in the device registry.
- **`/store/exists` calls `observeDevice` before the decision, on a `PermRead`
  route**, so a READ first-claims an unclaimed device id. This is the accepted
  face of round 6's journal-claim design decision, but it is the read door and
  round 5's fix was applied only to the write doors.
- **`bdrive import` aims at a project the archive names** (create-or-join-by-
  name, and the "must be empty" guard runs *after* the join) and has **no size
  cap on `spoolBlob`**. Zero `TestSec_*` coverage.
- **Unbounded reads on the sync path, both with the size in hand**:
  `syncer.go:504` `io.ReadAll` on a peer's journal (`o.Size` is in scope on the
  same loop iteration, unused) and `store.PutBlobReader`'s `io.Copy` (`op.Size`
  is in hand at the `pull` call site). Confirmed still unbounded this round.
- **`X-Forwarded-Proto` is trusted unconditionally in `requestBaseURL`** —
  it should be gated on `Server.TrustProxy` like `clientIP`, but `TrustProxy`
  lives on `Server` and `BuiltinAuth` cannot see it. A caller flips the scheme
  of mail links, `/join/<token>` and `/s/<token>` URLs.
- **`ValidateSignupPolicy` does not refuse `auth.smtp` set with
  `auth.base_url` empty** — the configuration in which the row-9 residual is
  reachable. Not added because `gating_test.go:170` constructs exactly that
  combination and would go red; that is a non-`TestSec_` test and someone has
  to decide.
- **`bdrive logout` does not stop a running daemon, and there is no
  device-token revocation route at all.**
- **`BuiltinAuth.Offboard` is `func(email string)` with no error return**, so
  `Deny` still cannot fail when offboarding fails. Round 7 removed the swallowed
  *refusal*; a swallowed store *error* remains.

**Still never reached by any round:**

- **GCS/S3 `List`/`Get` against a real bucket** — the signing arms run offline
  against synthetic credentials.
- **Expiry (row 1)** — deferred by decision, no concept in the code.
- **Windows** — `internal/autostart`'s Windows tests have still never executed,
  and `GOOS=windows go build ./...` still does not pass.

## Coverage gaps after round 6 — round 7's targets

Written by the CISO, verified against the tests that actually exist. A row
being `clean` or `fixed` above means one attack was refused, not that the
boundary is exhausted. **Be blunt here: overstating coverage is the only way
this process fails silently.**

**Rows the round-6 hackers CLAIMED but did not really exercise:**

- **Row 3 (routes outside `proj()`), row 12 (secret leakage) and row 16
  (frontend shell) were never sabotaged**, by the audit's own admission. They
  are `clean`/`fixed` on round 1–3 tests that have never been shown to fail
  when their fix is removed — which is precisely the class the sabotage table
  found five members of. Sabotage these three first.
- **`orgs.go`'s invite-revocation durability, `devices.go`'s `MayActAs`, and
  `upload.go`'s reserved-dir guard** were listed as sabotage targets and
  skipped because the whole-file revert did not compile. Nobody has shown
  those three fixes are load-bearing. Revert them by hand.
- **`bdrive init` end to end against a hub — still never driven.** Round 6
  attacked two ARTIFACTS init produces (the `.bdriveignore` scope block, the
  `.bdrive/config.json` a folder carries) by writing them directly, and
  called that CLI coverage. It is not. Untested: the generated hook command
  with a hostile project name, the autostart plist written during init, init
  inside another mount or inside `$BDRIVE_HOME`, init against a hostile hub,
  and whether a FAILED init leaves hooks or an autostart entry behind. Rounds
  2 and 5 both found injection exactly here.
- **Row 19 (the device as client of a hostile hub)** got nothing new this
  round. It still covers what the hub SAYS and not what it SERVES: a 10 GB
  body for a 3-byte blob, a `Content-Type` the viewer trusts, a journal 200
  that is an HTML error page.

**Routes and commands with zero `TestSec_*` coverage** (`server.go` route
table cross-checked against every `TestSec_*` name):

- `/store/exists`, `/api/auth/device/{start,poll}`, `/download`, `/remove` —
  token-only coverage, unchanged since round 5.
- **11 CLI commands no `TestSec_*` drives**: `login`, `logout`, `stop`,
  `forget`, `share`, `import`, `resume`, `autostart`, `read-log`, `whoami`,
  `daemon`. (Round 6 closed `export`, `scope`, `status`; `log`, `restore`,
  `sync`, `init`-adjacent files were already partly covered.)
- **Exported functions still unreached by any test name**: `SetDefault`,
  `SetCreator`, `SetTemplate`, `ClearPerm`, `Update`, `ManageURL`, `Endpoint`,
  `NewCLIAuth`, `OpenSQLStore`, `AgentHeat`, `PutBatch`, `DeleteBatch`,
  `Paused`, `SetPaused`, `remote.ReportReads`.

**Round 6's own fixes, which round 7 should attack first** — this is where the
defect rate lives (2, 5, 7, 5 broken fixes in four rounds):

- **`Server.offboard`.** It runs under no lock across three registries and is
  called from inside `Deny`'s critical path. What happens when it fails
  halfway (project grants cleared, org membership not)? Is `dropPerm`'s
  skipped last-admin guard reachable any other way? Does a hub with a
  non-`BuiltinAuth` provider have an account-removal path at all?
- **`mailBaseURL`'s pin-on-first-use.** The first request a hub serves decides
  where every reset mail points for the process's lifetime. Can an attacker
  BE that first request?
- **`reserveIfFits` holds `resMu` across `quota().CheckWrite`.** A quota
  provider that blocks (a managed deployment's network call) now blocks every
  presign on the hub. And the 24h backstop is a new unbounded-ish window.
- **`pull`'s line-boundary resume + shrink guard.** Attack a journal that
  keeps its op count and changes content; a first line that is itself torn
  (no `\n` anywhere); and the interaction with the `o.Size <= localSize` gate.
- **`DisplayTime` returning the zero time.** Anything that sorts, groups or
  formats on it (`groupRuns`, the History API, the frontend).
- **`unopenableLockIsRunning`'s in-process `held` set.** It is now the only
  thing standing between an unopenable lock and "no daemon". Does `Run` always
  reach `release`?
- **`PutAccount`'s insert-only rule.** It compares emails case-insensitively.
  What does a legitimate email CHANGE look like now — is there one?
- **`trimName`'s new filter.** It runs on create and rename; `Update` has its
  own path (`maxNameLen`). Do both agree?
- **`templates.SafePath` vs `cleanUploadPath` vs `unsafeRel`.** Three spellings
  of one rule now. Find a path two of them disagree about.

**Still never reached by any round (no test names it at all):**

- **GCS/S3 `List` and `Get` against a real bucket** — the signing arms run
  offline against synthetic credentials; no test has driven those backends'
  key handling end to end.
- **Expiry** (row 1) — still deferred by decision, still no reproducer,
  because the concept does not exist in the code.
- **The Postgres arm is not reproducible from a plain run.** It is now at
  least VISIBLE: `TestSec_Suite_RunModeIsVisible` prints to stderr when
  `BDRIVE_TEST_POSTGRES` is unset, and FAILS under `-short` naming the tests
  `-short` removes. Before this round nothing in-repo pinned either.

## Coverage gaps after round 5 — kept for the record

Written by the CISO, verified against the tests that actually exist.

**The meta-finding, acted on: `permHub` builds a hub with `srv.Devices == nil`.**
Every device-ownership decision resolves through the registry, so in that
fixture `ownJournal`'s account binding is INERT — it returns early. Which
earlier results this makes vacuous, plainly:

- `TestSec_Perms_StoreAndUploadRoutesUnderDeviceToken`, `TestSec_Row3_*`,
  `TestSec_CrossOrg_ProjectRoutesRefuseOutsider` and every other permHub-based
  test that pushes a journal proved **permission** (org/project level), never
  **device ownership**. They are still valid for what they assert.
- Round 1's `TestSec_Store_ForeignDeviceJournalWrite` measured the key↔header
  match only — which round 4 showed was satisfiable by construction.
- Only the tests that install a registry themselves (`secfx4Registry`,
  `TestSec_Store_MemberCannotWriteAPeersJournalByRenamingItself`,
  `TestSec_Browser_JournalKeyMustNameARegistrableDevice`, the round-5 journal
  four) have ever exercised the binding at all.
- **I did not change `permHub`** (it would move a dozen existing tests'
  behaviour in one commit). The honest reading: on a fixture with no registry
  the hub falls back to the key↔header match plus `projectPerm`, which is what
  a single-volume/auth-less hub does too. `cmd/bdrive/web.go` always sets
  `Devices` in hub mode, so no served configuration has that gap — and
  `TestSec_Config_NoServedConfigurationReachesTheAdminEscape` is the test that
  now says so.

**The floor for round 6 — carried verbatim from round 5's completeness sweep:**

- **zero-test packages**: `internal/templates` — project seeding writes
  attacker-named files into a fresh project and has never been touched.
- **exported functions no `TestSec_*` names**: 35 in `webapp`, 14 in `store`,
  4 in `syncer` (`Restore`, `Explain`, `NotSyncedFiles`, `PruneDir`), 1 in
  `remote`.
- **routes with only token coverage**: `/store/exists`,
  `/api/auth/device/{start,poll}`, `/download`, `/remove`.
- **15 of 22 CLI commands that no `TestSec_*` drives**, including `init` — the
  front door for hooks, autostart, scope-file writing and the device token.
- **`encodeCursor`'s `UnixNano` overflow past 2262** — named three rounds,
  never reached.
- **account ids are 32 bits with no uniqueness check** (`"u-"+randHex(4)`,
  `a.users[u.ID] = u` unguarded) — an overwrite primitive against a live
  account.
- **`sizeFitsContentAddress` only catches zero**, so a 1-byte declaration for
  a 4 GiB presigned upload is still a signed, quota-charged lie on both doors.

**Round 5's own fixes, which round 6 should attack first (this is where the
defect rate lives — 2, then 5, then 7 broken fixes in three rounds):**

- **`DeviceRegistry.OwnerOf` + the "first push must name its own device" rule.**
  Two requests still claim an unclaimed id. Attack the read door
  (`GET /store/list` as a claim), the ops-consistency rule (set `device` and
  claim anyway), and the new **project-admin override** — an admin may now
  write any journal in their project, which is a forgery primitive against a
  member's history that did not exist before this round.
- **`pull`'s byte-offset resume.** What happens when a peer's journal shrinks,
  or when the local copy is truncated? `o.Size <= localSize` still skips the
  fetch entirely.
- **`journalOps` reads the whole journal body into memory** on the hub's
  busiest write path before deciding anything.
- **The reservation ledger** (`reserve.go`, new in r5): unbounded in principle
  (one entry per outstanding grant, pruned on expiry and on every reserve),
  process-local, and the only thing standing between a signed URL and an
  unbilled object.
- **The refused cross-origin redirect** — is there a same-origin redirect loop
  that still burns the client?
- **`Filter.underMountOnDisk`** stats ancestors during scan and materialize and
  memoizes misses per cycle: a directory that BECOMES a mount mid-cycle, and
  the cost on a deep tree.
- **`killToken`'s voided row** — a token row with no user now persists on disk
  when the delete fails. Does anything else read those rows?
- **`safeField`** — it strips C0 and bounds bytes; it does not touch C1
  (0x80–0x9F, which some terminals treat as CSI) or right-to-left overrides.

**Still never reached by any round (no test names it at all):**

- **GCS/S3 `List` and `Get` against a real bucket** — the signing arms run
  offline against synthetic credentials; no test has driven those backends'
  key handling end to end.
- **Expiry** (row 1) — still deferred by decision, still no reproducer,
  because the concept does not exist in the code.

## Coverage gaps after round 4 — kept for the record

Written by the CISO, verified against the tests that actually exist. A row
being `clean` or `fixed` above means one attack was refused, not that the
boundary is exhausted.

**Closed this round (was on round 3's gap list, now has an asserting test):**

- **`store/sign` on a backend that can actually presign** — three rounds listed
  it, three rounds skipped it. `sec_sign_test.go` supplies a fake `PutSigner`
  in ~40 lines, and the branch held three real holes (content-address
  poisoning, the zero-size lie, unbilled bytes) plus five clean assertions.
  This is the single best argument in the file for writing the fixture instead
  of listing the gap again.
- **The whole client** — `internal/{store,config,journal,remote}` and
  `cmd/bdrive` had never been attacked. Five of the seven packages in this
  round were first contact, and the two worst findings of the round were there.

**Still never reached by any round (no test names it at all):**

- **`internal/daemon` and `internal/autostart`** — zero security tests after
  four rounds. The daemon runs unattended, holds the flock that is the entire
  liveness story, and writes `daemon.pid`/`daemon.log` into `$BDRIVE_HOME`
  (`daemon.log` is not covered by this round's 0600 change — check it). The
  autostart unit is a file that runs a binary at every login: what happens when
  `selfPath` is not what it was, or when the mount registry it reads is
  hostile?
- **The browser's presigned commit flow** (`SignBlobPut` → `BlobSize` →
  `Commit`). Round 4 attacked the DEVICE door (`/store/sign`) and they share no
  guard: `handleUploadCommit` decides what to journal from a size it reads back
  out of storage. Now that a signing fixture exists, this is cheap.
- **`Op.Note` reaching `bdrive log`'s terminal output** — a peer's string, in a
  terminal, unescaped. Still argued, never tested.
- **`Op.Mtime` and `Op.Seq` at extremes** — still "argued subsumed by Lamport",
  which is sound for ordering and says nothing about `Mtime` reaching a
  formatter or `Seq` sizing anything.
- **GCS/S3 `List` and `Get` against a real bucket** — the signing arms now run
  offline against synthetic credentials, but no test has ever driven those
  backends' key handling end to end.
- **`Dir == nil || Auth == nil → PermAdmin`** (row 2) and **expiry** (row 1) —
  still deferred by decision, still no reproducer.

**Claimed but thin — a test exists, but only for one narrow shape:**

- Row 14's `NULBytesDoNotTruncateRecords` is `clean` on file+sqlite and RED on
  Postgres. Unchanged, still a documented divergence rather than a closed row.
- Row 19 covers what the hub *says* (keys, redirects, sizes, TLS). It does not
  cover what the hub *serves*: a hub that returns a 10 GB body for a 3-byte
  blob, a `Content-Type` the viewer trusts, or a journal 200 that is actually
  an HTML error page.
- Row 17's permission tests check three files. `daemon.log`, `daemon.pid`,
  `mounts.json`, `device.json` and the blob store itself are not asserted.

**Fixes made this round that deserve their own next-round attack:**

- **`DeviceRegistry.LookupIn` is now three things at once** — the display join,
  the org-scoping wall, and (through `ownJournal`) the write gate on the
  journal key. `FirstSeen` is the ownership fact; a zero `FirstSeen` sorts
  oldest, which is the direction that favours an existing row. Attack the
  unclaimed-id window and the two-accounts-one-machine case named in
  known-open.
- **`store.UnderRoot`** — one implementation now guards both the syncer's mount
  boundary and the `file://` backend's storage root. It resolves the deepest
  existing ancestor, so it answers about the filesystem as it is *now*: it is
  a check, not a lock, and the window between it and `os.Rename` is real.
- **`RemoteSource.OpenBlob`'s verified-set** — an in-process map keyed by sha,
  no bound, populated by anything that reads a blob. Memory growth, and the
  fact that a verified blob is never re-checked, are both worth poking.
- **`journal.Parse`'s skip** — a line that decodes on one Go version and not
  another (or a field type change) now silently disappears instead of failing
  loudly. Is there a shape that decodes DIFFERENTLY for two readers?
- **`prefixed.safeKey`** — it refuses rather than normalizes, and it allows a
  trailing slash so `List` keeps working. Check the empty prefix, `"/"`, and
  what a backend does with `<project>//blobs/x`.
- **`sizeFitsContentAddress`** — it only catches zero. A 1-byte declaration for
  a 4 GiB upload is still a signed, quota-charged lie on every signing hub.
