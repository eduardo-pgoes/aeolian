package host

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/eduardo-pgoes/aeolian/internal/store"
)

// spawnLater starts a helper without waiting and returns its id.
func (f *fixture) spawnLater(t *testing.T, session *mcp.ClientSession, role, text string) string {
	t.Helper()
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "spawn", Arguments: map[string]any{
		"role": role, "handoff": map[string]any{"text": text, "worktrees": []store.Worktree{}}, "wait": false,
	}})
	if err != nil || res.IsError {
		t.Fatalf("spawn: %v %+v", err, res)
	}
	m := regexp.MustCompile(`starting as ([0-9a-f-]{36})`).FindStringSubmatch(resultText(res))
	if m == nil {
		t.Fatalf("no id in %q", resultText(res))
	}
	return m[1]
}

type awaitCall struct {
	text  string
	isErr bool
}

func (f *fixture) await(session *mcp.ClientSession, args map[string]any) <-chan awaitCall {
	out := make(chan awaitCall, 1)
	go func() {
		params := &mcp.CallToolParams{Name: "await", Arguments: args}
		params.SetProgressToken("await-1")
		res, err := session.CallTool(context.Background(), params)
		if err != nil {
			out <- awaitCall{err.Error(), true}
			return
		}
		out <- awaitCall{resultText(res), res.IsError}
	}()
	return out
}

func (f *fixture) waitStarted(t *testing.T, id string) {
	t.Helper()
	waitFor(t, "helper "+id+" to start", func() bool {
		a, err := f.store.Agent(id)
		return err == nil && a.State == store.StateBusy
	})
}

func (h *Host) awaits(parentID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	if mb := h.mail[parentID]; mb != nil {
		for _, w := range mb.waits {
			if w.want != nil {
				n++
			}
		}
	}
	return n
}

