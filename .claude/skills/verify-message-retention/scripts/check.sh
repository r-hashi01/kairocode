#!/usr/bin/env bash
# Model-checks ../model/MessageRetention.tla with TLC (see ../SKILL.md).
#
#   check.sh                          # the whole check, bounds from the .cfg
#   check.sh -set MaxTime=3           # a constant set otherwise (repeatable)
#   check.sh -set RemoveDuringFork=TRUE   # reproduce finding F1 (exit 12)
#   check.sh -set ForkFailed=TRUE         # reproduce finding F2 (exit 12)
#   check.sh -coverage                # also report how often each action ran
#
# Exit status is TLC's: 0 passed, 12 an invariant violated, 150 the model
# does not parse, 2 tla2tools.jar's checksum is wrong.
# Output and any counterexample: ${KAIROCODE_TLA_OUT:-/tmp/kairocode-tla}/<stamp>/
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
model="$here/../model"
spec=MessageRetention
version=v1.7.4
sha=936a262061c914694dfd669a543be24573c45d5aa0ff20a8b96b23d01e050e88
# The same pinned jar kairo's verify-snapshot-retention uses, in the same place.
jar="${XDG_CACHE_HOME:-$HOME/.cache}/kairo-tla/$version/tla2tools.jar"
image=eclipse-temurin@sha256:d77e090ae1f6970c047b443c6f0a6c1358b0426d44432bb5a7753408cd43de0d

sets=()
coverage=()
while [ $# -gt 0 ]; do
	case $1 in
	-set)
		[[ ${2:-} =~ ^[A-Za-z]+=[A-Za-z0-9]+$ ]] || { echo "-set NAME=VALUE" >&2; exit 2; }
		sets+=("$2")
		shift 2
		;;
	-coverage) coverage=(-coverage 1); shift ;;
	*) echo "unknown argument: $1 (see the top of $0)" >&2; exit 2 ;;
	esac
done

if [ ! -f "$jar" ]; then
	mkdir -p "$(dirname "$jar")"
	curl -fsSL -o "$jar.tmp" "https://github.com/tlaplus/tlaplus/releases/download/$version/tla2tools.jar"
	mv "$jar.tmp" "$jar"
fi
echo "$sha  $jar" | shasum -a 256 -c - >/dev/null || { echo "tla2tools.jar checksum mismatch: $jar" >&2; exit 2; }

out="${KAIROCODE_TLA_OUT:-/tmp/kairocode-tla}/$(date +%Y%m%d-%H%M%S)-$$"
mkdir -p "$out"
cp "$model/$spec.tla" "$model/$spec.cfg" "$out/"
for kv in ${sets[@]+"${sets[@]}"}; do
	name=${kv%%=*}
	grep -q "^CONSTANT $name = " "$out/$spec.cfg" || { echo "no constant $name in $spec.cfg" >&2; exit 2; }
	sed -i.bak "s/^CONSTANT $name = .*/CONSTANT $name = ${kv#*=}/" "$out/$spec.cfg"
done
rm -f "$out/$spec.cfg.bak"
{
	echo "commit: $(git -C "$here" rev-parse HEAD) ($(git -C "$here" status --porcelain | wc -l | tr -d ' ') uncommitted paths)"
	grep '^CONSTANT' "$out/$spec.cfg"
} >"$out/run.txt"

# A Java 11 or later: JAVA, Homebrew's openjdk@21, or java on PATH; else the
# pinned JRE image through Docker.
java=${JAVA:-}
if [ -z "$java" ]; then
	for j in /opt/homebrew/opt/openjdk@21/bin/java /usr/local/opt/openjdk@21/bin/java "$(command -v java || true)"; do
		[ -n "$j" ] && [ -x "$j" ] && "$j" -version >/dev/null 2>&1 && java=$j && break
	done
fi
tlc=(-cp tla2tools.jar tlc2.TLC -workers auto -deadlock -cleanup ${coverage[@]+"${coverage[@]}"} -config "$spec.cfg" "$spec.tla")
cp "$jar" "$out/tla2tools.jar"
set +e
if [ -n "$java" ]; then
	(cd "$out" && "$java" -XX:+UseParallelGC "${tlc[@]}") >"$out/tlc.log" 2>&1
else
	docker run --rm -v "$out:/w" -w /w "$image" java -XX:+UseParallelGC "${tlc[@]}" >"$out/tlc.log" 2>&1
fi
rc=$?
set -e
rm -f "$out/tla2tools.jar"
grep -E '^(Model checking completed|Error:|Finished in|[0-9,]+ states generated)|Invariant .* is violated' "$out/tlc.log" || true
echo "tlc exit: $rc  log: $out/tlc.log"
exit "$rc"
