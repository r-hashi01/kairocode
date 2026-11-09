package provider

import (
	"strings"

	"github.com/r-hashi01/kairocode/internal/msg"
)

// Price is what a model's tokens cost, in US dollars per million tokens
// (ADR 0039).
type Price struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

// Cost is what u costs at p, in US dollars.
func (p Price) Cost(u msg.Usage) float64 {
	return (float64(u.Input)*p.Input + float64(u.Output)*p.Output +
		float64(u.CacheRead)*p.CacheRead + float64(u.CacheWrite)*p.CacheWrite) / 1e6
}

// claudePrice is the price of a Claude model on Anthropic's API: a cache
// write (five minutes) costs 1.25 times the input, a read costs read times the
// input (0.1 for most models).
func claudePrice(in, out, read float64) Price {
	return Price{Input: in, Output: out, CacheRead: in * read, CacheWrite: in * 1.25}
}

// prices are the prices of the models KairoCode knows, by the model's id
// (a dated id, claude-…-20250101, is priced as its id without the date).
// Taken from Anthropic's published prices (2026-10); the settings
// (models.<name>.price) override or add to them.
var prices = map[string]Price{
	"claude-fable-5-1":  {Input: 10, Output: 50, CacheRead: 0.25, CacheWrite: 12.5},
	"claude-mythos-5-1": {Input: 10, Output: 50, CacheRead: 0.25, CacheWrite: 12.5},
	"claude-fable-5":    claudePrice(10, 50, 0.1),
	"claude-opus-5-5":   {Input: 4, Output: 20, CacheRead: 0.20, CacheWrite: 5},
	"claude-opus-5":     claudePrice(5, 25, 0.1),
	"claude-opus-4-8":   claudePrice(5, 25, 0.1),
	"claude-opus-4-7":   claudePrice(5, 25, 0.1),
	"claude-opus-4-6":   claudePrice(5, 25, 0.1),
	"claude-opus-4-5":   claudePrice(5, 25, 0.1),
	"claude-sonnet-5-5": claudePrice(2, 10, 0.1),
	"claude-sonnet-5":   claudePrice(2, 10, 0.1),
	"claude-sonnet-4-6": claudePrice(3, 15, 0.1),
	"claude-sonnet-4-5": claudePrice(3, 15, 0.1),
	"claude-sonnet-4-0": claudePrice(3, 15, 0.1),
	"claude-haiku-5-5":  claudePrice(0.10, 0.50, 0.1),
	"claude-haiku-4-5":  claudePrice(1, 5, 0.1),
}

// PriceOf is the price of model, and whether it is known.
func PriceOf(model string) (Price, bool) {
	if p, ok := prices[model]; ok {
		return p, true
	}
	// A dated id: claude-sonnet-4-5-20250929.
	if i := strings.LastIndex(model, "-"); i > 0 && len(model)-i-1 == 8 && digits(model[i+1:]) {
		p, ok := prices[model[:i]]
		return p, ok
	}
	return Price{}, false
}

func digits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}
