# PRD: MCP server — the hub as a filesystem any agent can mount

Today an agent reaches a BearDrive project exactly one way: someone installs
the CLI, runs `bdrive init`, syncs the folder to a real disk, and lets
`internal/agenthooks` keep it fresh. That is the right story for Claude Code
on a laptop. It is the *only* story, and it excludes every agent that has no
laptop: Claude on the web, ChatGPT, a teammate's Cursor on a machine that will
never hold the team's files, an automation with no `$HOME`.

The hub already holds every project's content and already knows, per account,
per project, per folder, exactly what may be read and written. What it lacks
is an agent-shaped door.

This spec adds one: **a hub-hosted remote MCP server** at `/mcp` that presents
connected projects as a virtual filesystem — `read`, `grep`, `glob`, `list`,
`write`, `edit` — over OAuth, with no install and no local files. There is no
real filesystem behind it; the "files" are the journal folded into a tree and
blobs fetched on demand, which is the same thing the viewer has always shown.

Implement the phases in order; a phase is done only when every acceptance box
in its checklist is checked. Record progress and blockers in §Status.

## Decisions (settled, do not relitigate)

| Question | Answer |
|---|---|
| Where it runs | **Hub-hosted, `bdrive serve` mode only.** Streamable HTTP at `/mcp`. Zero install for the client. |
| Local `bdrive mcp` (stdio) | **Out of scope.** A laptop with the CLI already has real files and hooks; a second path to the same bytes earns nothing. Revisit only if offline agent access is asked for. |
| Multi-project surface | **One virtual root.** Every tool takes one path; the first segment is the project id. `read("/wiki/docs/spec.md")`. `grep(pattern, path="/")` searches every connected project. |
| Project selection | **At the OAuth consent screen**, as checkboxes. The grant records the set; the token is scoped to it. |
| Writes | **Full write** — `write`, `edit`, `delete`, `move` — journaled under the hub's own device with `Op.User` = the OAuth'd account, exactly like a browser upload. History shows the human. |
| `grep` | **On-demand bounded blob scan.** No index, no new persistent artifact. Hard caps on files and bytes scanned; the response says when it truncated. |
| Token class | **New: a scoped MCP grant token.** Not a device token, not a session cookie. |
| Protocol primitives | **Tools only.** No MCP resources, prompts, sampling, or elicitation in v1. |
| Transport | `github.com/modelcontextprotocol/go-sdk` — the official Go SDK. Hand-rolling JSON-RPC framing, session ids, and SSE resumption to save one dependency is not laziness, it is a second protocol implementation to keep correct. |

## The security design, in one function

Everything about this feature that can go badly wrong goes wrong in the same
place: a credential that is scoped to two projects being honored on a third.
The hub already funnels every per-project route through one resolver, so the
whole enforcement story is one more line in it.

```go
func (s *Server) projectPermOf(r *http.Request, p Project) string {
	if s.Desktop {
		return PermRead
	}
	if s.Dir == nil || s.Auth == nil {
		return PermAdmin
	}
	if p.Org == "" {
		return PermNone
	}
	base := s.projectPermFor(p, normEmail(s.requestUser(r).Email))
	return capByGrant(r, p.ID, base) // NEW: a scoped token ceilings the answer
}

// capByGrant lowers a resolved level to PermNone when the request's
// credential is an MCP grant that does not name this project. A credential
// with no grant attached (browser session, device token) is uncapped and
// returns have unchanged — this must never become a second permission model,
// only a ceiling on an existing one.
func capByGrant(r *http.Request, projectID, have string) string {
	g, ok := grantFrom(r.Context())
	if !ok || g.covers(projectID) {
		return have
	}
	return PermNone
}
```

Why here and nowhere else: `proj()` calls `requirePermOn` → `projectPermOf`
for *every* per-project route at registration time, including the ones this
feature never touches. A scoped token that leaks out of an MCP client and is
replayed against `/api/p/<other>/download` or `/api/p/<other>/store/object`
hits the same resolver and gets `PermNone`. Putting the check inside the MCP
handlers instead would secure the door and leave the windows open.

Two corollaries, both non-negotiable:

- **`GET /api/projects` filters to the grant.** A scoped token must not be
  able to enumerate the projects it was denied. Listing is not reading, but a
  project list is a map of the org.
