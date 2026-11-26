package host

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/eduardo-pgoes/aeolian/internal/agent"
	"github.com/eduardo-pgoes/aeolian/internal/store"
)

type spawnCall struct {
	res *mcp.CallToolResult
	err error
}

func (f *fixture) spawn(ctx context.Context, t *testing.T, session *mcp.ClientSession, role string, worktrees ...store.Worktree) <-chan spawnCall {
	t.Helper()
	if worktrees == nil {
		worktrees = []store.Worktree{}
	}
	params := &mcp.CallToolParams{Name: "spawn", Arguments: map[string]any{
		"role":               role,
		"handoff":            map[string]any{"text": "review the fix", "worktrees": worktrees},
		"extra_instructions": "be brief",
	}}
	params.SetProgressToken("spawn-1")
	out := make(chan spawnCall, 1)
	go func() {
		res, err := session.CallTool(ctx, params)
		out <- spawnCall{res, err}
	}()
	return out
}

func (f *fixture) helperOf(t *testing.T, parentID string) store.Agent {
	t.Helper()
	var helper store.Agent
	waitFor(t, "helper to start", func() bool {
		agents, _ := f.store.Agents()
		for _, a := range agents {
			if a.ParentID == parentID {
				helper = a
				f.keeper.mu.Lock()
				defer f.keeper.mu.Unlock()
				return slices.Contains(f.keeper.started, a.TmuxSession)
			}
		}
		return false
	})
	return helper
}

func resultText(res *mcp.CallToolResult) string {
	if len(res.Content) == 0 {
		return ""
	}
	return res.Content[0].(*mcp.TextContent).Text
}

func TestSpawnReturnsHelperHandoff(t *testing.T) {
	f := newFixture(t)
	f.host.MaxAgents = 1
	f.host.ProgressInterval = 5 * time.Millisecond
	parent := f.start(t)
	wt := store.Worktree{Repo: "/r", Path: t.TempDir(), Branch: "fix"}

	var progress atomic.Int32
	client := mcp.NewClient(&mcp.Implementation{Name: "test"}, &mcp.ClientOptions{
		ProgressNotificationHandler: func(context.Context, *mcp.ProgressNotificationClientRequest) { progress.Add(1) },
	})
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: agent.MCPURL(f.srv.URL, parent.ID)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	call := f.spawn(context.Background(), t, session, "reviewer", wt)

	helper := f.helperOf(t, parent.ID)
	if helper.Dir != wt.Path || helper.Role != "reviewer" || f.keeper.dirs[1] != wt.Path {
		t.Fatalf("helper %+v started in %q", helper, f.keeper.dirs[1])
	}
	for _, want := range []string{"reviewer helper", "review the fix", wt.Path, "be brief"} {
		if !strings.Contains(helper.Prompt, want) {
			t.Fatalf("helper prompt lacks %q: %q", want, helper.Prompt)
		}
	}
	waitFor(t, "progress", func() bool { return progress.Load() >= 3 })
	select {
	case c := <-call:
		t.Fatalf("spawn returned before the helper was done: %+v %v", c.res, c.err)
	default:
	}

	f.done(t, helper.ID, "looks good", wt)
	c := <-call
	if c.err != nil || c.res.IsError {
		t.Fatalf("spawn: %v %+v", c.err, c.res)
	}
	text := resultText(c.res)
	if !strings.HasPrefix(text, "Helper reviewer finished:") || !strings.Contains(text, "looks good") || !strings.Contains(text, wt.Path) {
		t.Fatalf("spawn result: %q", text)
	}
}

func TestBlockedParentDoesNotHoldSlot(t *testing.T) {
	f := newFixture(t)
	f.host.MaxAgents = 1
	parent := f.start(t)
	call := f.spawn(context.Background(), t, f.connect(t, parent.ID), "reviewer", store.Worktree{Path: t.TempDir()})
	helper := f.helperOf(t, parent.ID)

	queued := make(chan store.Agent, 1)
	go func() { queued <- f.start(t) }()
	waitFor(t, "start to queue behind the helper", func() bool { return f.host.queued() == 1 })

	f.done(t, helper.ID, "ok")
	<-call
	time.Sleep(20 * time.Millisecond)
	if n := f.keeper.startedCount(); n != 2 || f.host.queued() != 1 {
		t.Fatalf("parent back from spawn left its slot: %d started, %d queued", n, f.host.queued())
	}
	f.done(t, parent.ID, "ok")
	<-queued
}

