#!/usr/bin/env bash
# SessionStart: a short orientation added to Claude's context.
set -uo pipefail
root=${CLAUDE_PROJECT_DIR:-$(pwd)}
cd "$root" || exit 0
. .claude/hooks/lib.sh
changed=$(git status --porcelain 2>/dev/null | wc -l | tr -d ' ')
branch=$(git branch --show-current 2>/dev/null)
green=unknown
[ -f .claude/.cache/last-green ] && { [ "$(cat .claude/.cache/last-green)" = "$(source_hash)" ] && green=yes || green="no (sources changed since)"; }
# A partial pass: the tests that start a sandbox were skipped (ADR 0031).
[ "$green" = yes ] && [ -f .claude/.cache/last-green-partial ] && [ "$(cat .claude/.cache/last-green-partial)" = "$(source_hash)" ] &&
  green="yes, partial (the tests that start a sandbox were skipped; run scripts/check.sh outside a sandbox to have them all)"
stage=$(grep -m1 '^今の段階' docs/milestones.md 2>/dev/null | sed 's/^今の段階: //')
design=missing
[ -f ../kairo/docs/kairocode/design.md ] && design=present
cat <<MSG
kairocode session: branch=${branch:-none}, uncommitted paths=$changed, last quick check green for this tree: $green ($(go version | awk '{print $3}')).
Current milestone: ${stage:-unknown} (docs/milestones.md). Requirements/design in ../kairo/docs/kairocode/ ($design).
Read AGENTS.md before changing code. Verify with scripts/check.sh; work milestones with /milestone.
MSG