- **An MCP grant can never exceed the account's own level.** `capByGrant`
  only lowers. Selecting a project at consent that you have `read` on gives
  the client `read`, never `write`, and a later downgrade of your membership
  downgrades every live grant with no revocation step.

## What the agent sees

```
/                          ← the grant: only the projects picked at consent
├── wiki/                  ← project id, exactly as in the URL bar
│   ├── docs/spec.md
│   └── README.md
└── infra/
    └── terraform/main.tf
```

One root, no new concepts. `list("/")` enumerates connected projects;
everything deeper is an ordinary path. Folder permissions apply underneath
without the agent learning they exist: a restricted subtree is simply not in
the listing, and reading it says *not found* — never *forbidden*, which would
confirm the file is there.

### Tools

| Tool | Shape | Backed by |
|---|---|---|
| `list` | `(path, depth=1)` → entries with size, modified, sha | `handleTree` / `Source.Files` |
| `read` | `(path, offset?, limit?)` → text, truncation flag | `handleFile` / `Source.Open` |
| `glob` | `(pattern, path?)` → matching paths, newest first | `Source.Files` + `path.Match` |
| `grep` | `(pattern, path?, glob?, ignore_case?, files_only?)` | new: bounded blob scan |
| `write` | `(path, content)` → new sha | `Uploader.Upload` |
| `edit` | `(path, old_string, new_string, replace_all?, sha)` | read + CAS + `Upload` |
| `delete` | `(path)` — a file, or a `dir/` prefix | `RemoteSource.Remove` / `appendOps` |
| `move` | `(from, to)` | `moves.go` |
| `history` | `(path, limit?)` → who changed it, when, sha | `handleHistory` |
| `restore` | `(path, sha)` | `handleRestore` |

There is no separate `tree`: `list` is one tool with a depth, `depth=1` being
`ls` and anything deeper the tree. It defaults to 1 and caps entries, because
`Source.Files()` hands back the whole project flat — a recursive listing of a
50k-file project is one cheap call on the hub and a blown context window on
the agent. Truncation is reported, never silent.

`history` and `restore` are the tools a local-filesystem MCP server cannot
offer, and they ship as a pair: an agent that can read the past but not act on
it is half a tool, and `restore` is the undo for an agent that made a mess.
Both are existing hub APIs. "Who last touched this and why, and put it back"
is two calls, not a git archaeology session.

`delete` takes a `dir/` prefix as well as a file, because agents `rm -rf`
directories and doing it as N separate calls is N chances to stop halfway.
`appendOps` already journals a batch in one append, so the whole removal lands
atomically or not at all.

`read` returns an MCP image content block for image mime types — the blob is
already right there, and an agent that syncs a folder of screenshots should be
able to look at them. Other binaries return size and a download URL, not bytes.

### What is deliberately absent

Agents reach for shell verbs. Each one below has an answer, and the answer
belongs in the tool descriptions — an agent that gets an error where it
expected a no-op will burn a turn working around a problem that does not
exist.

| Agent reaches for | Answer |
|---|---|
| `mkdir` | **Not needed, and not offered.** BearDrive has no empty directories — the journal maps path → content, so a directory exists because files are under it. `write("/wiki/new/deep/file.md")` just works. |
| `cat`, `head`, `tail`, `wc -l` | `read` with `offset`/`limit`. |
| `cp` | `read` + `write`. A server-side copy would be one op with zero bytes moved (the blob is content-addressed and already stored) — worth adding if agents actually copy often, not worth a tool on speculation. |
| `diff` | `history` + `read` at two shas. |
| `find` | `glob`. |
| `stat` | `list` already returns size, modified, and sha. |
| `touch` | Nothing to create. A file with no content is not a thing the journal represents. |
| Anything else in a shell | **There is no shell, and there will not be one.** There is no machine behind this filesystem to run commands on. |

That last row is the honest limit of this door: an agent that needs to *run*
the code it is editing still needs a synced folder and the CLI. This server is
for agents that read, search, and write — not for agents that build and test.

### `edit` is compare-and-swap, not read-modify-write

The journal is last-writer-wins per path. An `edit` that reads, substitutes,
and writes will silently discard a teammate's concurrent change — the classic
lost update, and the one failure mode that would make an agent actively
dangerous to a shared project.

So `edit` takes the sha the agent read and refuses if the file has moved on:

