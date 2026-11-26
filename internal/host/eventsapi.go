package host

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/eduardo-pgoes/aeolian/internal/store"
)

// listEvents answers GET /api/events with a JSON array of events in id
// order. Query parameters, all optional:
//
//	after  return events with an id above this one (default 0)
//	run    only this run's events
//	agent  only this agent's events
//	kind   only these kinds; repeat it or separate kinds with commas
//	limit  at most this many, up to store.DefaultEventLimit (the default)
//
// A reader pages through the log by passing the last id it saw as after,
// until a page comes back shorter than its limit.
func (h *Host) listEvents(w http.ResponseWriter, r *http.Request) {
	q, err := eventQuery(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	evs, err := h.Store.Events(q)
	if err != nil {
		httpError(w, err)
		return
	}
	if evs == nil {
		evs = []store.Event{}
	}
	writeJSON(w, http.StatusOK, evs)
}

func eventQuery(r *http.Request) (store.EventQuery, error) {
	v := r.URL.Query()
	q := store.EventQuery{RunID: v.Get("run"), AgentID: v.Get("agent")}
	if s := v.Get("after"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n < 0 {
			return q, fmt.Errorf("after must be an event id, not %q", s)
		}
		q.After = n
	}
	if s := v.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > store.DefaultEventLimit {
			return q, fmt.Errorf("limit must be between 1 and %d, not %q", store.DefaultEventLimit, s)
		}
		q.Limit = n
	}
	for _, k := range v["kind"] {
		for kind := range strings.SplitSeq(k, ",") {
			if kind = strings.TrimSpace(kind); kind != "" {
				q.Kinds = append(q.Kinds, kind)
			}
		}
	}
	return q, nil
}
