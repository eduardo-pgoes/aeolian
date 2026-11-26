package host

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/eduardo-pgoes/aeolian/internal/agent"
	"github.com/eduardo-pgoes/aeolian/internal/keeper"
	"github.com/eduardo-pgoes/aeolian/internal/store"
)

type fakeKeeper struct {
	mu      sync.Mutex
	started []string
	dirs    []string
	argv    [][]string
	dead    map[string]bool
	listErr error
	screens map[string]string
	killed  chan string
	typed   chan typed
}

type typed struct{ name, text string }

func (k *fakeKeeper) Type(name, text string) error {
	k.typed <- typed{name, text}
	return nil
}

func (k *fakeKeeper) Start(name, dir string, argv []string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.started = append(k.started, name)
	delete(k.dead, name)
	k.dirs = append(k.dirs, dir)
	k.argv = append(k.argv, argv)
	return nil
}

func (k *fakeKeeper) Kill(name string) error {
	k.mu.Lock()
	if k.dead == nil {
		k.dead = map[string]bool{}
	}
	k.dead[name] = true
	k.mu.Unlock()
	k.killed <- name
	return nil
}

func (k *fakeKeeper) List() ([]keeper.Session, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.listErr != nil {
		return nil, k.listErr
	}
	var sessions []keeper.Session
	for _, name := range k.started {
		if !k.dead[name] {
			sessions = append(sessions, keeper.Session{Name: name, Cols: 120, Rows: 40})
		}
	}
	return sessions, nil
}

func (k *fakeKeeper) Capture(name string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.dead[name] || !slices.Contains(k.started, name) {
		return "", keeper.ErrNoSession
	}
	return k.screens[name], nil
}

func (k *fakeKeeper) setScreen(name, screen string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.screens == nil {
		k.screens = map[string]string{}
	}
	k.screens[name] = screen
}

