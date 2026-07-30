#!/bin/bash
# Container entrypoint: set up Claude Code auth, start a hub, seed an account,
# then hand over to the command (a shell by default). Nothing here touches the
# host — $HOME is inside the container and /data is thrown away with it.
set -e
mkdir -p "$HOME/.claude" /work /data/store

# Claude Code keeps its OAuth token in its config dir, so a fresh $HOME is a
# logged-out one. Copy the read-only mounted credential into the container's
# own HOME: token refresh then writes here and never back to your host file.
if [ -f /run/claude/credentials.json ]; then
  cp /run/claude/credentials.json "$HOME/.claude/.credentials.json"
  chmod 600 "$HOME/.claude/.credentials.json"
fi
# Pre-trust /work so claude doesn't stop at the trust dialog, and mark
# onboarding done — in a fresh $HOME an interactive `claude` otherwise runs the
# first-run wizard, whose first step is a sign-in screen, even when the token in
# the environment is perfectly good.
[ -f "$HOME/.claude.json" ] || cat > "$HOME/.claude.json" <<'JSON'
{
  "hasCompletedOnboarding": true,
  "projects": {
    "/work": {"hasTrustDialogAccepted": true},
    "/src":  {"hasTrustDialogAccepted": true}
  }
}
JSON

# Report which credential actually arrived. "asked me to sign in" has two very
# different causes — nothing reached the container, or something did and was
# rejected — and without this you cannot tell them apart.
if [ -n "${CLAUDE_CODE_OAUTH_TOKEN:-}" ]; then
  CLAUDE_AUTH="CLAUDE_CODE_OAUTH_TOKEN (${#CLAUDE_CODE_OAUTH_TOKEN} chars)"
elif [ -n "${ANTHROPIC_API_KEY:-}" ]; then
  CLAUDE_AUTH="ANTHROPIC_API_KEY (${#ANTHROPIC_API_KEY} chars)"
elif [ -f "$HOME/.claude/.credentials.json" ]; then
  CLAUDE_AUTH="host .credentials.json — often stale on macOS"
else
  CLAUDE_AUTH="NONE — claude will ask you to sign in"
fi

# Completes `bdrive login --device` without a browser: signs in as the seeded
# hub account and POSTs the approval the browser would. Takes the link the CLI
# printed, or just its token. Approval is POST /auth/device/<token> with a
# session cookie and no body — see BuiltinAuth.pageDevice.
cat > /usr/local/bin/bdrive-approve <<'SH'
#!/bin/bash
set -e
[ -n "$1" ] || { echo "usage: bdrive-approve <link-or-token>" >&2; exit 1; }
token=${1##*/}
jar=$(mktemp)
curl -sf -c "$jar" -d "email=$HUB_EMAIL&password=$HUB_PASSWORD" "$HUB/auth/login" -o /dev/null
curl -sf -b "$jar" -X POST "$HUB/auth/device/$token" -o /dev/null
echo "approved $token"
SH
chmod +x /usr/local/bin/bdrive-approve

# Signs this device in with no browser and no interaction, by driving both
# halves of the device flow. Use it before an unattended `claude -p` run:
# `bdrive init` does its own device login and would otherwise print a link and
# block forever waiting for someone to open it.
cat > /usr/local/bin/bdrive-signin <<'SH'
#!/bin/bash
set -e
log=$(mktemp)
bdrive login --device "$HUB" >"$log" 2>&1 &
for _ in $(seq 1 30); do
  grep -qE "/auth/device/[a-f0-9]+" "$log" && break
  sleep 1
done
link=$(grep -oE "https?://\S+/auth/device/[a-f0-9]+" "$log" | tail -1)
[ -n "$link" ] || { echo "no sign-in link appeared:" >&2; cat "$log" >&2; exit 1; }
bdrive-approve "$link"
wait
bdrive login --status
SH
chmod +x /usr/local/bin/bdrive-signin

# allow_signup needs a gate or the hub refuses to start, hence allowed_domains.
if [ ! -f /data/hub.json ]; then
  cat > /data/hub.json <<'JSON'
{
  "remote": "file:///data/store",
  "addr": ":8080",
  "upload": true,
  "projects_db": "/data/projects.json",
  "auth": {
    "allow_signup": true,
    "allowed_domains": ["example.com"],
    "users_db": "/data/auth.json",
    "admins": ["me@example.com"]
  },
  "database": {"driver": "file"}
}
JSON
fi

BDRIVE_HOME=/data/hubhome bdrive web -c /data/hub.json >/data/hub.log 2>&1 &
for _ in $(seq 1 40); do
  curl -sf -o /dev/null "$HUB/auth/login" && break
  sleep 1
done
if ! curl -sf -o /dev/null "$HUB/auth/login"; then
  echo "hub failed to start:" >&2
  cat /data/hub.log >&2
  exit 1
fi

# Already-exists is fine: idempotent across restarts if /data is a volume.
curl -s -d "email=$HUB_EMAIL&password=$HUB_PASSWORD&name=Me" "$HUB/auth/signup" -o /dev/null || true

cat <<BANNER

  hub      $HUB  (also http://localhost:8080 on your Mac —
           but only while this container's command runs: exit and it is gone,
           which makes the project link init prints dead. To browse it, use the
           interactive shell — sandbox/run.sh with no arguments.)
  account  $HUB_EMAIL / $HUB_PASSWORD
  bdrive   $(bdrive version 2>&1)
  claude   $(claude --version 2>&1)
  auth     $CLAUDE_AUTH

  scenarios that need this machine:
    onboarding new|join   a real claude session following /src/INSTALL_FOR_AGENTS.md
    daemon-linux          the systemd user unit, and the daemon.pid/stop race

  poke at it by hand:
    bdrive-signin                                   # sign in, no browser
    mkdir shared && bdrive init shared --yes         # names the project after the folder
    bdrive-approve <link>                           # if you'd rather watch init
                                                    # ask: docker exec -it bdrive-sandbox bash

BANNER

exec "$@"
