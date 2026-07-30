#!/bin/sh
# Build and enter the sandbox: a disposable Linux machine to run a scenario in
# when it needs one. See the Dockerfile for what it provides and what does NOT
# belong in here.
#
#   ./sandbox/run.sh                     # interactive shell in a fresh machine
#   ./sandbox/run.sh onboarding new      # scenario: agent creates a new project
#   ./sandbox/run.sh onboarding join     # scenario: agent joins a seeded project
#   ./sandbox/run.sh daemon-linux        # scenario: systemd unit + pidfile race
#   ./sandbox/run.sh bash -c '...'       # anything else, then exit
#
# Test another checkout (a feature branch worktree) without touching this one:
#   BDRIVE_SRC=.claude/worktrees/my-branch ./sandbox/run.sh daemon-linux
#   BDRIVE_BIN=/path/to/linux/bdrive     ./sandbox/run.sh
#
# The hub is published on :8080 for your host browser, but it only lives as long
# as the container's command. The second form takes the hub down with it the
# moment the command finishes — so if you want to click the project link init
# prints, work inside the interactive shell.
#
# Claude Code auth, first match wins:
#
#   1. $CLAUDE_CODE_OAUTH_TOKEN  — mint once on your Mac with `claude setup-token`
#                                  and export it. This is the recommended path.
#   2. $ANTHROPIC_API_KEY        — a plain API key.
#   3. ~/.claude/.credentials.json — mounted read-only and copied inside, as a
#                                  fallback. On macOS this file is often a stale
#                                  leftover (the live token lives in Keychain),
#                                  so expect "OAuth session expired" if so.
#
# Deliberately NOT reading the Keychain: the container would refresh that token
# on its own and rotate it out from under your Mac, logging you out there —
# exactly the side effect this container exists to avoid. `claude setup-token`
# mints a separate token instead.
set -e
cd "$(dirname "$0")/.."

case "$(docker info --format '{{.Architecture}}')" in
  aarch64|arm64) GOARCH=arm64 ;;
  *)             GOARCH=amd64 ;;
esac

# $BDRIVE_SRC builds from another checkout (a worktree on a feature branch)
# instead of this one, so a branch can be tested without touching your working
# tree. $BDRIVE_BIN skips the build and uses a linux binary you already have.
if [ -n "${BDRIVE_BIN:-}" ]; then
  cp "$BDRIVE_BIN" sandbox/bdrive
else
  ( cd "${BDRIVE_SRC:-.}" && CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" go build \
      -o "$OLDPWD/sandbox/bdrive" ./cmd/bdrive )
fi

docker build -q -t bdrive-sandbox sandbox >/dev/null

AUTH=""
if [ -n "$CLAUDE_CODE_OAUTH_TOKEN" ]; then
  AUTH="-e CLAUDE_CODE_OAUTH_TOKEN"
elif [ -n "$ANTHROPIC_API_KEY" ]; then
  AUTH="-e ANTHROPIC_API_KEY"
elif [ -f "$HOME/.claude/.credentials.json" ]; then
  AUTH="-v $HOME/.claude/.credentials.json:/run/claude/credentials.json:ro"
  echo "note: no CLAUDE_CODE_OAUTH_TOKEN set, falling back to" >&2
  echo "      ~/.claude/.credentials.json — often stale on macOS. If claude asks" >&2
  echo "      you to sign in, that is why; see the header of this script." >&2
else
  echo "warning: no Claude credential found; claude will ask you to sign in." >&2
  echo "         Run \`claude setup-token\` on the host, export" >&2
  echo "         CLAUDE_CODE_OAUTH_TOKEN, and re-run this script." >&2
fi

# -t only when there is a terminal, so this stays scriptable from CI or an agent.
[ -t 0 ] && TTY=-it || TTY=-i

# shellcheck disable=SC2086 # AUTH and TTY are deliberately word-split
# A run killed at the client (Ctrl-C on a pipe, an agent's timeout) leaves the
# container up despite --rm, still holding :8080. Clear it rather than failing
# the next run with "port is already allocated".
docker rm -f bdrive-sandbox >/dev/null 2>&1 || true

# Named so a second shell can approve a device login mid-flow:
#   docker exec -it bdrive-sandbox bash
exec docker run --rm --name bdrive-sandbox $TTY \
  -v "$PWD/sandbox/bdrive":/usr/local/bin/bdrive:ro \
  -v "$PWD":/src:ro \
  $AUTH \
  -p 8080:8080 \
  bdrive-sandbox "$@"