```
edit(path, old, new, sha="a1b2…")
  → 409 {"error": "stale", "current_sha": "c3d4…"}   # agent re-reads, retries
```

`write` is unconditional (it is a declared overwrite), `edit` is not. The sha
comes back from `read` and `list`, so the agent never has to ask for it.

## OAuth: what must exist for Claude and ChatGPT to connect at all

The hub already has a PKCE authorization-code flow — `/auth/cli` → one-time
code → `/api/auth/exchange` — but it is BearDrive-shaped: the client is
`bdrive`, the redirect is a loopback port, and the token is an unscoped device
credential. A remote MCP client is none of those things. It has never heard of
this hub, has no client id, and will refuse to proceed without the discovery
documents.

New endpoints, all standard, none optional:

| Endpoint | Spec | Why it cannot be skipped |
|---|---|---|
| `GET /.well-known/oauth-protected-resource` | RFC 9728 | How the client learns `/mcp` needs auth and who issues tokens for it |
| `GET /.well-known/oauth-authorization-server` | RFC 8414 | Endpoint discovery; clients will not guess paths |
| `POST /oauth/register` | RFC 7591 (DCR) | Claude/ChatGPT arrive with no client id. No DCR, no connection. |
| `GET /oauth/authorize` | OAuth 2.1 + PKCE S256 | **The project picker lives here.** |
| `POST /oauth/token` | OAuth 2.1 | Code exchange + refresh |
| `401 WWW-Authenticate: Bearer resource_metadata="…"` on `/mcp` | RFC 9728 | The unauthenticated response is what starts the whole dance |

Tokens carry the `resource` indicator (RFC 8707) and are bound to it, so an
access token minted for this hub's `/mcp` is not replayable at another
resource that trusts the same issuer.

Follow `CLIAuth`'s shape: a self-contained type wired to the provider by two
hooks (`session` — who is this browser, cookie only, never a Bearer token; and
minting). The protocol half is identical no matter where accounts live, and
the managed hub must not end up carrying a drifting copy of it. A provider
that ships its own OAuth AS can opt out by declining to register the routes;
it still has to honor the grant scope, because that lives in the resolver.

### The consent screen is the feature

This is the surface the user actually asked for, so it gets designed, not
generated:

```
Claude wants to access your BearDrive projects

  Acme Inc
    ☑ wiki            you can edit
    ☑ infra           you can edit
    ☐ finance         you can view

  Side Projects
    ☐ notes           you can edit

  Claude will be able to read and change files in the projects you
  select, as you. Folders you cannot see stay hidden.

                                   [ Cancel ]  [ Connect 2 projects ]
```

- Grouped by org, each row showing the level *you* have — the grant can never
  exceed it, and saying so here is cheaper than a support thread later.
- Nothing pre-checked. An agent silently granted every project because the
  user hit Enter is the failure this screen exists to prevent.
- Zero selected disables the button. An empty grant is not a useful connection.

### Changing the selection later

Users will add a project a week after connecting. `/settings/connections`
lists every live grant — client name, projects, created, last used — with
**Revoke** and **Change projects** (which re-runs `/oauth/authorize` with the
current set pre-checked). Revoking kills the access and refresh tokens
immediately; a client mid-session gets a 401 and re-authorizes.

## Where the existing machinery already covers us

Deliberately listed so nobody rebuilds it:

- **Folder permissions** — route the tools through `visibility(r)` /
  `writablePath` and a restricted subtree is invisible and unwritable for
  free. A hidden path 404s; never 403.
- **Quota** — `QuotaProvider.CheckWrite` / `RecordUsage` on every write, the
  same as uploads. `grep` fetches blobs from storage, which is real egress:
  `RecordEgress` on the bytes scanned.
- **Blobs before journal** — `Upload`/`Commit` already enforce it. Do not add
  a second write path that bypasses them.
- **Device binding** — does not apply. MCP writes go through the hub's own
  device via `Commit`, not `/store/*`, so `ownJournal` is not in this path and
  no device is bound.
- **Rate limiting** — `ratelimit.go`'s token bucket, per grant rather than per
  IP (an MCP client is one credential behind one NAT).

## Read heat

MCP reads are agent reads: `ReadKindAgent`, actor = the grant id.