func TestHeadlessFanOutAwaitAll(t *testing.T) {
	f := newFixture(t)
	f.host.MaxAgents = 3
	f.host.ProgressInterval = 5 * time.Millisecond
	_, build := f.startHeadless(t, headlessWorkflow)
	session := f.connect(t, build.ID)
	var ids []string
	for i := range 3 {
		ids = append(ids, f.spawnLater(t, session, "reviewer", "part "+string(rune('a'+i))))
	}
	// The lead is busy, so only two helpers fit under the cap...
	waitFor(t, "two helpers", func() bool { return f.keeper.startedCount() == 3 })
	time.Sleep(20 * time.Millisecond)
	if n := f.keeper.startedCount(); n != 3 {
		t.Fatalf("%d started over the cap", n)
	}
	// ...until it blocks in await, which frees its slot.
	call := f.await(session, map[string]any{})
	waitFor(t, "third helper", func() bool { return f.keeper.startedCount() == 4 })
	for i, id := range ids {
		f.waitStarted(t, id)
		f.done(t, id, "finding "+string(rune('a'+i)))
		if i < 2 {
			select {
			case c := <-call:
				t.Fatalf("await all returned early: %+v", c)
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	c := <-call
	if c.isErr {
		t.Fatalf("await: %s", c.text)
	}
	for i, id := range ids {
		want := "Helper reviewer (" + id + ") finished:\n\nfinding " + string(rune('a'+i))
		if !strings.Contains(c.text, want) {
			t.Fatalf("await lacks %q: %q", want, c.text)
		}
	}
	if strings.Contains(c.text, "Still running") {
		t.Fatalf("await: %q", c.text)
	}
	if f.host.awaits(build.ID) != 0 {
		t.Fatal("await still registered")
	}
	// Collected handoffs are not returned twice.
	again := <-f.await(session, map[string]any{})
	if again.isErr || !strings.HasPrefix(again.text, "No helpers are outstanding") {
		t.Fatalf("second await: %+v", again)
	}
	// Named helpers that finished come back from the store.
	named := <-f.await(session, map[string]any{"helpers": []string{ids[1]}})
	if named.isErr || !strings.Contains(named.text, "finding b") {
		t.Fatalf("named await: %+v", named)
	}
	noTyping(t, f.keeper)
}

func TestAwaitAny(t *testing.T) {
	f := newFixture(t)
	f.host.MaxAgents = 3
	_, build := f.startHeadless(t, headlessWorkflow)
	session := f.connect(t, build.ID)
	first := f.spawnLater(t, session, "reviewer", "one")
	second := f.spawnLater(t, session, "reviewer", "two")
	call := f.await(session, map[string]any{"mode": "any"})
	f.waitStarted(t, second)
	f.done(t, second, "second is quick")
	c := <-call
	if c.isErr || !strings.Contains(c.text, "second is quick") || !strings.Contains(c.text, "Still running: reviewer ("+first+")") || strings.Contains(c.text, "Timed out") {
		t.Fatalf("await any: %+v", c)
	}
	f.waitStarted(t, first)
	f.done(t, first, "first is slow")
	c = <-f.await(session, map[string]any{"mode": "any"})
	if c.isErr || !strings.Contains(c.text, "first is slow") || strings.Contains(c.text, "second is quick") {
		t.Fatalf("second await any: %+v", c)
	}
}

func TestAwaitTimeout(t *testing.T) {
	f := newFixture(t)
	f.host.MaxAgents = 3
	_, build := f.startHeadless(t, headlessWorkflow)
	session := f.connect(t, build.ID)
	slow := f.spawnLater(t, session, "reviewer", "slow")
	quick := f.spawnLater(t, session, "reviewer", "quick")
	f.waitStarted(t, quick)
	f.done(t, quick, "quick result")
	begun := time.Now()
	c := <-f.await(session, map[string]any{"timeout_s": 1})
	if time.Since(begun) < time.Second {
		t.Fatal("returned before the timeout")
	}
	if c.isErr || !strings.Contains(c.text, "quick result") || !strings.Contains(c.text, "Timed out. Still running: reviewer ("+slow+")") {
		t.Fatalf("await timeout: %+v", c)
	}
	f.waitStarted(t, slow)
	f.done(t, slow, "slow result")
	c = <-f.await(session, map[string]any{})
	if c.isErr || !strings.Contains(c.text, "slow result") || strings.Contains(c.text, "quick result") {
		t.Fatalf("await after timeout: %+v", c)
	}
}

func TestAwaitRefusesStrangers(t *testing.T) {
	f := newFixture(t)
	f.host.MaxAgents = 3
	_, build := f.startHeadless(t, headlessWorkflow)
	other := f.start(t)
	for _, args := range []map[string]any{
		{"helpers": []string{other.ID}},
		{"helpers": []string{"nobody"}},
		{"mode": "some"},
	} {
		if c := <-f.await(f.connect(t, build.ID), args); !c.isErr {
			t.Fatalf("await %v: %+v", args, c)
		}
	}
}

func TestInteractiveParentAwaitOrTyped(t *testing.T) {
	f := newFixture(t)
	f.host.MaxAgents = 3
	parent := f.start(t)
	session := f.connect(t, parent.ID)

	typedOne := f.spawnLater(t, session, "reviewer", "one")
	f.waitStarted(t, typedOne)
	f.done(t, typedOne, "typed in")
	if got := nextTyped(t, f.keeper); got.name != parent.TmuxSession || !strings.Contains(got.text, "Helper reviewer finished:\n\ntyped in") {
		t.Fatalf("typed %+v", got)
	}

	awaited := f.spawnLater(t, session, "reviewer", "two")
	call := f.await(session, map[string]any{"helpers": []string{awaited}})
	waitFor(t, "await to open", func() bool { return f.host.awaits(parent.ID) == 1 })
	f.waitStarted(t, awaited)
	f.done(t, awaited, "awaited")
	if c := <-call; c.isErr || !strings.Contains(c.text, "awaited") {
		t.Fatalf("await: %+v", c)
	}
	noTyping(t, f.keeper)
}

// TestDeadHelperWakesHeadlessLead reproduces run 060c55f7: a headless lead
// blocked on a helper whose session died. The sweep ends the helper and the
// lead wakes with a handoff saying so, holding the helper's last message.
func TestDeadHelperWakesHeadlessLead(t *testing.T) {
	f := newFixture(t)
	_, build := f.startHeadless(t, headlessWorkflow)
	call := f.spawn(context.Background(), t, f.connect(t, build.ID), "reviewer")
	helper := f.helperOf(t, build.ID)
	f.transcriptOf(t, helper, "two sources found so far")
	f.keeper.vanish(helper.TmuxSession)
	f.host.Sweep()
	f.host.Sweep()
	c := <-call
	want := "Helper reviewer ended without calling done:\n\n(the session exited before it called done; this is its last message)\n\ntwo sources found so far"
	if c.err != nil || c.res.IsError || resultText(c.res) != want {
		t.Fatalf("spawn: %v %q", c.err, resultText(c.res))
	}
	if a := f.agent(t, helper.ID); a.State != store.StateFailed || a.Reason != store.ReasonExited || !a.Handoff.Synthesized {
		t.Fatalf("helper: %+v", a)
	}
}

// TestStalledHelperWakesLead is 060c55f7 as it happened: the helper's
// session stayed up but its process hung mid-search.
func TestStalledHelperWakesLead(t *testing.T) {
	for _, viaAwait := range []bool{false, true} {
		f := newFixture(t)
		f.host.StallAfter = 50 * time.Millisecond
		_, build := f.startHeadless(t, headlessWorkflow)
		session := f.connect(t, build.ID)
		var helperID string
		var result func() (string, bool)
		if viaAwait {
			helperID = f.spawnLater(t, session, "reviewer", "search")
			call := f.await(session, map[string]any{})
			result = func() (string, bool) { c := <-call; return c.text, c.isErr }
		} else {
			call := f.spawn(context.Background(), t, session, "reviewer")
			helperID = f.helperOf(t, build.ID).ID
			result = func() (string, bool) { c := <-call; return resultText(c.res), c.err != nil || c.res.IsError }
		}
		f.waitStarted(t, helperID)
		f.hook(t, helperID, "UserPromptSubmit", struct{}{})
		f.host.Sweep()
		if f.state(t, helperID) != store.StateBusy {
			t.Fatal("ended before it stalled")
		}
		// The lead itself is blocked, not stalled.
		time.Sleep(80 * time.Millisecond)
		f.host.Sweep()
		if name := killed(t, f.keeper); name != f.agent(t, helperID).TmuxSession {
			t.Fatalf("killed %s", name)
		}
		text, isErr := result()
		if isErr || !strings.Contains(text, "ended without calling done:") || !strings.Contains(text, "(the helper stalled: no sign of life for") {
			t.Fatalf("lead got %q", text)
		}
		if a := f.agent(t, helperID); a.State != store.StateKilled || a.Reason != store.ReasonLost {
			t.Fatalf("helper %s/%s", a.State, a.Reason)
		}
		if a := f.agent(t, build.ID); a.State != store.StateBusy {
			t.Fatalf("lead %s", a.State)
		}
	}
}

func TestWatchdogSparesActiveAndLeads(t *testing.T) {
	f := newFixture(t)
	f.host.StallAfter = 50 * time.Millisecond
	f.host.IdleCloseAfter = -1
	run, build := f.startHeadless(t, headlessWorkflow)
	helper := f.spawnLater(t, f.connect(t, build.ID), "reviewer", "x")
	f.waitStarted(t, helper)
	f.transcriptOf(t, f.agent(t, helper), "working")
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		// The transcript keeps growing, as a busy helper's does.
		path := f.host.transcript(f.agent(t, helper))
		for {
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Millisecond):
				now := time.Now()
				_ = os.Chtimes(path, now, now)
			}
		}
	}()
	time.Sleep(120 * time.Millisecond)
	f.host.Sweep()
	noKill(t, f.keeper)
	if f.state(t, helper) != store.StateBusy || f.state(t, build.ID) != store.StateBusy || f.run(t, run.ID).State != store.StateBusy {
		t.Fatal("the watchdog ended live work")
	}
}

