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

| # | Boundary | State after round 4 | Attacks that must be tried |
|---|----------|---------------------|----------------------------|
| 1 | Auth gate (`auth.go:authGate`) | **clean** — `TestSec_AuthGate_AnonymousPathTricksCannotReadAPI`, `…CannotWrite`, `…ConfigLeaksNothingToAnonymous`, `…ForgedAndTamperedCredentialsRefused`, `…CredentialDiesWithAccountAndMembership`, `TestSec_Path_LogoutRevokesTheTokenNotJustTheCookie`. **No TTL exists to test** — see "nothing expires" below. | reach any `/api/**` with no/expired/forged credential; abuse the `!HasPrefix("/api/")` open-path rule; path tricks (`//api/`, `/api/../`, encoded) that route to a handler but read as "open" |
| 2 | Per-project permission choke point (`perms.go:projectPerm/requirePerm`, `server.go` route table) | **fixed** (r1) — `TestSec_Perms_RemovedOrgMemberLosesProjectAccess`, `…OrgLessProjectIsNotAdminForEveryone`. **clean** — `…ReadOnlyMemberCannotWrite`, `…WriteMemberCannotAdmin`, `…NoneMemberReachesNothing`, `…CorruptGrantFailsClosed`, `…NoneMemberCannotListProjectSharesViaOrg`, `…StoreAndUploadRoutesUnderDeviceToken`. `s.Dir == nil \|\| s.Auth == nil → PermAdmin` **still open, still deliberately deferred**, untested. | `read` member performing any `PermWrite` action; `write` member performing `PermAdmin`; `none`/non-member reaching a project; the fail-open escapes reachable on a configured hub |
| 3 | Routes **outside** `proj()` | **fixed** (r1) — `TestSec_Row3_OrgSharesLeaksDeniedProject`, `…ExpiredShareRevokableByOutsider`. **clean** — `…ShareMutationByOutsider`, `…PermissionRoutes`, `…ProjectLifecycleRoutes`, `…OrgRoutes`, `…InviteAccept`, `…AdminRoutes`. | each one, exercised by a non-member, a read-only member, and a non-owner |
| 4 | Cross-org isolation (`orgs.go`, `projects.go`, `directory.go`, `remote/prefixed.go`) | **clean** — `TestSec_CrossOrg_ProjectRoutesRefuseOutsider`, `…OrgRoutesRefuseOutsiderAndNonOwner`. Round 2 found two cross-org leaks that entered through OTHER surfaces (rows 10 and 11), both now **fixed**. **fixed** (r4) — `TestSec_Prefixed_KeyCannotEscapeTheProjectNamespace`, `…ListedKeysStayInsideTheNamespace`: `remote.Prefixed` is the single containment primitive for multi-tenancy and it was string concatenation — `..` crossed into another project on Put/Get/Exists/SignPut, and `List` filtered on `HasPrefix` then trimmed, handing an escaping key back as an in-project one. No reachable caller today; every gate that saved it lives in `webapp` and the wall is in `remote`. **clean** (r4) — `…SiblingWithAPrefixNameIsNotListed`. | project id from org B against every route; `/api/projects` and `/api/orgs` leaking names/ids; org rename/member routes on someone else's org; a key that leaves the project prefix in either direction |
| 5 | Sync proxy `/store/*` (`store.go`, `remote/http.go`) | **fixed** (r1) — `TestSec_Store_ForeignDeviceJournalWrite`, `…BlobContentMustMatchItsKey`, `…QuotaHonorsUnsizedPut`. **clean** — `…KeyEscapesRefused`. **fixed** (r2) — `TestSec_Leak_ErrorBodiesRevealServerFilesystemPaths`. **fixed** (r3) — `TestSec_Heat_StoreRouteCannotMintAnArbitraryHeatActor`. **fixed** (r4), the round's worst hub finding — `TestSec_Store_MemberCannotWriteAPeersJournalByRenamingItself`: `ownJournal` bound the journal key to the `X-Bdrive-Device` header of the *same request*, so moving both together satisfied it by construction and any member replaced any peer's journal object (their ops gone, every peer replaying the forged deletes, History crediting the victim). Ownership is now resolved through `DeviceRegistry.LookupIn` — first account seen syncing under the id, scoped to the project's org. **The presign branch ran under a test for the first time in four rounds** (`sec_sign_test.go`, a fake `PutSigner`): **fixed** — `TestSec_Sign_DirectUploadCannotPoisonAContentAddress` (bytes bypass the hub, so round 2's content-address guard was simply absent; blobs are now verified on read, once per blob, on any signing backend), `…DeclaredSizeCannotUnderstateTheContent` (the zero-size lie `/upload/init` refuses), `…DirectDeviceUploadIsBookedAgainstTheQuota` (every device blob was free on an S3/GCS hub). **clean** (r4) — `…JournalKeyIsNeverPresigned`, `…SignedTargetStaysUnderTheProjectPrefix`, `…ExpiryIsTheConfiguredTTL`, `…ReadOnlyMemberAndOutsiderGetNoSignedURL`, `…BlobSignedForOneProjectIsNotVisibleInAnother`. | write a different device's journal key; key traversal; read another project's blob by sha; `store/sign` minting a URL outside the prefix or for a journal key |
| 6 | Upload (`upload.go`) | **fixed** (r1) — `TestSec_Upload_ReservedDirsRefused` + `TestSec_Sync_PeerJournalCannotMaterializeReservedPaths` (`internal/syncer`), `…QuotaUsesRealSize`. **fixed** (r2) — `TestSec_Path_DirUploadCannotEscapeThroughSymlink`. **fixed** (r3) — `TestSec_Path_RefusedUploadCreatesNothingOutsideTheServedFolder`. **clean** — `…TargetStaysInProject`, `TestSec_Path_WriteRoutesRefuseTraversal`, `TestSec_Path_RestoreRefusesForeignSHA`, `TestSec_Path_UploadOntoASymlinkedNameDoesNotFollowIt`. The declared-size guard is now a shared helper (`sizeFitsContentAddress`) both doors call. **The browser's presigned commit flow (`SignBlobPut`→`BlobSize`→`Commit`) is still untested** — round 4 attacked the device door only. | presigned target outside the project prefix; `upload/commit` journaling `..`/absolute; committing content never uploaded; quota bypass |
| 7 | Share links (`shares.go`, `ratelimit.go`, `server.go:handleShared`) | **fixed** (r1) — `TestSec_Share_OrgAuditLeaksDeniedProjectTokens`, `…RateLimitIgnoresSpoofedForwardedFor`, `…ErrorResponsesKeepSandboxCSP`, `…OutsiderCannotRevokeExpiredShare`. **fixed** (r2) — `TestSec_Share_RemovedOrgMemberLinkStopsServing` (offboarding now ends a link; resolved at read time in `shareCreatorStillBelongs`). **fixed** (r3) — `TestSec_Share_CreatorMembershipIsResolvedFailClosed` (round 2's own fix failed OPEN when the project's org was empty or unresolvable: clearing a project's org resurrected every offboarded member's public link). **clean** — `…RevokedAndExpiredTokensAreDead`, `…NoAuthCookieOnPublicResponse`, `…LiveShareMutationNeedsWrite`, `…DemotedMinterCannotManageTheirLink`, `TestSec_Share_PublicHitRecordsShareKindEndToEnd`, `…VisitorCannotInflateOrRedirectTheLedger`, `…DeadLinksRecordNothing`, `TestSec_Path_HostileBlobCannotRepointALiveShare`. **fixed** (r3) — `TestSec_RateLimit_TrustedProxyUsesTheHopItAdded` (with `trust_proxy` on the limiter keyed on the FIRST `X-Forwarded-For` entry, which the client prepends — so turning the flag on disabled the limiter it was added to fix; it now takes the last hop). **fixed** (r4) — `TestSec_RateLimit_TrustedProxyIgnoresAnExtraForwardedForLine`: round 3's "last hop" was read with `Header.Get`, i.e. the first field *line* only, so a client that added its own line owned the whole key again and the login limiter was off for the third round running. It now reads `Values()` and takes the last element of the last line. | revoked/expired token still serves; token guessable; missing CSP `sandbox`; auth cookie on `/s/*`; rate-limit bypass; share by someone who lost access |
| 8 | Invites & signup (`authlocal.go`, `authcli.go`, `orgs.go`) | **clean** — `TestSec_Invite_ForgedExpiredRevokedCannotCreateAccount`, `…RedemptionIsOrgScopedAndRevocable`, `…OnlyOwnersMintAndListLinks`, `…CLIOneTimeCodesAreNotReplayable`, `…SeatCheckCannotBeSkipped`. **fixed** (r2) — `TestSec_Invite_SeatCheckIsAtomic` (check-then-act race on the last seat), `TestSec_DB_RevokedInviteMustNotSurviveAFailedWrite` (revocation that only looked durable). | account created while `allow_signup:false`; invite reused past expiry/revocation; invite for org A joining org B; `signupInvited` skipping gates; seat check skipped or raced; CLI codes replayable |
| 9 | Password & token handling (`authlocal.go`) | **fixed** (r1) — `TestSec_Password_ResetRevokesExistingTokens`. **fixed** (r2) — `TestSec_Path_AuthNextCannotLeaveTheHub` (open redirect off the sign-in page via `/\`, `/<TAB>/`). **clean** — `…ResetGrantIsSingleUseAndExpires`, `…LoginAndResetDoNotEnumerateAccounts` (body/status only), `…NoCredentialMaterialInResponses` (responses only), `…ResetKillsCLIIssuedToken`, `TestSec_Path_VerifyGrantIsSingleUseAndTypeBound`. **fixed** (r3) — `TestSec_Leak_ResetTimingDoesNotEnumerateAccounts` (on a hub with SMTP, `POST /auth/reset` blocked on the mail dial only for addresses that exist, and was not rate limited; mail now goes out off the request path and `/auth/reset` joins `rateLimitAuth`). **clean** (r3) — `TestSec_Password_LoginTimingDoesNotEnumerateAccounts`, `TestSec_Leak_NewLogLinesCarryNoCredential`, `TestSec_Path_NextCannotLeaveTheHubOnAnyAuthRoute` (`safeNext` against 20 hostile values on every auth route). | reset token replay/expiry; reset for another account; enumeration via response or timing; non-constant-time compare; credentials in a log line |
| 10 | Read-heat privacy (`reads.go`, `handleHeat`) | **fixed** — `TestSec_Heat_ByDeviceLeaksForeignDeviceMetadata`, `TestSec_Heat_ReadReportCannotInjectAnIdentity`, `TestSec_Reads_ReportCannotRewriteAnotherOrgsDevice` (the device id a client reports is validated before it becomes an actor, `devices.go:ownsDevice`). **fixed** (r3) — `TestSec_Heat_PlantedIdentityCannotBeSelfRegisteredThenReported`, `TestSec_Heat_StoreRouteCannotMintAnArbitraryHeatActor`, `TestSec_Devices_IdCannotBeSquattedBeforeItsOwnerRegisters`, `TestSec_Devices_SquattedIdStillCountsItsOwnersReads`, `TestSec_Reads_OneUnstorableBucketCannotWedgeTheLedger` (a single NUL-bearing path from a read-only member wedged the whole hub's telemetry forever on Postgres). **clean** — `…NoQueryShapeLeaksAnActor`, `…RefusedWithoutReadPermission`, `TestSec_Reads_MalformedReportsStayHarmless`, `TestSec_Devices_ConcurrentRegistrationLeavesOneConsistentOwner`, `TestSec_Heat_ReaderDifferencingCannotNameAReader` + `…NestedPrefixAndDayWindowsCarryNoActorAxis` (**the reader-differencing oracle does not exist**: 112 query shapes, byte-identical responses), `TestSec_Ledger_ReplicationAndHistoryViewsAreNeverReads`. **fixed** (r4) — `TestSec_Devices_MemberCannotRelabelAnotherMembersDeviceInHeat` (see row 14: `LookupIn` returned the most recently OBSERVED row for an id regardless of owner, so a same-org member relabelled a peer's device in `/heat?by=device` with one ordinary store request). Design conflict resolved in favour of "`?by=device` may report an owned device id"; `reads.go`'s comment and CLAUDE.md now say the same thing. | any email, device id or token reaching a client through `/heat`, its errors, or `/api/p/<id>/reads`; heat for a project you can't read; the reader-differencing oracle |
| 11 | Path handling (`dir.go`, `handleFile/Download/Render/Blob`) | **fixed** — `TestSec_Path_ViewerBlobEscapesProjectPrefix`, `TestSec_Path_MemberReadsAnotherOrgsBlob` (a journal's `Blob` was an unvalidated storage key: read any file on the hub host, any org's), `TestSec_Path_BlobInlineHTMLIsSandboxed` (stored XSS on the hub origin via history `/blob`). **fixed** (r3) — `TestSec_Journal_HistoryDeviceFieldLeaksForeignDeviceMetadata`, `…IsNotAnExistenceOracle` (History joined the registry on the op's own `Device` field — client-asserted JSON, not the journal KEY round 1 bound; attribution now comes from the journal the op was read from, and the registry join is org-scoped), `TestSec_Journal_SizeFieldCannotForgeContentLength` (`Op.Size` was echoed as `Content-Length` for bytes the hub never measured). **fixed** (r4) — `TestSec_Devices_MemberCannotRelabelAnotherMembersDeviceInHistory` (the registry join behind History picked the freshest row for a device id, whoever owned it: one store request from a same-org member relabelled a peer's device on every change in the audit feed), `TestSec_Local_SymlinkInsideTheRootIsNotAWayOut` (round 3's `localBackend.path` guard is lexical and `os.Open`/`os.Rename` follow links, so a symlink anywhere inside a `file://` storage root read and wrote anywhere on the hub host; the check now resolves on disk via `store.UnderRoot`). **clean** — `…ShaParamsRejectNonHex`, `…ShaFromAnotherProjectMisses`, `…DirViewerRefusesTraversal`, `…DirSymlinkIsNotServed`, `…SingleVolumeRoutesAreModeScoped`, `TestSec_Journal_HostilePathCannotBeLaunderedThroughRestoreOrRemove`, `TestSec_Path_ValidBlobHashStaysInsideItsProject`, and **new in r4** `TestSec_Journal_ContentLengthAlwaysMatchesTheBodyServed`, `TestSec_Local_ListAndExistsCannotEscapeTheStorageRoot`, `TestSec_Devices_LookupScopeIsTheProjectsOrgNotTheCallers`, `TestSec_Devices_HistoryFallbackDoesNotDistinguishUnknownFromDenied`. | `..`, absolute paths, symlinks, encoded separators, NUL — reaching a file outside the project root or the served folder; every journal field (`Blob`, `Path`, `Device`, `Size` now audited; `Mtime`/`Seq` argued subsumed — see gaps) |
| 12 | Secret leakage (`handleConfig`, `web.go`, error bodies) | **fixed** — `TestSec_Leak_ErrorBodiesRevealServerFilesystemPaths` (storage paths / bucket+key in `/store/object`, `/file`, `/download`, `/render`). **clean** — `TestSec_Leak_NothingSensitiveForAnOrdinaryMember`, `TestSec_AuthGate_ConfigLeaksNothingToAnonymous`, `TestSec_Password_NoCredentialMaterialInResponses`. **fixed** (r3) — `TestSec_Leak_RealConfigPathKeepsSecretsOffTheWire` (the real config path set `srv.Volume` from the storage URL, so anonymous `/api/config` named the bucket — `s3://acme-prod-drive`; it now defaults to a storage-independent name and `--volume`/`volume:` stays the only way a storage string reaches the wire). **clean** (r3) — `TestSec_Admin_PolicyCannotWidenServerOwnedAccess`, `TestSec_Leak_NewLogLinesCarryNoCredential`. A hub built the production way is now instantiated by a test (real DSN, real SMTP password, `--upload`). | storage credentials, bucket URL, DB DSN, SMTP password, the hub's own device token reachable by any client; stack traces or internal paths in errors |
| 13 | Agent hook guard (`internal/agenthooks`) | **fixed** — `TestSec_Hooks_GuardNeverSpawnsBdriveOutsideAMount` (a newline in a directory name split the `grep -F` pattern and matched every mount), `TestSec_Hooks_InstallKeepsItsOwnUserConfig`, `…InstallFromHomeKeepsItsOwnUserConfig` (init silently deleted the hooks it had just written when `$HOME` is a git repo). **clean** — `…MountPathMetacharactersNeverExecute`, `…RegistryContentsNeverExecute`, `…EveryHookCommandIsGuarded`, and **new in r3** `…GuardStaysClosedForEveryControlCharacterInPWD`, `…GuardDoesNotTrustAnInheritedPWD`, `…GuardIsStillPureShell`, `…GuardStillFiresInsideARealMount` — 17 `$PWD` shapes across all three command builders including `hookPullCommand`, which round 2's regression test never exercised. **Round 3 came back dry here: this row's first dry result.** | shell injection through a mount path, project name, or file path into the inline hook command |
| 14 | Metadata store (`db_sql.go`, `db_file.go`) | **clean** — `TestSec_DB_HostileStringsStayDataOnEveryBackend`, `TestSec_DB_QueryRewriteOnlyEverSeesStaticSQL`, `…PlaceholderRewriteIsPositional`, `…QuestionMarksInValuesDoNotShiftPlaceholders` — **now verified on file, sqlite AND a real Postgres 16** (`BDRIVE_TEST_POSTGRES`, run this round). `…NULBytesDoNotTruncateRecords` is clean on file+sqlite and **RED on Postgres** — see "known-open". **fixed** (r3) — `TestSec_DB_EveryRegistryAccessorHandsOutACopy`, `…RollbackHoldsUnderConcurrentMutators`. **fixed** — `…OrgMemberMapDoesNotEscapeTheRegistry`, `…ProjectPermsMapDoesNotEscapeTheRegistry` (live maps handed out — a role/grant writable past every guard, plus a real `concurrent map iteration and map write` crash), `…FailedGrantWriteLeavesRegistryAgreeingWithDisk` (refused writes applied in memory), `…RevokedInviteMustNotSurviveAFailedWrite`, `…FileBackendSecretsDirectoryIsNotWorldReadable`. **fixed** (r4) — `TestSec_Devices_OwnershipSurvivesAHubRestart`: round 3's `(account, id)` device rekey was **in memory only** — `DeviceRepo.Put` keyed on the id alone in both backends, so two accounts' rows collapsed on disk and after any restart the hub refused the real owner, exactly the outcome the rekey claimed to dissolve. Both backends are now keyed `(user_email, id)` (`device_rows` table, migrated from `devices`) and rows carry `FirstSeen`, which is the ownership fact everything else resolves against. | SQL injection on any repo method; a record write crossing org/project scope; the file backend's atomic-rewrite path corrupting or exposing another tenant's rows |
| 15 | Peer journal on the RECEIVING device (`internal/syncer`: `materialize`, `Cycle`) | **fixed** (r3) — `TestSec_SyncJournal_PeerCannotMaterializeOutsideTheMount` (`..` in `Op.Path` resolved above the mount root: one pushed JSONL line wrote `~/.ssh/authorized_keys` on every teammate's machine), `…ReservedDirGuardIsCaseInsensitive` (`.GIT/hooks/pre-commit` cleared an exact-match guard and APFS/NTFS resolved it into the real `.git/hooks`), `…PeerCannotSetSetuidOrSetgidMode` (`Op.Mode` went to `os.Chmod` verbatim, setuid bits included), `…ExtremeLamportCannotFreezeADevice` (`Lamport: MaxInt64` wrapped a victim's clock negative and silently reverted its own edits forever). **fixed** (r4), the round's worst client findings — `TestSec_SyncJournal_PeerCannotMaterializeThroughASymlinkedDirectory` + `TestSec_SyncPeer_MaterializeCannotWriteThroughASymlink` (`unsafeRel` judges the path's SPELLING; `MkdirAll`/`CreateTemp`/`Rename` follow symlinks, and `walkFolder` refuses to descend into one — so a symlinked directory in a mount was a one-way door that took peer writes and never reported them. `writeFile` now resolves the boundary on disk, before it creates anything), `TestSec_Store_BlobKeyCannotEscapeTheBlobDir` + `…ShortBlobKeyIsRefusedNotFatal` (`Op.Blob` reached `store.BlobPath` unchecked: `"blob":"../secret.txt"` made `HasBlob` true, so `pull` skipped hash verification and `OpenBlob` handed any file on the teammate's machine to `writeFile` as that path's content — and a Blob under two characters panicked the daemon), `TestSec_SyncJournal_UnwritablePathCannotWedgeTheCycle` + `TestSec_SyncPeer_OneUnwritablePathCannotWedgeTheCycle` + `…ShortBlobStringCannotCrashTheCycle` + `…HostileDeviceNameCannotBreakTheConflictCopy` (four ways one peer op permanently killed sync on every device that pulled it — `materialize` now skips and logs per path, `pull` skips an unfetchable blob, `shortSha` replaces `op.Blob[:12]`, and `conflictName` bounds both variable parts), `TestSec_SyncPeer_IgnoreFileReloadCannotDropTheNestedMountBoundary` (reloading the filter after a pulled `.bdriveignore` handed `materialize` a fresh `Filter` whose `nested` list was empty, so one project wrote into another project's working folder), `TestSec_SyncJournal_CeilingLamportCannotFreezeADevice` + `…LocalClockStillAdvancesAfterAHostileLamport` (round 3's ceiling was inclusive in both directions, so `1<<62` was absorbed and then pinned the clock there forever — round 3's own silent write lock, reachable with the one value the clamp accepts), `TestSec_SyncJournal_ReservedDirGuardCoversFilesystemFoldings` (`EqualFold` misses the spellings NTFS/SMB fold away: `.git./hooks/pre-commit` IS `.git/hooks/pre-commit` there), `TestSec_SyncPeer_PruneRefusalCannotBeRacedByAPushedIgnoreFile` (the CLI's `!`-rule refusal read `.bdriveignore` before the cycle and `pruneOps` read it again after the pull replaced it — two reads of two different files, so an ordinary `bdrive scope` by a teammate turned a cleared `--prune` into a hub-wide delete; `pruneOps` now re-runs the refusal against the rules it is about to apply). **clean** — `…HostileDeviceKindAndSizeStayInert`, `TestSec_Sync_PeerJournalCannotMaterializeReservedPaths`, and **new in r4** `…SafeModeCacheAgreesWithDisk`, `…DegenerateRelativePathsMaterializeNothing`, `TestSec_SyncPeer_BlobContentMustHashToTheShaTheOpNames`. | every field of an op a peer pushes, applied to a victim's disk: path escape, reserved dirs, mode bits, clock values |
| 16 | Frontend shell + embedded assets (`server.go:Server.frontend`) | **fixed** (r3) — `TestSec_Frontend_ShellCarriesFramingAndSniffingDefenses` (the one page carrying the session cookie had no `X-Frame-Options`, no `frame-ancestors`, no `nosniff`), `…ImmutableCacheOnlyOnRealAssets` (a miss under `assets/` returned the app shell marked immutable for a year). **clean** — `…FallbackServesOnlyEmbeddedAssets`. | frame the signed-in UI; MIME-sniff the shell; poison a shared cache at an asset URL; serve something outside the embedded FS |
| 17 | Client local state + op log (`internal/store`, `internal/config`, `internal/journal`) | **NEW in r4.** **fixed** — `TestSec_Config_FolderConfigCannotRedirectTheDeviceToken` (a folder's `.bdrive/config.json` chose where this device's hub token was sent, `http://` included, and `bdrive login <other-hub>` shipped the new token to every old mount's host; the credential is now bound to `settings.Server`'s origin), `TestSec_Config_MountIdCannotEscapeTheBdriveHome` + `TestSec_Store_MountIdCannotEscapeTheVolumeDir` (`Project.ID` from that same untrusted file was joined onto `$BDRIVE_HOME` and onto `state-<id>.json`; validated where it is read), `TestSec_Store_VolumeJournalsAreNotWorldReadable` (journals, `state-*.json` and `sync.json` were 0644 inside a 0755 `$BDRIVE_HOME` — every local account could read a private project's path list, authorship and signed-in emails), `TestSec_Journal_TornTailDoesNotVoidTheWholeJournal` + `…OneUnreadableLineCannotVoidTheOpsBeforeIt` (`Append` is the one non-atomic state write and `Parse` was all-or-nothing, so one torn or planted line made every op the device ever committed unreadable with no recovery path), `TestSec_Journal_PathSurvivesTheWireFormatByteExact` (`encoding/json` rewrote invalid UTF-8 to U+FFFD, so two distinct legal unix filenames collapsed to one path on every peer and one file silently overwrote the other), `TestSec_Journal_ReplayIsDeterministicUnderInputPermutation` (`Less` was not a total order, so the stated determinism invariant was carried by `Store.AllOps`'s incidental ordering rather than by `Less`). **clean** — `TestSec_Store_AtomicWriteDoesNotFollowASymlinkAtTheDestination`, `TestSec_Config_SettingsFileHoldingTheTokenIsNotWorldReadable`, `…TokenNeverReachesAnErrorMessage`. | anything that reaches a path or a credential from a file that travels with a folder; permissions on the client's own state; a journal that cannot be parsed or replayed the same way twice |
| 18 | Project archive (`cmd/bdrive/migrate.go`) | **NEW in r4.** **fixed** — `TestSec_Migrate_ExportOnlyEmitsStoreKeys` (a hostile hub's object listing became tar member names verbatim, turning the archive users are told to pass around into a traversal bomb for `tar xzf`; export now applies the same key allowlist import does), `TestSec_Migrate_CorruptBlobNeverLandsInTheTargetStore` (the hash was compared after `be.Put` returned, so the object stayed under a content address promising different content — and every device that later connected failed its pull forever). **clean** — `TestSec_Migrate_ArchiveEntryCannotEscapeTheStorePrefix` (14 subtests: every classic tar trick, symlink/hardlink/fifo/device members, setuid modes, NUL-in-name). | a hostile archive; a hostile hub on the export side; a member that extracts outside the store layout in either direction |
| 19 | The device as client of a hostile hub (`remote/http.go`) | **NEW in r4** — the mirror of row 5, and it had no row for three rounds. **fixed** — `TestSec_HTTP_ListedKeysFromTheHubStayInTheKeySpace` (the hub names its own objects and the device believed it; those names become local journal file paths and tar member names), `TestSec_HTTP_BearerTokenIsNeverSentToAnotherOrigin` (net/http only strips `Authorization` when the HOSTNAME changes, so a hub's 302 handed the device token to another port, an https→http downgrade, or a sibling subdomain). **clean** — `TestSec_HTTP_UnverifiableTLSIsRefused`. **fixed** — `TestSec_Sign_DeclaredSizeIsBoundIntoTheSignature/gcs` (`gcsBackend.SignPut` discarded its `size`, so a GCS hub handed out a 15-minute unmetered write grant; `Content-Length` is now in the signature, verified present in `X-Goog-SignedHeaders`). **clean** — the same test's `s3` arm. | everything the hub says: object keys, redirects, presigned URLs, sizes, TLS |

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

### Loop status after round 4 — NOT done

Both conditions are stated in "What counts as done". Neither is met.

1. **Every row `clean` or `fixed`, backed by a named test** — *not met*. Row 14
   is not closed: `TestSec_DB_NULBytesDoNotTruncateRecords/postgres` still
   fails on a real Postgres and I did not weaken it. Rows 2 and 1 also carry
   named, still-open items (`Dir == nil → PermAdmin`; no expiry). Rows 5 and 6
   are closed against everything tested, but the browser presign commit flow
   in row 6 has no test at all.
2. **Two consecutive dry hacker rounds** — *not met*. Round 4 produced ~34
   failing tests across 32 holes, so it was not dry. The counter is back at
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

New from round 4 — consequences of this round's own fixes, named on purpose:

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
- `secsignHub(t)` (`sec_sign_test.go`, new in r4) — a hub whose storage CAN
  presign, plus a recorder of every key/size/TTL the signer was asked for.
  This is the fixture three rounds said they needed and skipped; use it for
  the browser commit flow next.
- outside `internal/webapp`: `sharedRemote`/`newDevice`/`cycle`/`prune`/
  `hubState` (`internal/syncer`) for multi-device attacks, and the r4 files
  `internal/{store,config,journal,remote}/sec_*_test.go` +
  `cmd/bdrive/sec_migrate_test.go` for the client side.

Rules that keep four attackers from colliding in one package:

- **Never edit an existing test file.** Add your own new file only.
- Every helper you add is prefixed with your file's slug (`gateDo`, `permsDo`)
  so two files can't declare the same name.
- Reuse the helpers above by calling them; do not copy them.

## Coverage gaps after round 4 — the next round's targets

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
