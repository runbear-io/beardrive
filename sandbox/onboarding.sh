#!/bin/bash
# Scenario A of the onboarding-e2e skill, run inside the test container: a real
# `claude -p` session following the LOCAL INSTALL_FOR_AGENTS.md against the
# local hub, in a $HOME that is thrown away with the container.
#
#   ./sandbox/run.sh onboarding new     # create a NEW project in an empty folder
#   ./sandbox/run.sh onboarding join    # join a project seeded by another device
#
# Needs a Claude credential in the environment — see run.sh's header. The
# mechanical assertions are checked here; the judgment calls (did it recommend
# a subfolder? did it ask before acting?) are for whoever reads the transcript,
# which is the actual deliverable.
set -u

if [ -z "${CLAUDE_CODE_OAUTH_TOKEN:-}${ANTHROPIC_API_KEY:-}" ]; then
  echo "no Claude credential in the environment — see the auth line in the banner" >&2
  exit 2
fi

# The case matrix's tool set: enough to stage, inspect and run bdrive, too
# little to install a plugin or a marketplace silently.
TOOLS='Read,Write,Edit,Bash(bdrive:*),Bash(command:*),Bash(git:*),Bash(mkdir:*),Bash(ls:*),Bash(cat:*)'
S=/work/e2e
rm -rf "$S"; mkdir -p "$S/agent1"

json() { node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>{try{console.log(eval("(d=>"+process.argv[1]+")")(JSON.parse(s))??"")}catch(e){console.log("")}})' "$1"; }
pass() { echo "  PASS  $1"; }
fail() { echo "  FAIL  $1"; FAILED=$((FAILED+1)); }
FAILED=0

MODE=${1:-new}

# The doc's default recommendation for a folder with no knowledge folder, and
# the name `bdrive init shared` gives the project it creates. In join mode this
# is also the seeded project's name, since the agent is meant to recommend a
# folder matching it.
PROJ=shared

echo "== 1. sign in this device =="
bdrive-signin >/dev/null || { echo "sign-in failed" >&2; exit 1; }

jar=$(mktemp)
curl -sf -c "$jar" -d "email=$HUB_EMAIL&password=$HUB_PASSWORD" "$HUB/auth/login" -o /dev/null

if [ "$MODE" = join ]; then
  # Seeded by a SECOND device identity: the agent must join a project that
  # already exists and that its own device has never mounted. Same device would
  # hit init's one-journal-per-project refusal, which is a different test.
  echo "== 2. seed project '$PROJ' as another device =="
  mkdir -p "$S/seed/$PROJ"
  printf '# Home\n\nStart at [[Runbook]].\n'      > "$S/seed/$PROJ/Home.md"
  printf '# Runbook\n\nDeploys go through CI.\n'  > "$S/seed/$PROJ/Runbook.md"
  BDRIVE_HOME=/data/seedhome bdrive-signin >/dev/null || exit 1
  BDRIVE_HOME=/data/seedhome bdrive init "$S/seed/$PROJ" --name "$PROJ" --yes >/dev/null || exit 1
  PID=$(curl -sf -b "$jar" "$HUB/api/projects" | json "d.projects.find(p=>p.name===\"$PROJ\").id")
  [ -n "$PID" ] || { echo "could not resolve the seeded project id" >&2; exit 1; }
  echo "  project: $PID"
  # Verbatim shape of the doc's teammate paste prompt, project name included —
  # that name is what the agent is supposed to recommend as the folder.
  ASK="to set up BearDrive project $PID on $HUB. Ask me which folder to
sync (the project is named \"$PROJ\")."
else
  echo "== 2. no seeding — the agent creates a new project =="
  ASK="on $HUB. Create a new project. Ask me which folder to sync."
fi

echo
echo "== 3. turn 1 — the paste prompt, in an empty folder =="
cd "$S/agent1"
ls -a
T1=$(claude -p "Follow /src/INSTALL_FOR_AGENTS.md
$ASK" \
  --output-format json --allowedTools "$TOOLS" 2>&1)
SID=$(printf '%s' "$T1" | json 'd.session_id')
printf '%s' "$T1" | json 'd.result'
echo
echo "  --- assertions ---"
[ -n "$SID" ] && pass "session_id captured ($SID)" || fail "no session_id — turn 1 did not complete"
if [ -d "$S/agent1/.bdrive" ] || compgen -G "$S/agent1/*/.bdrive" >/dev/null; then
  fail "scope hard gate: something was mounted before the folder question was answered"
else
  pass "scope hard gate: nothing mounted yet"
fi
printf '%s' "$T1" | grep -qiE 'which folder|recommend' \
  && pass "turn 1 asks / recommends a folder" \
  || fail "turn 1 does not appear to ask which folder syncs"

echo
echo "== 4. turn 2 — answer it, init must run =="
[ -n "$SID" ] || { echo "cannot resume without a session_id" >&2; exit 1; }
T2=$(claude -p --resume "$SID" "Go with your recommendation." \
  --output-format json --allowedTools "$TOOLS" 2>&1)
printf '%s' "$T2" | json 'd.result'
echo
echo "  --- assertions ---"
MOUNT=$(find "$S/agent1" -maxdepth 3 -name config.json -path '*/.bdrive/*' | head -1)
[ -n "$MOUNT" ] && pass "a mount exists: $MOUNT" || fail "init never ran"
[ -d "$S/agent1/.bdrive" ] \
  && fail "the parent folder itself was mounted (expected a dedicated subfolder)" \
  || pass "parent folder not mounted bare"
grep -q beardrive "$HOME/.claude/settings.json" 2>/dev/null \
  && pass "hooks registered in ~/.claude/settings.json" \
  || fail "no hooks in ~/.claude/settings.json"
# Since #85 there is no plugin and no bundled skill — hooks are the whole
# integration. An agent reaching for either is following a stale doc.
printf '%s%s' "$T1" "$T2" | grep -qE 'plugin (marketplace )?(add|install)|SKILL\.md|bdrive skill' \
  && fail "transcript reaches for a plugin/marketplace/skill — all removed in #85" \
  || pass "no plugin or skill install attempted"
compgen -G "$HOME/.claude/skills/*" >/dev/null \
  && fail "a skill was installed under ~/.claude/skills — #85 removed that" \
  || pass "no skill dir created (correct since #85)"
printf '%s%s' "$T1" "$T2" | grep -q 'bdrive hooks install' \
  && fail "ran a separate 'bdrive hooks install' — init registers hooks inline" \
  || pass "no separate hooks command"

echo
echo "== 5. the payoff link serves real content =="
[ "$MODE" = new ] && PID=$(curl -sf -b "$jar" "$HUB/api/projects" | json "d.projects.find(p=>p.name===\"$PROJ\").id")
if [ -z "$PID" ]; then
  fail "no project named '$PROJ' exists on the hub"
else
  curl -sf -b "$jar" -o /dev/null -w "  /api/p/$PID/tree -> %{http_code}\n" "$HUB/api/p/$PID/tree" \
    && pass "hub serves the project ($PID)" || fail "hub did not serve the project"
  curl -sf -b "$jar" "$HUB/api/p/$PID/tree" | json 'JSON.stringify(d).slice(0,300)'
fi

echo
echo "== done: $FAILED mechanical assertion(s) failed =="
echo "   resume the session with:  claude -p --resume $SID '<next user turn>'"
exit $((FAILED > 0))