One new invariant: **`GET /api/p/<id>/heat?by=device` must not report grant
ids.** That exception is documented as reporting *device* ids, which every
project member already sees in History. A grant id is not a device id, has
never appeared in History, and leaking it turns a privacy-reviewed exception
into a wider one by accident. Filter non-device actors out of that response.

`write`/`edit`/`delete` land in History as the human, via `Op.User` — the
`note` field carries the origin (`"mcp · Claude"`) so an admin can tell an
agent's edit from a browser upload without a second identity concept.

## Non-goals

- A local stdio server (`bdrive mcp`). Different problem, different day.
- MCP resources, prompts, sampling, elicitation. Tools only.
- Binary file editing. `read` returns text; a binary path returns a download
  URL and its size instead.
- Any CLI change. `cmd/bdrive` is untouched by this feature.
- Search ranking, semantic search, embeddings. `grep` is grep.

---

## Phase 1 — transport

Prove a real MCP client can talk to the hub before building an auth server for it.

- [ ] `github.com/modelcontextprotocol/go-sdk` added; `/mcp` served from
      `internal/webapp/mcp.go`, hub mode only (single-volume and desktop modes
      return 404).
- [ ] Config gate: `mcp: {enabled: bool}` in the server config, default off
      until Phase 2 lands. An MCP endpoint with no OAuth in front of it must
      not be reachable on a real hub even for a release.
- [ ] One tool, `list`, against a grant faked in test wiring.
- [ ] Connects from a real client (Claude Desktop custom connector) end to
      end; transcript in the PR.
- [ ] `initialize` advertises tools only.

## Phase 2 — OAuth and the scoped grant

- [ ] `Grant` type + repo behind `MetaStore` (`db.go`), both backends
      (`db_file.go`, `db_sql.go`), covered by `db_conformance_test.go`.
      Fields: id, account, client id/name, project ids, created, last used,
      expiry, refresh token digest. **Digests, never plaintext** — same rule
      as device tokens.
- [ ] Discovery documents, DCR, `/oauth/authorize`, `/oauth/token`, refresh.
- [ ] `401` on `/mcp` carries `WWW-Authenticate` with `resource_metadata`.
- [ ] Consent screen with the project picker, per the sketch above.
- [ ] `capByGrant` in `projectPermOf`; `/api/projects` filtered to the grant.
- [ ] `sec_mcp_test.go`: the authz matrix. A grant for project A, replayed
      against **every** registered per-project route of project B, is denied.
      Table-driven over the route list `recordingMux` already collects, so a
      route added later without thinking is a failing test, not a hole.
- [ ] A grant cannot exceed its account's level; an account downgraded to
      `read` immediately downgrades its live grants.
- [ ] Revocation kills access and refresh tokens at once.

## Phase 3 — read tools

- [ ] `list`, `read`, `glob`, `history` over the virtual root, honoring
      `pathFilter`. `read` emits an image content block for image mime types.
- [ ] `grep`: bounded scan with an LRU blob cache, caps on files scanned,
      bytes scanned, and line length (reuse `cmd/bdrive/grep.go`'s
      `binarySniff` / `maxLineScan` constants — same semantics, same answers).
- [ ] Truncation is reported in the result, never silent. An agent that thinks
      it searched everything and did not is worse than one that knows it
      stopped.
- [ ] `# ponytail:` comment naming the ceiling and the index upgrade path.
- [ ] Hidden paths 404; a multi-project `grep` never crosses into a project
      outside the grant.
- [ ] `RecordEgress` on bytes scanned.

## Phase 4 — write tools

- [ ] `write`, `edit` (CAS on sha, 409 + `current_sha` when stale), `delete`
      (file or `dir/` prefix, one atomic `appendOps`), `move`, `restore`.
- [ ] Ops carry `Op.User` = the OAuth'd account and a `note` naming the
      client. Verified in History.
- [ ] `writablePath` on every write; a read-only folder refuses.
- [ ] `CheckWrite` / `RecordUsage` on every write.
- [ ] Multi-device test in `internal/syncer`: hub-side MCP write → a real
      device pulls it → converges. A write path the sync engine has not
      actually seen is untested where it matters.

## Phase 5 — management and telemetry

- [ ] `/settings/connections`: list, revoke, change projects.
- [ ] Reads recorded as `ReadKindAgent`; `?by=device` excludes grant ids,
      with a test that fails if a grant id ever appears there.
