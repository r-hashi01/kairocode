package good

import (
	"maps"
	"slices"
	"time"
)

// Deterministic: sorted keys, durations as types only. "time.Now()" in a
// comment is fine.
func Turn(m map[string]int, timeout time.Duration) int {
	n := int(timeout / time.Second)
	for _, k := range slices.Sorted(maps.Keys(m)) {
		n += m[k]
	}
	return n
}
