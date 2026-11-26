package host

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/eduardo-pgoes/aeolian/internal/agent"
	"github.com/eduardo-pgoes/aeolian/internal/store"
)

func agentEnv(t *testing.T, l agent.Layout, id string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(l.Settings(id))
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	return s.Env
}

func assertRunEnv(t *testing.T, who string, env map[string]string) {
	t.Helper()
	if env["AYA_ITEM"] != "x" || env["GIT_CONFIG_COUNT"] != "1" ||
		env["GIT_CONFIG_KEY_0"] != "remote.origin.pushurl" || env["GIT_CONFIG_VALUE_0"] != "/nonexistent" {
		t.Fatalf("%s env: %v", who, env)
	}
}

func TestRunRefusesReservedEnv(t *testing.T) {
	f := newFixture(t)
	for _, c := range []struct {
		req  RunRequest
		want string
	}{
		{RunRequest{Env: map[string]string{"GH_TOKEN": "y"}}, "env: GH_TOKEN is reserved by aeolian"},
		{RunRequest{Env: map[string]string{"CLAUDE_CONFIG_DIR": "/c"}}, "env: CLAUDE_CONFIG_DIR is reserved by aeolian"},
		{RunRequest{Env: map[string]string{"GIT_CONFIG_COUNT": "1"}}, "env: GIT_CONFIG_COUNT is reserved by aeolian; pass git settings as git_config"},
		{RunRequest{Env: map[string]string{"GIT_CONFIG_PARAMETERS": "x"}}, "env: GIT_CONFIG_PARAMETERS is reserved by aeolian; pass git settings as git_config"},
		{RunRequest{GitConfig: [][2]string{{"", "v"}}}, "git_config: empty key"},
		{RunRequest{GitConfig: [][2]string{{"remote.origin.url", "u"}, {"pushurl", "v"}}}, "git_config: pushurl is not a git config key"},
		{RunRequest{GitConfig: [][2]string{{"bad key.x", "v"}}}, "git_config: bad key.x is not a git config key"},
		{RunRequest{GitConfig: [][2]string{{"remote.origin.pushurl", "u"}, {"Credential.https://github.com.Helper", "v"}}}, "git_config: Credential.https://github.com.Helper is set by aeolian"},
		{RunRequest{GitConfig: [][2]string{{"credential.helper", "store"}}}, "git_config: credential.helper is set by aeolian"},
	} {
		c.req.Workflow, c.req.Goal = testWorkflow, "g"
		resp := f.postRun(t, c.req)
		msg, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusBadRequest || strings.TrimSpace(string(msg)) != c.want {
			t.Errorf("%+v %v: %s %q", c.req.Env, c.req.GitConfig, resp.Status, msg)
		}
	}
	if runs, _ := f.store.Runs(store.RunFilter{}); len(runs) != 0 || f.keeper.startedCount() != 0 {
		t.Fatalf("refused run stored or started: %+v", runs)
	}
}

func TestRunEnvReachesEveryAgent(t *testing.T) {
	f := newFixture(t)
	req := RunRequest{
		Workflow:  testWorkflow,
		Goal:      "g",
		Env:       map[string]string{"AYA_ITEM": "x"},
		GitConfig: [][2]string{{"remote.origin.pushurl", "/nonexistent"}},
	}
	resp := f.postRun(t, req)
	var run store.Run
	if err := json.NewDecoder(resp.Body).Decode(&run); err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("start: %s %v", resp.Status, err)
	}
	stored := f.run(t, run.ID)
	if !reflect.DeepEqual(stored.Env, req.Env) || !reflect.DeepEqual(stored.GitConfig, req.GitConfig) ||
		!reflect.DeepEqual(run.Env, req.Env) {
		t.Fatalf("run row: %+v", stored)
	}
	events, err := f.store.Events(store.EventQuery{RunID: run.ID, Kinds: []string{store.KindRunCreated}})
	if err != nil || len(events) != 1 || !strings.Contains(string(events[0].Data), `"env":{"AYA_ITEM":"x"}`) ||
		!strings.Contains(string(events[0].Data), `"git_config":[["remote.origin.pushurl","/nonexistent"]]`) {
		t.Fatalf("run.created: %v %+v", err, events)
	}

	build := f.stepAgent(t, run.ID, "build")
	assertRunEnv(t, "step agent", agentEnv(t, f.host.Layout, build.ID))

	session := f.connect(t, build.ID)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.spawn(ctx, t, session, "reviewer")
	helper := f.helperOf(t, build.ID)
	assertRunEnv(t, "helper", agentEnv(t, f.host.Layout, helper.ID))

	f.done(t, helper.ID, "reviewed")
	killed(t, f.keeper)
	os.Remove(f.host.Layout.Settings(helper.ID))
	if code := f.post(t, "/api/agents/"+helper.ID+"/resume", nil); code != http.StatusOK {
		t.Fatalf("resume: %d", code)
	}
	assertRunEnv(t, "resumed helper", agentEnv(t, f.host.Layout, helper.ID))

	f.done(t, helper.ID, "reviewed again")
	killed(t, f.keeper)
	os.Remove(f.host.Layout.Settings(helper.ID))
	st, err := store.Open(filepath.Join(f.host.Layout.Home, "aeolian.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	restarted := &Host{Store: st, Layout: f.host.Layout, Keeper: f.keeper, URL: f.host.URL, KillGrace: time.Millisecond}
	f.srv.Config.Handler = restarted.Handler()
	if _, err := restarted.Resume(helper.ID); err != nil {
		t.Fatal(err)
	}
	assertRunEnv(t, "helper resumed after restart", agentEnv(t, f.host.Layout, helper.ID))

	late, err := restarted.launch(ctx, store.Agent{ID: "late", Dir: f.host.Layout.ScratchDir("late"), Prompt: "p", RunID: run.ID, Step: "build", ParentID: build.ID, Role: "reviewer", Headless: true})
	if err != nil {
		t.Fatal(err)
	}
	env := agentEnv(t, f.host.Layout, late.ID)
	if env["AYA_ITEM"] != "x" || env["GIT_CONFIG_COUNT"] != "3" ||
		env["GIT_CONFIG_KEY_0"] != "credential.helper" ||
		env["GIT_CONFIG_KEY_2"] != "remote.origin.pushurl" || env["GIT_CONFIG_VALUE_2"] != "/nonexistent" {
		t.Fatalf("headless helper started after restart: %v", env)
	}
}

func TestAgentOutsideRunGetsNoRunEnv(t *testing.T) {
	f := newFixture(t)
	a := f.start(t)
	if env := agentEnv(t, f.host.Layout, a.ID); env["AYA_ITEM"] != "" || env["GIT_CONFIG_COUNT"] != "" {
		t.Fatalf("env: %v", env)
	}
}
