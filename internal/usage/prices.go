package usage

import "strings"

// Price is a model's list price in US dollars per million tokens. Cost is
// computed when it is read, never stored: prices change, tokens do not.
type Price struct {
	Input, Output, CacheRead float64
	// CacheWrite is a five-minute cache write, CacheWrite1h a one-hour one.
	CacheWrite, CacheWrite1h float64
}

// list builds a Price from input and output with the usual cache rates:
// a five-minute cache write is 1.25x input, a one-hour one 2x, a cache read
// 0.1x unless read is given.
func list(input, output, read float64) Price {
	if read == 0 {
		read = input / 10
	}
	return Price{Input: input, Output: output, CacheRead: read, CacheWrite: input * 1.25, CacheWrite1h: input * 2}
}

// Prices are first-party API list prices as of 2026-11, keyed by model id.
// Haiku 5.5 is priced for prompts up to 100K tokens.
var Prices = map[string]Price{
	"claude-fable-5-2":  list(8, 40, 0.20),
	"claude-mythos-5-2": list(8, 40, 0.20),
	"claude-fable-5-1":  list(10, 50, 0),
	"claude-mythos-5-1": list(10, 50, 0),
	"claude-fable-5":    list(10, 50, 0),
	"claude-opus-5-5":   list(4, 20, 0.20),
	"claude-opus-5":     list(5, 25, 0),
	"claude-opus-4-8":   list(5, 25, 0),
	"claude-opus-4-7":   list(5, 25, 0),
	"claude-opus-4-6":   list(5, 25, 0),
	"claude-sonnet-5-5": list(2, 10, 0.10),
	"claude-sonnet-5":   list(2, 10, 0),
	"claude-sonnet-4-6": list(3, 15, 0),
	"claude-haiku-5-5":  list(0.10, 0.50, 0),
	"claude-haiku-4-5":  list(1, 5, 0),
}

// PriceOf finds model's price, also under a dated or suffixed id such as
// claude-haiku-4-5-20251001 or claude-opus-5-5[1m].
func PriceOf(model string) (Price, bool) {
	if p, ok := Prices[model]; ok {
		return p, true
	}
	best := ""
	for id := range Prices {
		if strings.HasPrefix(model, id) && len(id) > len(best) {
			best = id
		}
	}
	p, ok := Prices[best]
	return p, ok && best != ""
}

// Cost is t at model's list price, in dollars; ok is false for a model
// with no known price.
func Cost(model string, t Tokens) (dollars float64, ok bool) {
	p, ok := PriceOf(model)
	if !ok {
		return 0, false
	}
	return (float64(t.Input)*p.Input + float64(t.Output)*p.Output +
		float64(t.CacheRead)*p.CacheRead + float64(t.CacheWrite-t.CacheWrite1h)*p.CacheWrite +
		float64(t.CacheWrite1h)*p.CacheWrite1h) / 1e6, true
}
