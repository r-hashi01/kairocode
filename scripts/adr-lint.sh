#!/usr/bin/env bash
# Checks docs/adr: unique numbers, required sections, every ADR in the index.
set -euo pipefail
cd "$(dirname "$0")/.."
fail=0
dups=$(ls docs/adr | grep -E '^[0-9]{4}-' | cut -c1-4 | sort | uniq -d)
[ -n "$dups" ] && { echo "adr-lint: duplicate numbers: $dups"; fail=1; }
for f in docs/adr/[0-9][0-9][0-9][0-9]-*.md; do
  [ -e "$f" ] || continue
  for s in '- 状態:' '## 背景' '## 決定' '## 結果' '## 検討した代替案'; do
    grep -qF -- "$s" "$f" || { echo "adr-lint: $f lacks '$s'"; fail=1; }
  done
  grep -qF "($(basename "$f"))" docs/adr/README.md || { echo "adr-lint: $f is not in docs/adr/README.md"; fail=1; }
done
exit $fail
