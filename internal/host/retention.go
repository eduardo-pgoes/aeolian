package host

import (
	"log"
	"time"

	"github.com/eduardo-pgoes/aeolian/internal/store"
)

// DefaultEventsRetention is how long the hook input of a finished run is
// kept before its payloads are pruned.
const DefaultEventsRetention = 30 * 24 * time.Hour

// PruneInterval is how often Watch prunes old event payloads.
const PruneInterval = time.Hour

// PruneEvents prunes the bulky payloads (store.BulkyKinds) of every run
// that finished, and was acked if it ever was, more than EventsRetention
// before now. Rows and ids stay, so cursors and Fold are unaffected. It
// does nothing when EventsRetention is zero or less, and returns how many
// events it pruned.
func (h *Host) PruneEvents(now time.Time) int64 {
	if h.EventsRetention <= 0 {
		return 0
	}
	runs, err := h.Store.Runs(store.RunFilter{})
	if err != nil {
		log.Printf("prune events: %v", err)
		return 0
	}
	cutoff := now.Add(-h.EventsRetention)
	var old []string
	for _, r := range runs {
		if !store.Finished(r.State) || r.DoneAt == nil {
			continue
		}
		last := *r.DoneAt
		if r.AckedAt != nil && r.AckedAt.After(last) {
			last = *r.AckedAt
		}
		if last.Before(cutoff) {
			old = append(old, r.ID)
		}
	}
	n, err := h.Store.CompactEvents(old, store.BulkyKinds)
	if err != nil {
		log.Printf("prune events: %v", err)
	}
	return n
}
