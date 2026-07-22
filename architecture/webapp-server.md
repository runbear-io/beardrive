# `bdrive web` server — class diagram

Source of truth: `internal/webapp` (server, services, persistence) and
`internal/remote` (storage backends). Reflects the code as of this commit;
update this file in any PR that changes these types or their relationships.

## Server core, sources, and services

```mermaid
classDiagram
    direction LR

    class Server {
        +Source Source
        +Volume string
        +Root remote.Backend
        +Projects *ProjectDB
        +Device Identity
        +Refresh time.Duration
        +Upload UploadConfig
        +Auth AuthProvider
        +Devices *DeviceRegistry
        +Shares *ShareDB
        +Reads *ReadLedger
        +Dir Directory
        +Quota QuotaProvider
        +ShareRPM int
        -vols per-project volume cache
        +Handler() http.Handler
    }

    class volume {
        -source Source
        -refresh time.Duration
        -snap *snapshot
        +snapshot(ctx)
        +invalidate()
    }

    class Source {
        <<interface>>
        +Files(ctx) map path→FileInfo
        +Open(ctx, path, fi) io.ReadCloser
    }
    class DirSource {
        +Dir string
    }
    class RemoteSource {
        +Backend remote.Backend
        +Device Identity
    }
    class Uploader {
        <<interface>>
        +Upload(ctx, path, r, size, who)
    }
    class DirectUploader {
        <<interface>>
        +SignBlobPut(ctx, blob, size, ttl)
        +HasBlob(ctx, blob)
        +Commit(ctx, path, blob, size, who)
    }

    class Backend {
        <<interface>>
        +Put +Get +List +Exists +Close
    }
    class PutSigner {
        <<interface>>
        +SignPut(ctx, key, size, ttl)
    }
    note for Backend "internal/remote — impls: localBackend (file://), s3Backend, gcsBackend, httpBackend (https:// hub), Prefixed wrapper"

    class AuthProvider {
        <<interface>>
        +CLILoginPath()
        +Authenticate(r) User
        +Register(mux)
        +Accounts() []User
    }
    class AccountApprover {
        <<interface>>
        +PendingUsers() +Approve +Deny +SetPolicy +Policy
    }
    class BuiltinAuth {
        +AllowSignup bool
        +AllowedDomains
        +RequireVerification bool
        +RequireApproval bool
        +Admins
        +InviteValid func(token)
        -store AccountRepo
        -users, tokens, pending
    }
    class Mailer
    class User {
        +ID +Email +Name +Admin
    }

    class Directory {
        <<interface>>
        +Role(org, email)
        +Get +OrgsFor +ListInvites +ValidInvite +ManageURL
        +Create +Rename +AddMember +SetRole +RemoveMember
        +CreateInvite +RevokeInvite +Redeem
    }
    class LocalDirectory {
        +ManageURL(orgID)
    }
    class OrgDB {
        -repo OrgRepo
        -byID, invites
    }
    class Org {
        +ID +Name +Members email→role +Created
    }
    class OrgInvite {
        +Token +Org +Creator +Expires +Uses
    }

    class ProjectDB {
        -repo ProjectRepo
        -byID
        +Get +Create +Rename +List
    }
    class Project {
        +ID +Name +Org +Created
    }

    class ShareDB {
        -repo ShareRepo
        -byToken
        +Create +Get +Revoke
    }
    class Share {
        +Token +Project +Path +Creator +Expires
    }

    class DeviceRegistry {
        -repo DeviceRepo
        -byID
        +Observe(DeviceInfo)
    }
    class DeviceInfo {
        +ID +Name +OS +User +IP +LastSeen
    }

    class ReadLedger {
        -repo ReadRepo
        -retention
        -byKey, dirty, seen
        +Record(...)
        +Heat(project, prefix, days)
    }
    class ReadStat {
        +Project +Path +Day +Kind +Actor +Count +Last
    }
    class HeatEntry {
        +Human +Agent +Share +Readers +LastRead
    }

    class QuotaProvider {
        <<interface>>
        +CheckWrite(org, bytes)
        +CheckSeat(org, members)
        +RecordUsage(org, bytes)
    }
    class UnlimitedQuota

    Server o-- "0..1" Source : single-volume mode
    Server o-- "0..1" Backend : Root (hub mode)
    Server o-- ProjectDB
    Server o-- AuthProvider
    Server o-- Directory
    Server o-- DeviceRegistry
    Server o-- ShareDB
    Server o-- ReadLedger
    Server o-- QuotaProvider
    Server *-- volume : per project, cached
    volume o-- Source

    Source <|.. DirSource
    Source <|.. RemoteSource
    Uploader <|-- DirectUploader
    DirectUploader <|.. RemoteSource
    RemoteSource o-- Backend : Prefixed(Root, projectID)
    Backend <|-- PutSigner : optional capability

    AuthProvider <|.. BuiltinAuth
    AccountApprover <|.. BuiltinAuth
    BuiltinAuth o-- Mailer : nil → log links
    AuthProvider ..> User

    Directory <|.. LocalDirectory
    LocalDirectory *-- OrgDB : embeds
    OrgDB ..> Org
    OrgDB ..> OrgInvite
    BuiltinAuth ..> OrgDB : InviteValid wiring

    ProjectDB ..> Project
    ShareDB ..> Share
    DeviceRegistry ..> DeviceInfo
    ReadLedger ..> ReadStat
    ReadLedger ..> HeatEntry
    QuotaProvider <|.. UnlimitedQuota
```

## Metadata persistence (`MetaStore`)

Service structs keep in-memory maps + logic; every change persists as one
record through a typed repo. Blobs and journals never touch this layer.

```mermaid
classDiagram
    direction LR

    class MetaStore {
        <<interface>>
        +Accounts() AccountRepo
        +Projects() ProjectRepo
        +Orgs() OrgRepo
        +Shares() ShareRepo
        +Devices() DeviceRepo
        +Reads() ReadRepo
        +Close()
    }

    class AccountRepo {
        <<interface>>
        +Load() +PutAccount +DeleteAccount +PutToken +DeleteToken +PutPolicy
    }
    class ProjectRepo {
        <<interface>>
        +Load() +Put +Delete
    }
    class OrgRepo {
        <<interface>>
        +Load() +PutOrg +DeleteOrg +PutInvite +DeleteInvite
    }
    class ShareRepo {
        <<interface>>
        +Load() +Put +Delete
    }
    class DeviceRepo {
        <<interface>>
        +Load() +Put
    }
    class ReadRepo {
        <<interface>>
        +Load() +PutBatch +DeleteBatch
    }
    note for ReadRepo "batch-oriented: one flush = one write"

    class fileMetaStore {
        JSON files, atomic rewrite per change
    }
    class sqlMetaStore {
        one database/sql impl
        sqlite (modernc) or postgres (pgx)
    }

    MetaStore <|.. fileMetaStore
    MetaStore <|.. sqlMetaStore
    MetaStore *-- AccountRepo
    MetaStore *-- ProjectRepo
    MetaStore *-- OrgRepo
    MetaStore *-- ShareRepo
    MetaStore *-- DeviceRepo
    MetaStore *-- ReadRepo

    class BuiltinAuth
    class ProjectDB
    class OrgDB
    class ShareDB
    class DeviceRegistry
    class ReadLedger

    BuiltinAuth o-- AccountRepo
    ProjectDB o-- ProjectRepo
    OrgDB o-- OrgRepo
    ShareDB o-- ShareRepo
    DeviceRegistry o-- DeviceRepo
    ReadLedger o-- ReadRepo
```