type fixture struct {
	host   *Host
	keeper *fakeKeeper
	store  store.Store
	srv    *httptest.Server
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	home := t.TempDir()
	st, err := store.Open(filepath.Join(home, "aeolian.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	t.Cleanup(func() { assertFolds(t, st) })
	k := &fakeKeeper{killed: make(chan string, 8), typed: make(chan typed, 8)}
	h := &Host{Store: st, Layout: agent.Layout{Home: home}, Keeper: k, KillGrace: time.Millisecond}
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)
	h.URL = srv.URL
	return &fixture{host: h, keeper: k, store: st, srv: srv}
}

// assertFolds fails t unless the event log folds back into exactly the
// tables: every host test then also proves no change escaped the log.
func assertFolds(t *testing.T, st store.Store) {
	t.Helper()
	diff, err := store.FoldDiff(st)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range diff {
		t.Errorf("fold: %s", d)
	}
}

func (f *fixture) start(t *testing.T) store.Agent {
	t.Helper()
	resp, err := http.Post(f.srv.URL+"/api/agents", "application/json", strings.NewReader(`{"prompt":"fix it"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("start: %s", resp.Status)
	}
	var a store.Agent
	if err := json.NewDecoder(resp.Body).Decode(&a); err != nil {
		t.Fatal(err)
	}
	return a
}

func (f *fixture) connect(t *testing.T, agentID string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: agent.MCPURL(f.srv.URL, agentID)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func (f *fixture) done(t *testing.T, agentID, text string, worktrees ...store.Worktree) {
	t.Helper()
	if worktrees == nil {
		worktrees = []store.Worktree{}
	}
	res, err := f.connect(t, agentID).CallTool(context.Background(), &mcp.CallToolParams{Name: "done", Arguments: map[string]any{
		"text": text, "worktrees": worktrees,
	}})
	if err != nil || res.IsError {
		t.Fatalf("done %s: %v %+v", agentID, err, res)
	}
}

func (k *fakeKeeper) startedCount() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.started)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func (h *Host) queued() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.queue)
}

func TestStartsOverCapWaitInOrder(t *testing.T) {
	f := newFixture(t)
	f.host.MaxAgents = 1
	first := f.start(t)

	started := make(chan store.Agent, 2)
	for i := range 2 {
		go func() { started <- f.start(t) }()
		waitFor(t, "start to queue", func() bool { return f.host.queued() == i+1 })
	}
	time.Sleep(20 * time.Millisecond)
	if n := f.keeper.startedCount(); n != 1 {
		t.Fatalf("%d agents started over a cap of 1", n)
	}

	f.done(t, first.ID, "ok")
	second := <-started
	if n := f.keeper.startedCount(); n != 2 || f.host.queued() != 1 {
		t.Fatalf("after first done: %d started, %d queued", n, f.host.queued())
	}
	f.done(t, second.ID, "ok")
	third := <-started
	if !slices.Equal(f.keeper.started, []string{first.TmuxSession, second.TmuxSession, third.TmuxSession}) {
		t.Fatalf("started out of order: %v", f.keeper.started)
	}
}

func TestStartThroughAPI(t *testing.T) {
	f := newFixture(t)
	a := f.start(t)
	if a.State != store.StateBusy || a.TmuxSession != agent.SessionName(a.ID) {
		t.Fatalf("agent: %+v", a)
	}
	if !slices.Equal(f.keeper.started, []string{a.TmuxSession}) {
		t.Fatalf("started: %v", f.keeper.started)
	}
	if argv := f.keeper.argv[0]; promptOf(t, argv) != "fix it" {
		t.Fatalf("argv: %q", argv)
	}

	resp, err := http.Get(f.srv.URL + "/api/agents")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var list []store.Agent
	json.NewDecoder(resp.Body).Decode(&list)
	if len(list) != 1 || list[0].ID != a.ID {
		t.Fatalf("list: %+v", list)
	}
}

func TestHooksFlipBusyIdle(t *testing.T) {
	f := newFixture(t)
	a := f.start(t)
	for _, step := range []struct{ event, want string }{
		{"Stop", store.StateIdle},
		{"UserPromptSubmit", store.StateBusy},
	} {
		resp, err := http.Post(f.srv.URL+"/hooks/"+a.ID+"/"+step.event, "application/json", strings.NewReader(`{"hook_event_name":"x"}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("%s: %s", step.event, resp.Status)
		}
		got, _ := f.store.Agent(a.ID)
		if got.State != step.want {
			t.Fatalf("after %s: %q", step.event, got.State)
		}
	}

	resp, err := http.Post(f.srv.URL+"/hooks/nobody/Stop", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown agent hook: %s", resp.Status)
	}
}

func TestDoneRecordsHandoffThenKills(t *testing.T) {
	f := newFixture(t)
	a := f.start(t)
	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: agent.MCPURL(f.srv.URL, a.ID)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "done", Arguments: map[string]any{
		"text":      "fixed",
		"worktrees": []map[string]string{{"repo": "/r", "path": "/r/wt", "branch": "fix"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("done failed: %+v", res.Content[0])
	}

	select {
	case name := <-f.keeper.killed:
		if name != a.TmuxSession {
			t.Fatalf("killed %q", name)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("session not killed")
	}
	got, err := f.store.Agent(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := store.Handoff{Text: "fixed", Worktrees: []store.Worktree{{Repo: "/r", Path: "/r/wt", Branch: "fix"}}}
	if got.State != store.StateDone || got.Handoff == nil || got.Handoff.Text != want.Text || !slices.Equal(got.Handoff.Worktrees, want.Worktrees) {
		t.Fatalf("agent: %+v", got)
	}
}

func TestMCPSurvivesHostRestart(t *testing.T) {
	f := newFixture(t)
	a := f.start(t)
	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: agent.MCPURL(f.srv.URL, a.ID)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	restarted := &Host{Store: f.store, Layout: f.host.Layout, Keeper: f.keeper, URL: f.host.URL, KillGrace: time.Millisecond}
	f.srv.Config.Handler = restarted.Handler()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "done", Arguments: map[string]any{"text": "ok", "worktrees": []any{}}})
	if err != nil || res.IsError {
		t.Fatalf("done after restart: %v %+v", err, res)
	}
	<-f.keeper.killed
}

func TestMCPUnknownAgent(t *testing.T) {
	f := newFixture(t)
	resp, err := http.Post(f.srv.URL+"/mcp/nobody", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %s", resp.Status)
	}
}