- [ ] Per-grant rate limit.

## Phase 6 — docs

- [ ] `web/docs`: a `Working with agents` page for connecting an MCP client,
      listed in `astro.config.mjs` with a `description`.
- [ ] `reference/hub-config.md`: the `mcp` block.
- [ ] `README.md` + `INSTALL_FOR_AGENTS.md`: the no-install path exists and
      when to prefer it over `bdrive init`.
- [ ] `architecture/webapp-server.md` updated; PR carries the consolidated
      diff diagram per the repo's diagram rules.

---

## Risks

| Risk | Mitigation |
|---|---|
| A scoped token honored on a route nobody remembered | The check is in `projectPermOf`, not in the MCP handlers; the matrix test is table-driven over every registered route. |
| `grep` melts a big project | Hard caps, LRU cache, reported truncation, egress accounting. The ceiling is documented, not discovered in production. |
| Agent stomps a teammate's edit | `edit` is CAS; `write` is explicit. |
| Consent fatigue → user grants everything | Nothing pre-checked, levels shown inline, projects listed by org. |
| MCP auth spec moves under us | Discovery + DCR + PKCE + resource indicators is the stable core; keep the OAuth code in one file so a spec bump is one diff. |
| OSS ships an AS a managed hub must replace | Mirror `CLIAuth`: protocol in one provider-agnostic type, two hooks. A provider with its own AS declines the routes and still inherits the grant ceiling. |

## Open questions

- **Token lifetime.** Proposal: access 1h, refresh 30d rolling, grant expires
  after 90d unused. Needs a number, not a shrug.
- **Does a grant survive a project rename?** It stores project ids, and ids
  are stable, so yes — confirm against `ProjectDB`.
- **Org-wide grants?** "All projects in Acme, including ones created later" is
  the obvious v2 ask and the obvious v2 footgun. Not in v1.
- **Should `list("/")` show projects the account can see but did not select?**
  Proposal: no. Showing a locked door is an invitation to rattle it.

## Status

Implemented on branch `homeless-bird`. Full `go test ./...` green (webapp 410s), 47 MCP-specific tests, five rounds of black-box validation by agents driving the real protocol (~1,700 calls), the last returning a ship verdict with no blocking defect.

| Phase | State |
|---|---|
| 1 — transport | **Done.** `internal/webapp/mcp.go`, official Go SDK v1.8.0, streamable HTTP, stateless + JSON responses. Hub mode only; `mcp: {enabled}` config, off by default. |
| 2 — OAuth + scoped grant | **Done.** `internal/webapp/mcpauth.go`: RFC 9728 + 8414 discovery, RFC 7591 DCR, authorize/token with PKCE S256, refresh rotation. `MCPRepo` on `MetaStore` with file + SQL backends and conformance coverage. |
| 3 — read tools | **Done.** `list`, `read`, `glob`, `grep`, `history`. Bounded scan, reported truncation. |
| 4 — write tools | **Done.** `write`, `edit` (CAS on sha), `delete` (file or `dir/`), `move`, `restore`. |
| 5 — management + telemetry | **Done.** `GET/DELETE /api/mcp/grants` plus a `bdrive mcp list/revoke` CLI, both tested. Plus the Connected agents page at `/connections`. Reads land as agent traffic. |
| 6 — docs | **Done.** README, `reference/hub-config.md`, and `guides/mcp.md` (sidebar-listed). |

### What the implementation decided that the spec did not

- **Tools are internal clients of the hub's own HTTP handlers.** A write is a
  PUT to `/upload/content`, a delete a POST to `/remove`. That single choice is
  why folder permissions, quota, journaling, cache invalidation and live
  change fan-out all apply here with no second copy. Only `list`/`read`/
  `glob`/`grep` are native, because they have no HTTP equivalent.
- **The grant ceiling is applied outside the resolver, not inside one branch
  of it.** `projectPermOf` early-returns `PermAdmin` when a hub has no
  directory; capping only the common branch would have left an MCP token
  unscoped on exactly those hubs. `capByGrant` now wraps the whole resolution.
- **A project outside the grant reads as absent, never as forbidden.** The
  hub's own 403 text ("you have read-only access to this project") both
  confirms the project exists and reports a membership the caller did not ask
  about. `splitGranted` is the one guard for every HTTP-routed tool.
