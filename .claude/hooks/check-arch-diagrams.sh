#!/bin/sh
# PreToolUse(Bash) hook: block `gh pr create` when server code changed but
# architecture/ didn't. Heuristic only — a pure bug fix changes no
# structure; override by appending `# skip-diagram-check` to the command.
input=$(cat)
cmd=$(printf '%s' "$input" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("tool_input",{}).get("command",""))' 2>/dev/null)
case "$cmd" in
*"gh pr create"*) ;;
*) exit 0 ;;
esac
case "$cmd" in
*skip-diagram-check*) exit 0 ;;
esac
base=$(git merge-base origin/main HEAD 2>/dev/null || git merge-base main HEAD 2>/dev/null) || exit 0
# Every application package is drawn in architecture/ (see its README);
# frontend static/ is generated output, excluded via the src-only pathspec.
code=$(git diff --name-only "$base" HEAD -- 'cmd/' 'internal/*.go' \
	'internal/webapp/frontend/src/')
diag=$(git diff --name-only "$base" HEAD -- architecture/)
if [ -n "$code" ] && [ -z "$diag" ]; then
	cat >&2 <<EOF
This branch changes code covered by architecture/ diagrams but architecture/ is untouched:
$code

Before creating the PR: if any of these change types or relationships drawn
in architecture/, update the affected diagram, commit it, and add an
"Architecture changes" section to the PR body: say what changed, then show
Before and After mermaid excerpts of ONLY the affected classes and their
immediate relationships (Before = diagram at the merge base), never the full
diagram. If nothing structural changed, re-run the same command with
'# skip-diagram-check' appended.
EOF
	exit 2
fi
exit 0
