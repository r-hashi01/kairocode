#!/usr/bin/env bash
# Whether a change needs the tests (CI's "changes" job): code=true unless
# every file changed is documentation. Without a base to compare with (a new
# tag, a scheduled or manual run), it is code.
#   BASE=<sha> HEAD=<sha> scripts/ci-changes.sh   the files between them
#   scripts/ci-changes.sh <path>...                these paths (to try it)
# Prints code=true|false, and writes it to $GITHUB_OUTPUT when set.
set -euo pipefail
cd "$(dirname "$0")/.."

# docs says whether path is documentation: nothing the build or the tests
# read. docs/examples/<dir>/ is not (a test reads docs/examples/plugins),
# nor are scripts and hooks.
docs() {
  case $1 in
    */*.md) ;;
    *.md | LICENSE) return 0 ;;
  esac
  case $1 in
    docs/examples/*/*) return 1 ;;
    docs/*.md | docs/adr/* | docs/proposals/* | docs/examples/*.md) return 0 ;;
    .claude/skills/* | .claude/agents/*) return 0 ;;
  esac
  return 1
}

if [[ $# -gt 0 ]]; then
  files=$(printf '%s\n' "$@")
elif [[ -n ${BASE:-} && ! $BASE =~ ^0+$ && -n ${HEAD:-} ]]; then
  files=$(git diff --name-only "$BASE" "$HEAD") || files=""
else
  files=""
fi

code=true
if [[ -n $files ]]; then
  code=false
  while IFS= read -r f; do
    [[ -z $f ]] && continue
    if ! docs "$f"; then
      echo "code: $f"
      code=true
      break
    fi
  done <<< "$files"
fi
echo "code=$code"
if [[ -n ${GITHUB_OUTPUT:-} ]]; then
  echo "code=$code" >> "$GITHUB_OUTPUT"
fi