- **MCP reads are agent reads, and the grant id is filtered out of
  `?by=device`.** `recordRead` files everything as human; an MCP read using it
  would have conflated "a person opened this" with "an agent scanned this",
  which is the exact distinction the heat map exists to draw.

### Known gaps

- ~~No connections page in the web UI.~~ **Shipped.** Account menu →
  Connected agents (`/connections`), an account-level route beside `/orgs` and
  `/billing` — a grant spans projects, so it has no project to live under.
  `bdrive mcp list` / `revoke` and the API do the same from a terminal.
- **No "change projects" flow.** Re-consenting mints a second grant rather
  than editing the first.
- **`move` is write-then-delete**, capped at 64 MB, not an atomic rename. It
  now refuses a same-path move and refuses to clobber the destination, which
  is what made the non-atomicity dangerous.
- **Schema-validation errors leak SDK internals** (`<invalid reflect.Value>
  has type "null"`) when a caller passes a null where a string belongs. Ugly,
  not misleading; the SDK owns that message.
- The Windows build still does not compile (pre-existing, unrelated).

### Fixed after black-box validation

A subagent drove ~140 tool calls through the real protocol as an agent would.
The authorization surface held completely — 13 tools × unauthorized targets,
14 traversal payloads, cross-account and cross-token scoping, all refused with
a non-committal `no such project`, no existence oracle. Everything below is a
*correctness or ergonomics* bug it found, all now fixed and regression-tested:

| Was | Why it mattered |
|---|---|
| `move X → X` deleted the file and reported success | Write-then-delete on one path. "Rename these to our convention" destroys every file already correctly named — and the tool says it moved them. |
| `move` onto an existing path destroyed the destination | `write` is documented as overwrite; `move` is not. |
| `restore` demanded a 64-char sha; `history` only ever printed 12 | The recovery tool was unreachable from the only tool that names a version. Now resolves a prefix against that file's own history. |
| `write` had no compare-and-swap | `edit` was guarded, but "rewrite this file" must use `write` — so the whole class of full-file updates had no lost-update defence. |
| `read` had no raw mode | The decorated read is lossy: `"a\nb"` and `"a\nb\n"` rendered identically, so a copy invented a trailing newline. |
| `read` truncated mid-line with no resume point | The last line was a fragment the agent could neither use nor resume from. Now cuts on a line boundary and prints the offset. |
| A malformed `glob` returned "no files match" | A false negative an agent acts on. `grep` already errored; `glob` now does too. |
| `write` silently accepted a trailing slash | Left a file and a folder sharing one name, which `delete`'s slash convention then could not tell apart. |
| `list` stamps had no timezone; delete rows printed a bare `sha:` | An agent relaying a `list` time reports the wrong day. |
| `delete` on a folder said "no such file" | Directly contradicted the listing the agent had just seen. |
| Every tool advertised an output schema and returned `{}` | `Out` was `struct{}`, which the SDK infers a schema from; a client preferring structured output read every result as empty. `Out` is now `any`. |
| `/oauth/register` wrote its status twice | `superfluous response.WriteHeader` on every registration. |

### Fixed after validation round 2

Round 2 re-tested round 1's fixes (all held) and went after the paths those
fixes opened up. Six more, all now fixed and regression-tested:

| Was | Why it mattered |
|---|---|
| `read` paged a 1 MiB **prefix** and reported the prefix's line count as the file's | The worst bug this door has had. `grep` would quote line 28500 of a file `read` called 11072 lines long, and "offset is past the end" is indistinguishable from real EOF — so "summarize this export" drew conclusions from the first 40% and reported success. `read` now streams the whole file: any offset is reachable and the count is true. |
| `**` in a glob matched exactly one directory level | `glob("tmp/**/*.log")` returned 1 of 7 files, the agent deleted it and reported the cleanup done. A plausible short answer is worse than an error. Replaced `path.Match` with a real segment matcher where `**` spans any depth. |
| Glob patterns matched the project-relative path, ignoring `path` | The tool's own error text promised "patterns are relative to the search path" while the engine did the opposite. |
| `grep`'s `glob` filter had none of `glob`'s validation | A malformed filter searched zero files and said "No matches" — an agent concludes the symbol is unused. |
| Compare-and-swap was check-then-write with no lock | Eight concurrent writers passing one sha were all accepted, seven updates lost. Now a per-path mutex spans the check and the write. |
| `move` refused a file collision but shadowed a folder | `move notes.md docs` means "into docs/" to whoever typed it; it created a junk file called `docs`. |