func TestSpawnWithoutWorktreeStartsInScratch(t *testing.T) {
	f := newFixture(t)
	parent := f.start(t)
	call := f.spawn(context.Background(), t, f.connect(t, parent.ID), "researcher")
	helper := f.helperOf(t, parent.ID)
	if helper.Dir != f.host.Layout.ScratchDir(helper.ID) {
		t.Fatalf("helper started in %q", helper.Dir)
	}
	f.done(t, helper.ID, "found it")
	if c := <-call; c.err != nil || c.res.IsError || !strings.Contains(resultText(c.res), "found it") {
		t.Fatalf("spawn: %v %+v", c.err, c.res)
	}
}

func TestSpawnWithoutWaitReturnsAtOnce(t *testing.T) {
	f := newFixture(t)
	f.host.MaxAgents = 3
	parent := f.start(t)
	session := f.connect(t, parent.ID)
	for _, role := range []string{"a", "b"} {
		res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "spawn", Arguments: map[string]any{
			"role": role, "handoff": map[string]any{"text": "go", "worktrees": []store.Worktree{}}, "wait": false,
		}})
		if err != nil || res.IsError || !strings.Contains(resultText(res), "Helper "+role+" is starting") {
			t.Fatalf("spawn %s: %v %+v", role, err, res)
		}
	}
	waitFor(t, "both helpers to start", func() bool { return f.keeper.startedCount() == 3 })
	f.host.mu.Lock()
	waiting := len(f.host.spawns)
	f.host.mu.Unlock()
	if waiting != 0 {
		t.Fatalf("%d spawns still waiting", waiting)
	}

	agents, _ := f.store.Agents()
	for _, a := range agents {
		if a.ParentID != parent.ID {
			continue
		}
		f.done(t, a.ID, "did "+a.Role)
		select {
		case got := <-f.keeper.typed:
			if got.name != parent.TmuxSession || !strings.HasPrefix(got.text, "Helper "+a.Role+" finished:") || !strings.Contains(got.text, "did "+a.Role) {
				t.Fatalf("typed %q into %s", got.text, got.name)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("handoff of %s not typed into the parent", a.Role)
		}
	}
}

func TestCancelledSpawnHandsBackThroughTerminal(t *testing.T) {
	f := newFixture(t)
	parent := f.start(t)
	wt := store.Worktree{Path: t.TempDir()}
	ctx, cancel := context.WithCancel(context.Background())
	call := f.spawn(ctx, t, f.connect(t, parent.ID), "reviewer", wt)
	helper := f.helperOf(t, parent.ID)

	cancel()
	if c := <-call; c.err == nil {
		t.Fatalf("cancelled spawn returned %+v", c.res)
	}
	waitFor(t, "parent to stop waiting", func() bool {
		f.host.mu.Lock()
		defer f.host.mu.Unlock()
		return len(f.host.spawns) == 0
	})
	select {
	case name := <-f.keeper.killed:
		t.Fatalf("killed %s on cancel", name)
	case <-time.After(20 * time.Millisecond):
	}

	f.done(t, helper.ID, "looks good", wt)
	select {
	case got := <-f.keeper.typed:
		if got.name != parent.TmuxSession || !strings.HasPrefix(got.text, "Helper reviewer finished:\n") || !strings.Contains(got.text, "looks good") {
			t.Fatalf("typed %q into %s", got.text, got.name)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handoff not typed into the parent")
	}
}

func TestHelperDoneAfterRestartHandsBack(t *testing.T) {
	f := newFixture(t)
	parent := f.start(t)
	session := f.connect(t, parent.ID)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	f.spawn(ctx, t, session, "reviewer", store.Worktree{Path: t.TempDir()})
	helper := f.helperOf(t, parent.ID)

	restarted := &Host{Store: f.store, Layout: f.host.Layout, Keeper: f.keeper, URL: f.host.URL, KillGrace: time.Millisecond}
	f.srv.Config.Handler = restarted.Handler()
	f.done(t, helper.ID, "looks good")
	select {
	case got := <-f.keeper.typed:
		if got.name != parent.TmuxSession || !strings.HasPrefix(got.text, "Helper reviewer finished:") {
			t.Fatalf("typed %q into %s", got.text, got.name)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handoff not typed into the parent")
	}
}

func TestCancelledNotificationEndsMatchingCall(t *testing.T) {
	h := &Host{}
	token, closeCall := h.openCall(context.Background(), "a1", json.RawMessage("7"))
	defer closeCall()
	other, closeOther := h.openCall(context.Background(), "a1", json.RawMessage("8"))
	defer closeOther()

	h.cancelCall("a2", json.RawMessage("7"))
	h.cancelCall("a1", json.RawMessage(" 7 "))
	select {
	case <-h.callDone(token):
	default:
		t.Fatal("call 7 of a1 not cancelled")
	}
	select {
	case <-h.callDone(other):
		t.Fatal("call 8 cancelled too")
	default:
	}
}
