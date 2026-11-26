package host

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eduardo-pgoes/aeolian/internal/agent"
	"github.com/eduardo-pgoes/aeolian/internal/store"
)

func (f *fixture) withAccounts(t *testing.T, def string, names ...string) map[string]string {
	t.Helper()
	dirs := map[string]string{}
	for _, n := range names {
		dirs[n] = t.TempDir()
	}
	f.host.Accounts, f.host.DefaultAccount = dirs, def
	return dirs
}

func configDirOf(argv []string) string {
	c := slices.Index(argv, "claude")
	for _, a := range argv[:max(c, 0)] {
		if v, ok := strings.CutPrefix(a, "CLAUDE_CONFIG_DIR="); ok {
			return v
		}
	}
	return ""
}

func (f *fixture) decodeRun(t *testing.T, resp *http.Response) store.Run {
	t.Helper()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("start run: %s %s", resp.Status, b)
	}
	var run store.Run
	if err := json.NewDecoder(resp.Body).Decode(&run); err != nil {
		t.Fatal(err)
	}
	return run
}

func TestRunAccountReachesEveryLaunch(t *testing.T) {
	f := newFixture(t)
	f.withClaudeJSON(t)
	dirs := f.withAccounts(t, "home", "home", "work")
	if err := os.WriteFile(filepath.Join(dirs["work"], "settings.json"), []byte(`{"model":"opus","outputStyle":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	src := strings.Replace(testWorkflow, "    helpers: [reviewer]\n", "    helpers: [reviewer]\n    boot: lean\n", 1)
	run := f.decodeRun(t, f.postRun(t, RunRequest{Workflow: src, Goal: "g", Account: "work"}))
	if run.Account != "work" || f.run(t, run.ID).Account != "work" {
		t.Fatalf("account not on the run: %+v", run)
	}
	build := f.stepAgent(t, run.ID, "build")
	argv := f.keeper.argv[0]
	if got := configDirOf(argv); got != dirs["work"] {
		t.Fatalf("CLAUDE_CONFIG_DIR %q, want %q: %q", got, dirs["work"], argv)
	}
	if promptOf(t, argv) != "Build g." {
		t.Fatalf("prompt moved: %q", argv)
	}
	if !trusted(t, agent.ClaudeJSONIn(dirs["work"]), build.Dir) || trusted(t, f.host.Layout.ClaudeJSON, build.Dir) {
		t.Fatal("trust went to the wrong .claude.json")
	}
	b, err := os.ReadFile(f.host.Layout.Settings(build.ID))
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	json.Unmarshal(b, &settings)
	if settings["model"] != "opus" || settings["outputStyle"] != "default" {
		t.Fatalf("lean boot did not carry the account's settings: %s", b)
	}

	if err := f.store.EndAgent(build.ID, store.End{State: store.StateKilled, Reason: store.ReasonKilled, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	restarted := &Host{Store: f.store, Layout: f.host.Layout, Keeper: f.keeper, URL: f.host.URL, Accounts: f.host.Accounts}
	if _, err := restarted.Resume(build.ID); err != nil {
		t.Fatal(err)
	}
	resumed := f.keeper.argv[len(f.keeper.argv)-1]
	if !slices.Contains(resumed, "--resume") || configDirOf(resumed) != dirs["work"] {
		t.Fatalf("resume after restart lost the account: %q", resumed)
	}
}

func TestRunAccountDefaultAndUnknown(t *testing.T) {
	f := newFixture(t)
	dirs := f.withAccounts(t, "home", "home", "work")
	run := f.decodeRun(t, f.startRun(t, testWorkflow, "g"))
	if run.Account != "home" {
		t.Fatalf("account %q, want the default", run.Account)
	}
	f.stepAgent(t, run.ID, "build")
	if got := configDirOf(f.keeper.argv[0]); got != dirs["home"] {
		t.Fatalf("CLAUDE_CONFIG_DIR %q, want %q", got, dirs["home"])
	}

	f.host.Accounts["host"] = ""
	own := f.decodeRun(t, f.postRun(t, RunRequest{Workflow: testWorkflow, Goal: "own", Account: "host"}))
	f.stepAgent(t, own.ID, "build")
	if argv := f.keeper.argv[len(f.keeper.argv)-1]; own.Account != "host" || configDirOf(argv) != "" {
		t.Fatalf("empty account dir should keep the host config: %+v %q", own, argv)
	}
	delete(f.host.Accounts, "host")

	resp := f.postRun(t, RunRequest{Workflow: testWorkflow, Goal: "g2", Account: "nope"})
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(b), `account "nope" is not one of home, work`) {
		t.Fatalf("unknown account: %s %s", resp.Status, b)
	}
	runs, _ := f.store.Runs(store.RunFilter{})
	if len(runs) != 2 {
		t.Fatalf("refused run was stored: %d runs", len(runs))
	}
	if _, err := f.host.StartArena(ArenaRequest{Question: "q?", Account: "nope"}); !errors.Is(err, ErrUnknownAccount) {
		t.Fatalf("arena with unknown account: %v", err)
	}
}

func TestNoAccountsKeepsHostConfig(t *testing.T) {
	f := newFixture(t)
	run := f.decodeRun(t, f.startRun(t, testWorkflow, "g"))
	if run.Account != "" {
		t.Fatalf("account %q without accounts", run.Account)
	}
	f.stepAgent(t, run.ID, "build")
	if argv := f.keeper.argv[0]; configDirOf(argv) != "" || slices.Contains(argv, "env") {
		t.Fatalf("argv sets a config dir without accounts: %q", argv)
	}
	resp := f.postRun(t, RunRequest{Workflow: testWorkflow, Goal: "g2", Account: "work"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("named account without accounts: %s", resp.Status)
	}
}

func TestTranscriptUsesRunAccount(t *testing.T) {
	f := newFixture(t)
	dirs := f.withAccounts(t, "", "work")
	f.host.Transcripts = t.TempDir()
	for _, r := range []store.Run{{ID: "r-work", Goal: "g", Account: "work"}, {ID: "r-host", Goal: "g"}} {
		if err := f.store.CreateRun(r); err != nil {
			t.Fatal(err)
		}
	}
	write := func(root, sid string) string {
		p := filepath.Join(root, "-proj", sid+".jsonl")
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	work := write(agent.TranscriptsIn(dirs["work"]), "s-work")
	write(f.host.Transcripts, "s-work")
	host := write(f.host.Transcripts, "s-host")
	if got := f.host.transcript(store.Agent{RunID: "r-work", SessionID: "s-work"}); got != work {
		t.Fatalf("account transcript %q, want %q", got, work)
	}
	if got := f.host.transcript(store.Agent{RunID: "r-host", SessionID: "s-host"}); got != host {
		t.Fatalf("host transcript %q, want %q", got, host)
	}
	if got := f.host.transcript(store.Agent{SessionID: "s-host"}); got != host {
		t.Fatalf("lone agent transcript %q, want %q", got, host)
	}
}

func TestGCUntrustsInRunAccount(t *testing.T) {
	f := newFixture(t)
	cj := f.withClaudeJSON(t)
	dirs := f.withAccounts(t, "", "work")
	at := time.Now().Add(-30 * 24 * time.Hour)
	if err := f.store.CreateRun(store.Run{ID: "old", Goal: "g", Account: "work", CreatedAt: at, Steps: []store.Step{{Name: "s"}}}); err != nil {
		t.Fatal(err)
	}
	a := store.Agent{ID: "agent-old", RunID: "old", Step: "s", Dir: f.host.Layout.ScratchDir("agent-old"), State: store.StateBusy}
	os.MkdirAll(a.Dir, 0o755)
	for _, err := range []error{
		f.store.CreateAgent(a),
		f.store.StartStep("old", 0, a.ID, at),
		f.store.EndAgent(a.ID, store.End{State: store.StateDone, Reason: store.ReasonDelivered, At: at}),
		f.store.FinishStep("old", 0, store.End{State: store.StateDone, At: at}),
		f.store.FinishRun("old", store.End{State: store.StateDone, At: at}),
		f.store.Ack("old", at),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	work := agent.ClaudeJSONIn(dirs["work"])
	for _, p := range []string{cj, work} {
		if err := agent.Trust(p, a.Dir); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := f.host.GC(time.Now(), false)
	if err != nil || rep.Untrusted != 1 {
		t.Fatalf("gc: %+v %v", rep, err)
	}
	if trusted(t, work, a.Dir) || !trusted(t, cj, a.Dir) {
		t.Fatal("untrusted in the wrong .claude.json")
	}
}

func TestListAccounts(t *testing.T) {
	f := newFixture(t)
	f.withAccounts(t, "home", "work", "home")
	resp, err := http.Get(f.srv.URL + "/api/accounts")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var v AccountsView
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(v.Accounts, []string{"home", "work"}) || v.Default != "home" {
		t.Fatalf("accounts: %+v", v)
	}
}
