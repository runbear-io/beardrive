#!/bin/sh
# Storyboard for the quickstart→sync demo GIF (BEA-4 content-calendar rev 3:
# agent-first, no bare CLI on screen). Rendered by demo.tape via vhs.
# Every output line replays a real output from the fresh-machine run
# verified in BEA-33 on v0.10.0; timing is condensed for demo pace.

ESC=$(printf '\033')
DIM="${ESC}[2m"; BOLD="${ESC}[1m"; RESET="${ESC}[0m"
ORANGE="${ESC}[38;5;214m"; GREEN="${ESC}[38;5;114m"; BLUE="${ESC}[38;5;110m"; GRAY="${ESC}[38;5;245m"

say() { # typewriter for the human's prompt line
  printf '%s' "${BOLD}❯ ${RESET}"
  printf '%s' "$1" | while IFS= read -r -n1 c 2>/dev/null || [ -n "$c" ]; do
    printf '%s' "$c"; sleep 0.03
  done
  printf '\n'; sleep 0.4
}
agent() { printf '%s\n' "${ORANGE}●${RESET} $1"; sleep 0.7; }
step()  { sleep 0.5; printf '%s\n' "  ${GREEN}✓${RESET} $1"; }
note()  { printf '%s\n' "  ${GRAY}$1${RESET}"; sleep 0.8; }
title() { printf '%s\n\n' "${BLUE}${BOLD}$1${RESET}"; sleep 0.6; }

printf '\033[?25l' # hide cursor for the recording
clear
title "your agent — machine 1"
say "/beardrive:install"
agent "Setting up BearDrive…"
step "bdrive 0.10.0 installed"
step "signed in: snow@runbear.io"
step "project connected: launch-wiki — this folder now syncs every agent turn"
sleep 1.2
say "draft tomorrow's launch plan and save it for the team"
agent "Writing plan.md…"
step "plan.md — synced"
sleep 2

clear
title "your teammate's agent — another machine"
say "what's the plan for tomorrow?"
agent "Reading plan.md — synced seconds ago by Snow on Snows-Mac-mini"
note "\"Launch Wed 09:00 PT — beta invites at 08:00, status thread in #launch…\""
sleep 2
say "share it with our PM"
agent "Shared — renders as a page, with full change history:"
note "https://hub.beardrive.ai/s/f5669a3cbe035281"
sleep 2.5

clear
printf '\n\n'
printf '%s\n' "  ${ORANGE}${BOLD}BearDrive${RESET} — Google Drive for AI agents ${DIM}(and their humans)${RESET}"
printf '\n'
printf '%s\n' "  ${DIM}github.com/runbear-io/beardrive${RESET}"
sleep 3