Plus: `read` on a folder said "no such file"; an empty file read as "1 lines";
`read` called a file an image on its extension alone (a text file named
`fake.png` was unreadable by any means); `grep` skipped binary files silently;
echoed patterns were re-escaped, so copying one into a retry searched for a
literal backslash; deleting a whole project gave a generic syntax hint.

Round 2 again found **no authorization defects**: every tool returns a
byte-identical `no such project` for an ungranted project and a nonexistent
one, traversal is refused, and cross-project `move` refuses without confirming
the target exists.

### Beyond the spec: paths name projects by name

Both validation rounds independently complained that every path carried 36
characters of UUID — unreadable in a transcript, easy to transpose between two
projects, and carried forward into every subsequent call. The first path
segment now accepts **either** the project's name or its id, and output is
formatted with the name when it is unambiguous:

```
/mynotes/a/b.md:1:by name        instead of   /8c088329-e809-.../a/b.md:1:by name
```

Ids win over names, so a name can never shadow a real id; a name shared by two
connected projects falls back to ids rather than guessing; and an unknown name
is refused with the same `no such project` an unknown id gets, so this adds no
existence oracle.

### Fixed after validation round 3

Round 3 ran ~700 calls and went after the surfaces the earlier fixes created.
The compare-and-swap held under 500 concurrent calls (exactly one winner,
five rounds), copy fidelity was byte-exact on 9/9 pathological encodings, and
there was still no existence oracle. Twelve more defects, all fixed:

| Was | Why it mattered |
|---|---|
| The root listing printed a **name that addressed a different project** | A project whose name is another project's id is unaddressable by name (ids win on resolution) — and the listing advertised that name as its path. An agent following the row it was handed wrote into a different project, and the write said success. The label now has to resolve back to its own row. |
| `write` accepted a **file/folder collision** in both directions | These paths sync to real disks, where `src` the file and `src/` the folder cannot coexist. An agent could leave a project no teammate's device can materialize, with MCP reporting green — and the only escape hatch told them to delete the whole subtree. |
| `grep` answered "No matches" for files it **could not read** | A line over the scan buffer ended the scan and returned "not found". "Find every reference to the old key and remove it" came back done with the key still in a minified bundle. It now names every file it could not search. |
| `grep`'s binary-skip notice was omitted on the **no-match** answer | Exactly when it decides whether "not found" is true. "No matches in 1 file(s) searched" for a file that was skipped claims a search that never happened. |
| `grep`'s header reported the **display cap as the total** | "How many occurrences?" answered 200 for a file with 40,000. |
| Brace globs (`**/*.{go,md}`) matched nothing, silently | A top-three agent idiom failing clean. Now an error naming the workaround. |
| `read` had a line cap but **no byte cap** | Five 400k-character lines returned a 2 MB response with no truncation notice. |
| `raw` said "read it in pages" for files paging cannot handle | Three calls to discover there was no path at all. The message now distinguishes the two cases. |
| `move` accepted a trailing-slash destination `write` refused | `move X /docs/` silently created a *file* named `docs`. |
| An empty project read as `Nothing at X` | Identical in shape to a typo — on the first call an agent makes in a new project. |
| `edit` on a folder said "no such file"; a no-op `edit` burned a version | The second invalidated every teammate's held sha for a change that changed nothing. |
| `restore` on a never-existing file sent the agent to `history` | Which would be empty. One guaranteed-wasted call. |

`grep` also gained **context lines** (`context: N`), the affordance every round
named as the biggest missing one: "find the panic and show me around it" was
two calls and hand-done offset arithmetic, and is now one.

### Fixed after validation round 4

Round 4 confirmed round 3's fixes held and found the remaining defects were
concentrated in **one seam**: the per-line and per-byte caps, and the advice
written around them.

