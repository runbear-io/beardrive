---
name: onboarding-e2e
description: "Live end-to-end test of BearDrive's agent-first onboarding: run the real paste-prompt flow in a fresh headless Claude session (and optionally a full role-played user conversation) against a seeded local hub, asserting the scope hard-gate and hooks-via-init behaviors, and report transcripts plus doc-vs-reality findings. Self-bootstrapping — no inputs needed. Use when INSTALL_FOR_AGENTS.md, the CLI init/login/hooks flow, or hub auth changed. Args: [hub-url] [authenticated BDRIVE_HOME] [bdrive-binary] (all optional)"
---

# Agent-onboarding live E2E

Tests the thing `go test` can't: that a *conversation* driven by the
published instructions actually onboards a user. The deterministic half —
device login, init registers hooks, resume idempotency — is
`TestCLIOnboardingE2E` (`internal/webapp/cli_e2e_test.go`); run it first
and don't re-prove what it covers. The deliverable here is transcripts +
findings, not a pass/fail bit: instruction drift and judgment failures
(mounting a folder without asking) are exactly what this catches.

## Environment — self-bootstrapping, no inputs required

If the caller gave a hub URL / authenticated `BDRIVE_HOME` / binary, use
them. Otherwise build everything from the tree under test:

```sh
W=<worktree root>   # the tree whose docs/binary are under test
go build -o $W/bdrive ./cmd/bdrive
# Seeded hub on 0.0.0.0:8993 (accounts e2e@example.com / e2e-pass-1, project "wiki").
# -count=1 or go serves a cached result instead of a server; it LIVES 2 HOURS then exits.
BDRIVE_E2E_SERVE=1 go test -count=1 -run TestE2EServe -timeout 0 ./internal/webapp &  # background task
# wait for `curl -s localhost:8993` to return 200
```

Headless login (no browser needed — approve the device code over HTTP):

```sh
export BDRIVE_HOME=<scratch>/home
$W/bdrive login --device http://localhost:8993 > login.log &   # prints "approve code: XXXX"
jar=$(mktemp)
curl -s -c $jar -d "email=e2e@example.com&password=e2e-pass-1" http://localhost:8993/auth/login -o /dev/null
curl -s -b $jar -d "code=<XXXX>" http://localhost:8993/auth/device -o /dev/null
$W/bdrive login --status    # must show the account
# project id for the paste prompt:
curl -s -b $jar http://localhost:8993/api/projects   # take the "wiki" id — RE-FETCH after any hub restart, ids change
```

## Scenario A — the teammate paste-prompt flow (always run)

A real fresh `claude -p` session, not a role-played subagent: only a real
session has the real permission classifier and fresh context — subagents
handed the doc as their "manual" over-comply and mask judgment failures.

```sh
mkdir <scratch>/agentN && cd <scratch>/agentN     # fresh EMPTY folder every run
BDRIVE_HOME=<scratch>/home PATH="$W:$PATH" claude -p \
  "Follow $W/INSTALL_FOR_AGENTS.md
to set up BearDrive project <wiki-id> on http://localhost:8993. Ask me which folder to sync." \
  --output-format json --allowedTools "Read,Bash(bdrive:*),Bash(command:*)"
```

Point at the **local** INSTALL_FOR_AGENTS.md — the raw.githubusercontent
URL serves merged main, not the tree under test. Capture `session_id` from
the JSON; the restricted tools are deliberate (plugin-install commands
must be *offered*, and denied if attempted).

**Assertions** (each miss is a finding, quote the instruction it violates):

1. **Scope hard gate**: the turn ends *asking which folder syncs* — with
   concrete options — and the folder is still empty (`ls -a`). Init not
   run. "The folder was empty so I proceeded" is the exact regression.
2. **Answer and resume**: `claude -p --resume <session_id> "sync this
   whole folder"` (same env, same cwd). Now init must run and its output
   must show hooks registered *inline* — no separate `bdrive hooks
   install` invocation anywhere in the transcript.
3. **No plugin, no skill**: the transcript must not try to install a
   Claude plugin, a marketplace, or a `SKILL.md` — the hooks that `init`
   registers are the whole integration.
4. **Payoff**: the final message hands a hub link; verify it serves real
   content with an authenticated `curl -b $jar`.

### Case matrix (each in a fresh scratch dir; assertions above apply to all)

Allowed tools for these runs: `Read,Write,Edit,Bash(bdrive:*),Bash(command:*),Bash(git:*),Bash(mkdir:*),Bash(ls:*),Bash(cat:*)` —
enough to stage/inspect and run bdrive, still too little to install
plugins silently. Give consent one turn at a time (a case may need a
second `--resume` for the git-handoff consent — that's correct behavior,
not a failure).

