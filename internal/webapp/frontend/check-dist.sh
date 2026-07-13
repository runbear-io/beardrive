#!/bin/sh
# Verify the committed build output at ../static matches the frontend
# source. Run before tagging a release (goreleaser requires a clean tree,
# so a stale dist would otherwise slip through silently).
set -e
cd "$(dirname "$0")"
npm ci
npm run build
if [ -n "$(git status --porcelain -- ../static)" ]; then
  echo "internal/webapp/static is stale: rebuild and commit it" >&2
  git status --short -- ../static >&2
  exit 1
fi
echo "internal/webapp/static is fresh"