// TestWatchdogSparesIdleHelperWithoutHandoff: a helper idle at its prompt
// that never handed back may be asking its person something or waiting on
// a background job, so the watchdog leaves it alone however long it sits.
func TestWatchdogSparesIdleHelperWithoutHandoff(t *testing.T) {
	f := newFixture(t)
	f.host.IdleCloseAfter = time.Minute
	parent := f.start(t)
	call := f.spawn(context.Background(), t, f.connect(t, parent.ID), "reviewer")
	helper := f.helperOf(t, parent.ID)
	f.transcriptOf(t, helper, "Should I also check the tests?")
	if err := f.store.Hook(helper.ID, "Stop", time.Now().Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	f.host.Sweep()
	noKill(t, f.keeper)
	if f.state(t, helper.ID) != store.StateIdle {
		t.Fatal("idle helper without a handoff closed")
	}
	// An idle lead that never handed back stays too.
	if err := f.store.Hook(parent.ID, "Stop", time.Now().Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	f.host.Sweep()
	noKill(t, f.keeper)
	if f.state(t, parent.ID) != store.StateIdle {
		t.Fatal("idle lead closed")
	}
	f.done(t, helper.ID, "checked")
	if c := <-call; c.err != nil || !strings.Contains(resultText(c.res), "checked") {
		t.Fatalf("spawn: %v %q", c.err, resultText(c.res))
	}
}

// TestWatchdogClosesIdleResumedHelper: a helper that handed back, was
// resumed and left idle is closed, keeps its handoff, and its parent is
// sent that handoff, not a synthesized "sat idle" text.
func TestWatchdogClosesIdleResumedHelper(t *testing.T) {
	f := newFixture(t)
	f.host.IdleCloseAfter = time.Minute
	f.host.MaxAgents = 3
	_, build := f.startHeadless(t, headlessWorkflow)
	session := f.connect(t, build.ID)
	helper := f.spawnLater(t, session, "reviewer", "x")
	f.waitStarted(t, helper)
	f.done(t, helper, "delivered")
	if c := <-f.await(session, map[string]any{}); !strings.Contains(c.text, "delivered") {
		t.Fatalf("await: %q", c.text)
	}
	f.keeper.vanish(f.agent(t, helper).TmuxSession)
	if _, err := f.host.Resume(helper); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Hook(helper, "Stop", time.Now().Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	f.host.Sweep()
	got := f.agent(t, helper)
	if got.State != store.StateKilled || got.Reason != store.ReasonClosed || got.Handoff.Text != "delivered" || got.Handoff.Synthesized {
		t.Fatalf("resumed helper: %+v %+v", got, got.Handoff)
	}
	c := <-f.await(session, map[string]any{})
	if strings.Contains(c.text, "sat idle") || !strings.Contains(c.text, "delivered") {
		t.Fatalf("parent got %q", c.text)
	}
}

func TestWatchdogClosesIdleResumedAgent(t *testing.T) {
	f := newFixture(t)
	f.host.IdleCloseAfter = time.Minute
	a := f.start(t)
	f.done(t, a.ID, "delivered")
	killed(t, f.keeper)
	f.keeper.vanish(a.TmuxSession)
	if _, err := f.host.Resume(a.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Hook(a.ID, "Stop", time.Now().Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	f.host.Sweep()
	killed(t, f.keeper)
	got := f.agent(t, a.ID)
	if got.State != store.StateKilled || got.Reason != store.ReasonClosed || got.Handoff.Text != "delivered" {
		t.Fatalf("resumed: %+v %+v", got, got.Handoff)
	}
}

func TestHelperOfFinishedParentIsDropped(t *testing.T) {
	f := newFixture(t)
	f.host.MaxAgents = 3
	_, build := f.startHeadless(t, headlessWorkflow)
	helper := f.spawnLater(t, f.connect(t, build.ID), "reviewer", "x")
	f.waitStarted(t, helper)
	f.done(t, build.ID, "moved on")
	killed(t, f.keeper)
	f.done(t, helper, "too late")
	f.host.mu.Lock()
	n := len(f.host.mail)
	f.host.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d mailboxes left", n)
	}
	evs, err := f.store.Events(store.EventQuery{AgentID: helper, Kinds: []string{store.KindHandoffDropped}})
	if err != nil || len(evs) != 1 {
		t.Fatalf("events: %v %+v", err, evs)
	}
	if a := f.agent(t, helper); a.Handoff.Text != "too late" {
		t.Fatalf("helper: %+v", a)
	}
}
