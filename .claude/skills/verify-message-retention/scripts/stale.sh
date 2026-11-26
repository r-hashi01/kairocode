#!/usr/bin/env bash
# Lists the changes, since the sync commit in ../SKILL.md (or since $1), to
# the code that ../model/MessageRetention.tla describes: committed and
# uncommitted alike. Exit 0 with a line saying so when nothing covered
# changed; exit 1 (and the hunks on stderr) when something did.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
repo=$(git -C "$here" rev-parse --show-toplevel)
sync=${1:-$(sed -n 's/^sync commit: `\([0-9a-f]*\)`.*/\1/p' "$here/../SKILL.md")}
[ -n "$sync" ] || { echo "no sync commit in SKILL.md" >&2; exit 2; }

# What the model stands for (SKILL.md, coverage). A hunk counts if its
# header (the enclosing func) or one of its changed lines names one of them.
# (Bracketed, not backslashed: awk -v would take the backslashes.)
funcs='sweepRoots|walk|take|[)] sweep[(]|Sweep[(]|sweepMessages|hashRe|sweepMu|Unswept|SweepMessage|RootTexts|ContentKinds|[)] Put[(]|func Hash|Extend|Compacted|Link|PutMessage|PutJSON|Fork|CreateSession|[)] Send[(]|sendMessage|CloseSession|runSession|SessionOutput|runUndo|RunTurnV3|ActHistoryExtend|HistoryExtendInput|r-hashi01/kairo '
files=(internal/runtime/sweep.go internal/store/sweep.go internal/store/store.go internal/runtime/fork.go
	internal/runtime/send.go internal/runtime/declare.go internal/workflows/session.go internal/workflows/turn.go go.mod)

diff=$(git -C "$repo" diff "$sync" -- "${files[@]}")
[ -z "$diff" ] && { echo "covered code unchanged since $sync"; exit 0; }
hits=$(echo "$diff" | awk -v re="$funcs" '
	/^diff --git/ { if (hunk != "" && hit) printf "%s", hunk; hunk = ""; hit = 0; file = $0; next }
	/^@@/ { if (hunk != "" && hit) printf "%s", hunk; hunk = file "\n" $0 "\n"; hit = ($0 ~ "(" re ")"); next }
	hunk != "" { hunk = hunk $0 "\n"; if ($0 ~ /^[+-]/ && $0 ~ "(" re ")") hit = 1 }
	END { if (hunk != "" && hit) printf "%s", hunk }
')
if [ -n "$hits" ]; then
	echo "$hits" >&2
	echo "the covered code changed since $sync: map the hunks above onto the model (SKILL.md, coverage)"
	exit 1
fi
echo "changes since $sync do not touch the covered functions"