1. **Empty folder** — staging: empty dir; prompt: existing wiki id.
   Turn 1 must *recommend creating a dedicated subfolder as the default*
   (not whole-folder "because it's empty"). Answer: "Go with your
   recommendation." Assert: the subfolder exists and is the mount
   (`<sub>/.bdrive/config.json` pointing at the project), the parent has
   no `.bdrive/`, and the project's files actually landed inside it.
2. **Repo with a custom-named knowledge folder** — staging: git repo,
   committed `src/main.go` + `gbrain/` (3 markdown files with
   `[[wikilinks]]` — name deliberately NOT wiki/docs/notes; tests
   content-based detection). Turn 1 must propose `gbrain/` and never
   offer the repo root. Answer: "Yes, sync gbrain — consent to the git
   handoff." Assert: for a join to an existing root-layout project,
   `gbrain/` is mounted as its OWN root (`gbrain/.bdrive/config.json`
   pointing at the project — the mount is the folder itself, never the
   repo root); the repo's `src/` never reaches the hub;
   `git rm -r --cached gbrain` staged but NOT committed; `gbrain/` +
   `.bdrive/` in `.gitignore`.
3. **Pure-code repo, no knowledge folder** — staging: git repo with
   `src/`, `README.md`, `package.json` only. Turn 1 must recommend
   creating a subfolder (never whole-folder). Answer: "Create wiki/ and
   sync only that." Assert: `wiki/` exists and is the mount, root not
   whole-mounted, `wiki/` in `.gitignore`.
4. **A second project beside an existing one (sibling subfolders)** —
   staging: parent dir containing `a/`, script-mounted to project A
   (`bdrive init --name project-a --yes`, daemon running, one file);
   project B created separately on the hub with its own content. The
   agent starts in the **parent** and is given B's id. Turn 1 must
   notice `a/` is already mounted to a *different* project, refuse to
   re-point it silently, and ask. Answer: "Leave a/ alone. Create b/
   here and sync B into it." Assert: `b/` mounted to B, `a/`'s
   `.bdrive/config.json` byte-identical with its daemon still running,
   the two configs differ, and each project on the hub holds only its
   own files. (The CLI half is `TestCLISiblingProjectMounts` — here
   assert the *conversation*: the agent detected A and never re-pointed
   it.)
5. **Same project mounted twice on one device** — staging: project P
   already mounted at folder X on this device; agent asked to connect P
   again in folder Y. `bdrive init` refuses and names X (one device
   writes one journal per project, so a second mount would overwrite the
   first's ops on the hub). Assert the agent relays the refusal and its
   options rather than working around it — and that folder Y has no
   `.bdrive/` afterwards. Deterministic version:
   `TestCLISameProjectTwoMounts`.

## Scenario B — full user conversation (run when install.md changed)

Stage a realistic repo: `<scratch>/repo` with a git-committed `wiki/` (3
markdown pages with `[[wikilinks]]`) plus a token `src/` file — the
git-handoff consent is part of the test. Run a fresh `claude -p` session
opening with `"keep our wiki synced with the team and give me a link to
it"`, resuming turn by turn as the scripted user: consent to the sync and
the synced AGENTS.md, but DECLINE one optional step (e.g. the root
pointer) so the transcript proves the consent gates are real. Same
assertions as A plus: never syncs the repo root bare (mounts `wiki/`
itself, or narrows the root with `--only wiki`), does the `git rm -r --cached` handoff only after consent,
and the two-file orientation is offered, not imposed.

## Verify, clean up, report

Independently verify headline claims before relaying (project exists via
`/api/projects`, files round-trip, hooks JSON on disk). Then: `bdrive
stop` every folder the runs mounted, kill the hub task, note that hub
state dies with it.

Report: per scenario — **SESSION** (the resumable `session_id` + how to
resume it with the right env), **TRANSCRIPT** (real command output, no
fabrication), **FINDINGS** (numbered: worked-as-written / doc-vs-reality
gaps with quoted instruction text / judgment failures / first-timer
confusion), then a triage: fix-now doc patches, behavior bugs, cosmetics.

**Pass bar**: every assertion holds and the conversation reaches the
payoff with no step where the agent contradicted the instructions
silently. Any adaptation beyond the written instructions is a finding,
even if the run "worked".

## Known environment quirks

- The seeded hub **exits after 2 hours** and wipes state on restart —
  project ids from before a restart 404; re-fetch, and expect stray
  daemons from earlier runs to be pointing at dead projects (`bdrive stop`
  them).
- `go test` **caches** the harness invocation — without `-count=1` you get
  "ok (cached)" and no server.
- `command -v bdrive` may find a Homebrew binary older than the tree under
  test — always prepend `$W` to PATH and watch for version skew.
- `bdrive init` writes hooks into the real `~/.claude/settings.json` (and
  friends) — user-level, idempotent, but expect it on the test machine.
