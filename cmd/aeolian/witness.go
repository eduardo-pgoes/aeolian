package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/eduardo-pgoes/aeolian/internal/agent"
	"github.com/eduardo-pgoes/aeolian/internal/host"
	"github.com/eduardo-pgoes/aeolian/internal/keeper"
	"github.com/eduardo-pgoes/aeolian/internal/store"
)

// claims prints a run's claims with their verdicts, the ones that need a
// person first.
func claims(runID string) error {
	var cs []store.Claim
	if err := api("GET", "/api/runs/"+url.PathEscape(runID)+"/claims", nil, &cs); err != nil {
		return err
	}
	return writeClaims(os.Stdout, cs)
}

// verdictRank orders claims for a reader: what is wrong, then what nobody
// could check, then what holds.
var verdictRank = []string{store.VerdictContradicted, store.VerdictUnverifiable, store.VerdictUnwitnessed, "", store.VerdictHolds}

func writeClaims(out io.Writer, cs []store.Claim) error {
	if len(cs) == 0 {
		fmt.Fprintln(out, "no claims")
		return nil
	}
	slices.SortStableFunc(cs, func(a, b store.Claim) int {
		return slices.Index(verdictRank, a.Verdict) - slices.Index(verdictRank, b.Verdict)
	})
	counts := map[string]int{}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "VERDICT\tCLAIM\tTEXT\tNOTE")
	for _, c := range cs {
		v := c.Verdict
		if v == "" {
			v = "pending"
		}
		counts[v]++
		key := c.Key
		if key == "" {
			key = c.ID
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", v, key, clip(c.Text, 80), clip(c.VerdictNote, 100))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	var parts []string
	for _, v := range append(slices.Clone(verdictRank[:3]), "pending", store.VerdictHolds) {
		if counts[v] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[v], v))
		}
	}
	fmt.Fprintf(out, "\n%d claims: %s\n", len(cs), strings.Join(parts, ", "))
	return nil
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

type replayArgs struct {
	cases, precedents, model string
	boot                     string
	match, asJSON            bool
	parallel, bar            int
	timeout                  time.Duration
	require                  []string
}

func parseReplayArgs(args []string) (replayArgs, error) {
	var r replayArgs
	var require string
	fs := flag.NewFlagSet("witness-replay", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&r.precedents, "precedents", "", "")
	fs.StringVar(&r.model, "model", "", "")
	fs.StringVar(&r.boot, "boot", agent.BootLean, "")
	fs.BoolVar(&r.match, "match", false, "")
	fs.BoolVar(&r.asJSON, "json", false, "")
	fs.IntVar(&r.parallel, "parallel", 3, "")
	fs.IntVar(&r.bar, "bar", 8, "")
	fs.DurationVar(&r.timeout, "timeout", 10*time.Minute, "")
	fs.StringVar(&require, "require", "w02,w04,w09", "")
	if err := fs.Parse(args); err != nil {
		return r, err
	}
	if fs.NArg() != 1 {
		return r, errors.New("want one folder of cases")
	}
	r.cases = fs.Arg(0)
	if r.boot == "" {
		return r, errors.New("--boot is empty")
	}
	if err := agent.CheckBoot(r.boot); err != nil {
		return r, err
	}
	if r.precedents == "" {
		r.precedents = host.NoPrecedents
		if dir := filepath.Join(r.cases, "..", "precedents"); isDir(dir) {
			r.precedents = dir
		}
	}
	for _, s := range strings.Split(require, ",") {
		if s = strings.TrimSpace(s); s != "" {
			r.require = append(r.require, s)
		}
	}
	return r, nil
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// witnessReplay runs the witness over past corrections on a host of its
// own: a temporary AEOLIAN_HOME and a private tmux server, never the live
// host's. It prints the score and fails below the bar.
func witnessReplay(r replayArgs) error {
	cases, err := host.LoadReplayCases(r.cases)
	if err != nil {
		return err
	}
	if os.Getenv(keeper.SocketEnv) == "" {
		keeper.UsePrivateSocket()
		defer keeper.KillPrivateServer()
	}
	if keeper.Socket == keeper.DefaultSocket {
		return errors.New("witness-replay will not run on the live host's tmux socket")
	}
	home, err := os.MkdirTemp("", "aeolian-replay-")
	if err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(home, "aeolian.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	cj, err := agent.ClaudeJSON()
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	us, err := agent.UserSettingsPath()
	if err != nil {
		return err
	}
	h := &host.Host{Store: st, Layout: agent.Layout{Home: home, ClaudeJSON: cj, UserSettings: us}, Keeper: host.Tmux{}, URL: "http://" + ln.Addr().String(), MaxAgents: r.parallel,
		Boot: host.BootConfig{Roles: map[string]string{host.WitnessRole: r.boot}}}
	srv := &http.Server{Handler: h.Handler()}
	go srv.Serve(ln)
	defer srv.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go h.Watch(ctx)

	fmt.Fprintf(os.Stderr, "replaying %d cases, precedents %s, boot %s, home %s, tmux socket %s\n", len(cases), r.precedents, r.boot, home, keeper.Socket)
	c := host.WitnessConfig{Model: r.model, Precedents: r.precedents}
	results := h.Replay(ctx, cases, c, host.ReplayOptions{Parallel: r.parallel, Timeout: r.timeout, Match: r.match})
	report := host.Score(results, r.bar, r.require)
	if r.asJSON {
		if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
			return err
		}
	} else {
		writeReport(os.Stdout, report)
	}
	fmt.Fprintf(os.Stderr, "witness inputs and logs kept in %s\n", home)
	if !report.OK {
		return fmt.Errorf("replay below the bar: %d of %d passed", report.Passed, len(report.Results))
	}
	return nil
}

func writeReport(out io.Writer, r host.ReplayReport) {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "CASE\tWANT\tGOT\tPASS\tPRECEDENTS\tSTALE\tPROBLEMS")
	for _, res := range r.Results {
		pass := "ok"
		if !res.Pass {
			pass = "FAIL"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", res.Case.ID, res.Case.ExpectedVerdict, orDash(res.Verdict), pass,
			orDash(strings.Join(res.Injected, ",")), orDash(strings.Join(res.Stale, ",")), clip(strings.Join(res.Problems, "; "), 80))
	}
	w.Flush()
	fmt.Fprintln(out)
	for _, res := range r.Results {
		fmt.Fprintf(out, "%s: %s — %s\n", res.Case.ID, orDash(res.Verdict), orDash(res.Note))
		if len(res.Unmatched) > 0 {
			fmt.Fprintf(out, "  precedent_ids the matcher missed: %s\n", strings.Join(res.Unmatched, ", "))
		}
	}
	verdict := "PASS"
	if !r.OK {
		verdict = "FAIL"
	}
	fmt.Fprintf(out, "\n%s: %d of %d passed (bar %d; required %s)\n", verdict, r.Passed, len(r.Results), r.Bar, strings.Join(r.Required, ", "))
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
