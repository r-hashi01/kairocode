# Shared by the hooks. A hash of every source the checks depend on, so a green
# run can be reused until something changes.
source_hash() {
  find . \( -path ./.git -o -path ./.claude/.cache -o -path ./bin \) -prune -o \
    \( -name '*.go' -o -name go.mod -o -name go.sum -o -path './scripts/*' -o -path './docs/adr/*' \) \
    -type f -print | LC_ALL=C sort | xargs cat 2>/dev/null | shasum | cut -d' ' -f1
}