| Was | Why it mattered |
|---|---|
| One over-long line made a whole file unreachable by **every** tool, and the errors pointed at each other | A 4,002-line log with one dumped payload: paged `read` refused it, `raw` refused it, `grep` called it unsearchable — and `read`→`raw`→`read` looped forever. The root cause was `bufio.Scanner`, which fails a file for one long token. Replaced with a reader that **truncates the line and says so**: a partial answer about line 2,002 beats a confident "no matches" about the file. |
| `grep context:N` duplicated lines in overlapping windows and could emit them out of order | Anything reconstructing a file view from grep output got a corrupted, longer-than-real file. |
| `grep`/`glob` on a **nonexistent folder** answered "no matches" | A typo'd directory read exactly like a clean negative — the same silent-wrong-answer family as a skipped file. `list` already knew; now these do too. |
| Inspecting a past version required restoring it | Two writes and two new versions just to look. `read` now takes a `sha`. |
| A malformed `sha` was reported as "stale" | The agent re-read, retried, and looped — the one error shape that cannot converge. |
| A 300-byte filename was accepted | A file that can never materialize on a synced device, in a product whose job is materializing files on devices. |
| The root listing did not mark read-only projects | The agent learned it from a refusal, after composing the edit. |

Three messages also advised "download it from the viewer" while no tool ever
emits a URL — unfollowable advice, now replaced with something the agent can
actually do.

### Validation round 5: ship

Round 5 was run as a ship/don't-ship judgment, with the ten use cases scored on
"could you complete this correctly **without being misled**" rather than "did
it work" — the right question, because every serious defect in this feature has
been one that looked like success.

**Verdict: ship. No blocking defect.** All ten tasks completed correctly. The
authorization model was attacked again and held: a live permission downgrade
and a full revocation both take effect immediately on an already-minted token,
consent silently drops a project the account cannot see, folder rules are
enforced through every tool (`read` 404s rather than 403s, no per-file oracle
inside a restricted prefix), and unconnected vs nonexistent is byte-identical
across all ten tools by id and by name.

Everything remaining was message accuracy, and all of it is fixed:

| Was | Why it mattered |
|---|---|
| A **folder** rule refusing a write said "you have read-only access to this project" | False when the caller has write on the project — it sends them to fix permissions they already hold. Three contradictory facts in three consecutive calls. |
| A well-formed sha naming **no version** was reported as "stale" | "Stale" asserts the file changed and says to re-read and retry — which reads, gets the same sha, and fails identically forever. `restore` already discriminated; `write`/`edit` now ask the same question before the accusation. |
| `grep` clipped a long line at 400 chars, **eliding the matched token** | A real hit came back looking like a false positive, with no notice that anything was cut. The window now centres on the match and marks both elisions. |
| `history` answered "No history for X" for a folder and for a typo | Reads as a factual claim that the files are unversioned. |
| `edit` diagnosed `old_string` **before** checking write permission | A read-only agent was coached to "pass replace_all", acted on it, and only then learned it could not write. |
| An uppercase sha was reported as naming no version | It is a spelling problem, not a missing one. |

### A pre-existing defect this work uncovered

**Renaming a file twice destroys its version history**, and `restore` cannot
reach the lost versions. This is *not* an MCP bug — it reproduces with no MCP
involvement, driving only the hub's own upload/remove API, which is the same
journal shape a synced device produces for a rename:

```
3 writes to m1.md      -> history: 3 rows
rename m1.md -> m2.md  -> history: 4 rows (3 + a duplicate of the newest)
rename m2.md -> m3.md  -> history: 1 row   <- the first three are unreachable
```

`buildMoveIndex` pairs a create with a delete by `(device, blob)`. After a
second rename of unchanged content there are two creates and two deletes
sharing that key, the one-to-one check cannot choose, and the chain is dropped
entirely. Pairing by time proximity would fix it; that is core history logic
with its own performance constraints and does not belong in this change.

Repro kept as a skipped test: `TestMoveHistoryChainSurvivesTwoRenames` in
`internal/webapp/moves_test.go`. **Worth fixing on its own branch** — it means
any user who renames a file twice, from any client, loses its history.

### Verification

- `internal/webapp/mcp_test.go` — the real protocol, SDK client against an
  httptest hub: OAuth dance, tool listing, filesystem round trip, CAS, restore.
- `internal/webapp/sec_mcp_test.go` — the authorization matrix, table-driven
  over every registered per-project route. Mutation-checked: disabling
  `capByGrant` fails it in 7 places.
- `internal/webapp/db_conformance_test.go` — grants and clients across both
  metadata backends, including that a revoked grant stays revoked.
