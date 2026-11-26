package host

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eduardo-pgoes/aeolian/internal/store"
)

type sseEvent struct {
	id    int64
	event string
	data  string
}

// stream opens /api/events/stream with query and, unless lastID is empty,
// a Last-Event-ID header, and parses events into the returned channel
// until the response ends. cancel closes the connection.
func (f *fixture) stream(t *testing.T, query, lastID string) (<-chan sseEvent, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", f.srv.URL+"/api/events/stream"+query, nil)
	req.Header.Set("Accept", "text/event-stream")
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		cancel()
		t.Fatalf("stream: %s %s", resp.Status, resp.Header.Get("Content-Type"))
	}
	out := make(chan sseEvent, 4096)
	go func() {
		defer resp.Body.Close()
		defer close(out)
		sc := bufio.NewScanner(resp.Body)
		var ev sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if ev.event != "" || ev.data != "" {
					out <- ev
				}
				ev = sseEvent{}
			case strings.HasPrefix(line, "id: "):
				ev.id, _ = strconv.ParseInt(line[4:], 10, 64)
			case strings.HasPrefix(line, "event: "):
				ev.event = line[7:]
			case strings.HasPrefix(line, "data: "):
				ev.data = line[6:]
			}
		}
	}()
	t.Cleanup(cancel)
	return out, cancel
}

func next(t *testing.T, c <-chan sseEvent, within time.Duration) sseEvent {
	t.Helper()
	select {
	case ev, ok := <-c:
		if !ok {
			t.Fatal("stream ended")
		}
		return ev
	case <-time.After(within):
		t.Fatalf("no event within %v", within)
	}
	return sseEvent{}
}

func (f *fixture) appendN(t *testing.T, n int, kind, run string) {
	t.Helper()
	evs := make([]store.Event, n)
	for i := range evs {
		evs[i] = store.Event{Kind: kind, RunID: run, Data: json.RawMessage(fmt.Sprintf(`{"i":%d}`, i))}
	}
	if err := f.store.Append(evs...); err != nil {
		t.Fatal(err)
	}
}

func TestStreamBacklogThenLive(t *testing.T) {
	f := newFixture(t)
	f.appendN(t, 3, "a", "")
	evs, _ := f.stream(t, "?after=0", "")
	for want := int64(1); want <= 3; want++ {
		ev := next(t, evs, time.Second)
		if ev.id != want || ev.event != "a" {
			t.Fatalf("backlog event %+v, want id %d", ev, want)
		}
		var got store.Event
		if err := json.Unmarshal([]byte(ev.data), &got); err != nil || got.ID != want || got.Kind != "a" {
			t.Fatalf("data %q: %v", ev.data, err)
		}
	}
	start := time.Now()
	f.appendN(t, 1, "live", "")
	if ev := next(t, evs, time.Second); ev.id != 4 || ev.event != "live" {
		t.Fatalf("live: %+v", ev)
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Errorf("live event took %v", d)
	}
}

func TestStreamWithoutCursorStartsAtNewest(t *testing.T) {
	f := newFixture(t)
	f.appendN(t, 5, "old", "")
	evs, _ := f.stream(t, "", "")
	f.appendN(t, 1, "new", "")
	if ev := next(t, evs, time.Second); ev.id != 6 || ev.event != "new" {
		t.Fatalf("first event %+v", ev)
	}
}

