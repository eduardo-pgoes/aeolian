package host

import (
	"context"
	"testing"

	"github.com/eduardo-pgoes/aeolian/internal/store"
)

func TestIdleLeadFreesSlot(t *testing.T) {
	f := newFixture(t)
	f.host.MaxAgents = 1
	lead := f.start(t)

	queued := make(chan store.Agent, 1)
	go func() { queued <- f.start(t) }()
	waitFor(t, "start to queue", func() bool { return f.host.queued() == 1 })

	f.hook(t, lead.ID, "Stop", struct{}{})
	second := <-queued
	if n := f.keeper.startedCount(); n != 2 {
		t.Fatalf("%d started", n)
	}

	// The lead wakes: two busy over a cap of one is allowed.
	f.hook(t, lead.ID, "UserPromptSubmit", struct{}{})
	if f.state(t, lead.ID) != store.StateBusy || f.state(t, second.ID) != store.StateBusy {
		t.Fatal("not both busy")
	}
	// A third start waits until both are idle or done.
	third := make(chan store.Agent, 1)
	go func() { third <- f.start(t) }()
	waitFor(t, "third to queue", func() bool { return f.host.queued() == 1 })
	f.hook(t, second.ID, "Stop", struct{}{})
	if f.host.queued() != 1 {
		t.Fatal("admitted with one still busy over the cap")
	}
	f.hook(t, lead.ID, "Notification", struct{}{})
	f.hook(t, lead.ID, "Stop", struct{}{})
	<-third
}

func TestIdleHelperOfSpawnDoesNotHoldSlot(t *testing.T) {
	f := newFixture(t)
	f.host.MaxAgents = 1
	parent := f.start(t)
	f.hook(t, parent.ID, "Stop", struct{}{})
	call := f.spawn(context.Background(), t, f.connect(t, parent.ID), "reviewer")
	helper := f.helperOf(t, parent.ID)
	f.hook(t, helper.ID, "Stop", struct{}{})
	other := f.start(t)
	if other.State != store.StateBusy {
		t.Fatalf("start: %+v", other)
	}
	f.done(t, helper.ID, "ok")
	if c := <-call; c.err != nil || c.res.IsError {
		t.Fatalf("spawn: %v %+v", c.err, c.res)
	}
}
