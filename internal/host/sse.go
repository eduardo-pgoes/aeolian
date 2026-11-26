package host

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/eduardo-pgoes/aeolian/internal/store"
)

const (
	// ssePage is how many events the stream reads from the log at once.
	ssePage = 500
	// SSEMaxBacklog is how far behind the newest event a stream may
	// resume. A cursor further back gets a reset event instead, and the
	// client reloads its snapshot.
	SSEMaxBacklog = 10000
)

// ssePing is how often an idle stream writes a comment, so the client and
// any proxy see it is alive.
var ssePing = 15 * time.Second

// streamEvents serves GET /api/events/stream: the event log as
// Server-Sent Events. Each event's id is its log id and its data the event
// as JSON. The stream starts after the Last-Event-ID header, else the after
// query, else at the newest event, so a client that took a snapshot first
// gets only what came since. run, agent and kinds (comma separated) filter.
// A cursor more than SSEMaxBacklog events behind, or ahead of the log (a
// restored database), gets one reset event with the newest id, and the
// stream carries on from there.
func (h *Host) streamEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	q := r.URL.Query()
	eq := store.EventQuery{RunID: q.Get("run"), AgentID: q.Get("agent"), Limit: ssePage}
	if k := q.Get("kinds"); k != "" {
		eq.Kinds = strings.Split(k, ",")
	}
	cursor := r.Header.Get("Last-Event-ID")
	if cursor == "" {
		cursor = q.Get("after")
	}
	// Subscribe before the first read, so no append between the read and
	// the wait goes unnoticed.
	sig, cancel := h.Store.Subscribe()
	defer cancel()
	last, err := h.Store.LastEventID()
	if err != nil {
		httpError(w, err)
		return
	}
	after := last
	reset := false
	if cursor != "" {
		n, err := strconv.ParseInt(cursor, 10, 64)
		if err != nil || n < 0 {
			http.Error(w, "bad event cursor "+strconv.Quote(cursor), http.StatusBadRequest)
			return
		}
		after = n
		if n > last || last-n > SSEMaxBacklog {
			after, reset = last, true
		}
	}

	hd := w.Header()
	hd.Set("Content-Type", "text/event-stream")
	hd.Set("Cache-Control", "no-cache")
	hd.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "retry: 1000\n\n")
	if reset {
		fmt.Fprintf(w, "event: reset\ndata: {\"last_id\":%d}\n\n", last)
	}
	flusher.Flush()

	ping := time.NewTicker(ssePing)
	defer ping.Stop()
	for {
		eq.After = after
		evs, err := h.Store.Events(eq)
		if err != nil {
			fmt.Fprintf(w, "event: error\ndata: %s\n\n", jsonString(err.Error()))
			flusher.Flush()
			return
		}
		for _, ev := range evs {
			if err := writeEvent(w, ev); err != nil {
				return
			}
			after = ev.ID
		}
		if len(evs) > 0 {
			flusher.Flush()
		}
		if len(evs) == ssePage {
			continue
		}
		select {
		case <-sig:
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		case <-h.streamsStopped():
			return
		}
	}
}

func (h *Host) streamsStopped() chan struct{} {
	h.streamsOnce.Do(func() { h.streamsStop = make(chan struct{}) })
	return h.streamsStop
}

// StopStreams ends every event stream, now and from now on. Register it
// with http.Server.RegisterOnShutdown: Shutdown waits for open responses,
// and a stream never ends by itself.
func (h *Host) StopStreams() {
	ch := h.streamsStopped()
	h.streamsClose.Do(func() { close(ch) })
}

func writeEvent(w http.ResponseWriter, ev store.Event) error {
	if len(ev.Data) == 0 {
		ev.Data = json.RawMessage("{}")
	} else if bytes.ContainsAny(ev.Data, "\r\n") {
		var b bytes.Buffer
		if json.Compact(&b, ev.Data) == nil {
			ev.Data = b.Bytes()
		}
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return nil
	}
	_, err = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.ID, sseField(ev.Kind), data)
	return err
}

// sseField keeps a value on one SSE line.
func sseField(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