func TestStreamResumesFromLastEventID(t *testing.T) {
	f := newFixture(t)
	f.appendN(t, 3, "a", "")
	evs, cancel := f.stream(t, "?after=0", "")
	var last int64
	for range 2 {
		last = next(t, evs, time.Second).id
	}
	cancel()
	f.appendN(t, 2, "b", "")
	// The header wins over the query.
	evs, _ = f.stream(t, "?after=0", strconv.FormatInt(last, 10))
	var got []int64
	for range 3 {
		got = append(got, next(t, evs, time.Second).id)
	}
	if fmt.Sprint(got) != "[3 4 5]" {
		t.Fatalf("resumed ids %v", got)
	}
	select {
	case ev := <-evs:
		t.Fatalf("extra event %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestStreamFilters(t *testing.T) {
	f := newFixture(t)
	f.appendN(t, 1, "x", "r1")
	f.appendN(t, 1, "y", "r1")
	f.appendN(t, 1, "x", "r2")
	f.appendN(t, 1, "z", "r1")
	evs, _ := f.stream(t, "?after=0&run=r1&kinds=x,z", "")
	if a, b := next(t, evs, time.Second), next(t, evs, time.Second); a.id != 1 || b.id != 4 {
		t.Fatalf("filtered: %+v %+v", a, b)
	}
}

func TestStreamPagesALongBacklog(t *testing.T) {
	f := newFixture(t)
	f.appendN(t, ssePage+20, "a", "")
	evs, _ := f.stream(t, "?after=0", "")
	for want := int64(1); want <= ssePage+20; want++ {
		if ev := next(t, evs, 2*time.Second); ev.id != want {
			t.Fatalf("event %d: %+v", want, ev)
		}
	}
}

func TestStreamResetsAFarCursor(t *testing.T) {
	f := newFixture(t)
	f.appendN(t, 3, "a", "")
	// A cursor past the log: the database was restored from a backup.
	evs, cancel := f.stream(t, "", "99")
	if ev := next(t, evs, time.Second); ev.event != "reset" || ev.data != `{"last_id":3}` {
		t.Fatalf("reset: %+v", ev)
	}
	f.appendN(t, 1, "after-reset", "")
	if ev := next(t, evs, time.Second); ev.id != 4 {
		t.Fatalf("after reset: %+v", ev)
	}
	cancel()

	// A cursor too far behind.
	for range SSEMaxBacklog/1000 + 1 {
		f.appendN(t, 1000, "bulk", "")
	}
	last, _ := f.store.LastEventID()
	evs, _ = f.stream(t, "?after=1", "")
	if ev := next(t, evs, time.Second); ev.event != "reset" || ev.data != fmt.Sprintf(`{"last_id":%d}`, last) {
		t.Fatalf("far reset: %+v", ev)
	}
	f.appendN(t, 1, "tail", "")
	if ev := next(t, evs, time.Second); ev.id != last+1 {
		t.Fatalf("after far reset: %+v", ev)
	}
}

func TestStreamBadCursor(t *testing.T) {
	f := newFixture(t)
	resp, err := http.Get(f.srv.URL + "/api/events/stream?after=nope")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestStreamPings(t *testing.T) {
	old := ssePing
	ssePing = 20 * time.Millisecond
	defer func() { ssePing = old }()
	f := newFixture(t)
	resp, err := http.Get(f.srv.URL + "/api/events/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	r := bufio.NewReader(resp.Body)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == ": ping\n" {
			return
		}
	}
	t.Fatal("no ping")
}

func TestStreamDisconnectLeaksNothing(t *testing.T) {
	f := newFixture(t)
	f.appendN(t, 1, "warm", "")
	// Warm the client and server pools first.
	_, c := f.stream(t, "", "")
	c()
	time.Sleep(50 * time.Millisecond)
	before := runtime.NumGoroutine()
	var cancels []context.CancelFunc
	for range 10 {
		_, c := f.stream(t, "", "")
		cancels = append(cancels, c)
	}
	for _, c := range cancels {
		c()
	}
	waitFor(t, "stream goroutines to end", func() bool {
		f.appendN(t, 1, "nudge", "")
		return runtime.NumGoroutine() <= before+2
	})
}

func TestHostInfoLastEventID(t *testing.T) {
	f := newFixture(t)
	f.appendN(t, 4, "a", "")
	resp, err := http.Get(f.srv.URL + "/api/host")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var v HostView
	json.NewDecoder(resp.Body).Decode(&v)
	if v.LastEventID != 4 {
		t.Fatalf("host: %+v", v)
	}
}

func TestStopStreamsEndsStreams(t *testing.T) {
	f := newFixture(t)
	evs, _ := f.stream(t, "", "")
	f.host.StopStreams()
	select {
	case _, ok := <-evs:
		if ok {
			t.Fatal("event after stop")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream still open after StopStreams")
	}
	// A stream opened afterwards ends at once too.
	evs, _ = f.stream(t, "", "")
	select {
	case <-evs:
	case <-time.After(2 * time.Second):
		t.Fatal("new stream stayed open")
	}
}
