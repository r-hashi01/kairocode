package api

import "fmt"

// SweepReport is what a sweep removed (ADR 0045), or with Dry would
// remove.
type SweepReport struct {
	Dry          bool  `json:"dry,omitempty"`
	Messages     int   `json:"messages"`
	MessageBytes int64 `json:"message_bytes"`
	// Images: the files images were given to engines as.
	Images int `json:"images"`
	// Stores: the snapshot stores pruned (with Dry, that would be);
	// StoreBytes, what pruning them freed.
	Stores     int   `json:"stores"`
	StoreBytes int64 `json:"store_bytes"`
	// Rows: rows about sessions that are gone.
	Rows int `json:"rows"`
	// Usage: rows of the usage ledger past retention.usage (not counted
	// with Dry).
	Usage      int64 `json:"usage"`
	Items      int   `json:"items"`
	Branches   int   `json:"branches"`
	Caches     int   `json:"caches"`
	CacheBytes int64 `json:"cache_bytes"`
}

func (r SweepReport) String() string {
	return fmt.Sprintf("%d messages (%s) and %d image files, %d snapshot stores pruned (%s freed), %d rows of sessions gone, %d usage rows, %d items (%d branches), %d check caches (%s)",
		r.Messages, Bytes(r.MessageBytes), r.Images, r.Stores, Bytes(r.StoreBytes), r.Rows, r.Usage, r.Items, r.Branches, r.Caches, Bytes(r.CacheBytes))
}

// Bytes is n bytes, for people.
func Bytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
