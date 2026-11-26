package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/eduardo-pgoes/aeolian/internal/digest"
	"github.com/eduardo-pgoes/aeolian/internal/host"
	"github.com/eduardo-pgoes/aeolian/internal/precedent"
)

type digestOpts struct {
	since string
	md    bool
	run   string
	max   int
	all   bool
}

func digestArgs(args []string) (digestOpts, error) {
	o := digestOpts{since: "12h", max: digest.DefaultMax}
	fs := flag.NewFlagSet("digest", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.since, "since", o.since, "")
	fs.BoolVar(&o.md, "md", false, "")
	fs.StringVar(&o.run, "run", "", "")
	fs.IntVar(&o.max, "max", o.max, "")
	fs.BoolVar(&o.all, "all", false, "")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() != 0 {
		return o, errors.New("digest takes no arguments")
	}
	if o.max < 0 {
		return o, errors.New("max must be 0 (all) or more")
	}
	if _, err := host.ParseSince(o.since, time.Now()); err != nil {
		return o, err
	}
	return o, nil
}

// digestCmd prints the return page: what needs the person from the runs
// that finished since o.since, for a terminal or, with --md, to append to
// a daily note.
func digestCmd(o digestOpts) error {
	since, _ := host.ParseSince(o.since, time.Now())
	q := url.Values{"since": {since.Format(time.RFC3339)}, "max": {strconv.Itoa(o.max)}}
	if o.run != "" {
		q.Set("run", o.run)
	}
	if o.all {
		q.Set("all", "1")
	}
	var v digest.View
	if err := api("GET", "/api/digest?"+q.Encode(), nil, &v); err != nil {
		return err
	}
	v.Since, v.Now = v.Since.Local(), v.Now.Local()
	if o.md {
		fmt.Print(digest.Markdown(v))
	} else {
		fmt.Print(digest.Text(v))
	}
	return nil
}

func digestDismiss(keys []string) error {
	var got host.DismissRequest
	if err := api("POST", "/api/digest/dismiss", host.DismissRequest{Keys: keys}, &got); err != nil {
		return err
	}
	fmt.Printf("dismissed %d; aeolian digest --all still shows them\n", len(keys))
	return nil
}

type precedentOpts struct {
	req   host.PrecedentRequest
	stale bool
}

// precedentArgs parses precedent add, list and from-claim; flags come
// before the text.
func precedentArgs(sub string, args []string) (precedentOpts, error) {
	var o precedentOpts
	fs := flag.NewFlagSet("precedent "+sub, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var scope, roles, repos string
	switch sub {
	case "list":
		fs.BoolVar(&o.stale, "stale", false, "")
	case "add", "from-claim":
		fs.StringVar(&scope, "scope", "", "")
		fs.StringVar(&roles, "role", "", "")
		fs.StringVar(&repos, "repo", "", "")
		if sub == "add" {
			fs.BoolVar(&o.req.Global, "global", false, "")
		}
		fs.StringVar(&o.req.Kind, "kind", "", "")
		fs.StringVar(&o.req.ReviewBy, "review-by", "", "")
		fs.StringVar(&o.req.Source, "source", "", "")
		fs.StringVar(&o.req.Confidence, "confidence", "", "")
	default:
		return o, fmt.Errorf("no precedent command %q", sub)
	}
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	o.req.Scope, o.req.Roles, o.req.Repos = splitList(scope), splitList(roles), splitList(repos)
	switch sub {
	case "list":
		if fs.NArg() != 0 {
			return o, errors.New("precedent list takes no arguments")
		}
	case "add":
		if fs.NArg() != 1 {
			return o, errors.New(`want the correction as one argument: aeolian precedent add --scope a,b "text"`)
		}
		if !o.req.Global && len(o.req.Scope)+len(o.req.Roles)+len(o.req.Repos) == 0 {
			return o, errors.New("give --scope (terms that must all appear), --role, --repo, or --global for every agent")
		}
		o.req.Text = fs.Arg(0)
	case "from-claim":
		if fs.NArg() != 2 {
			return o, errors.New(`want a claim id and the correction: aeolian precedent from-claim <claim-id> "what is true"`)
		}
		o.req.ClaimID, o.req.Text = fs.Arg(0), fs.Arg(1)
	}
	return o, nil
}

func splitList(s string) []string {
	var out []string
	for _, t := range strings.Split(s, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func precedentCmd(sub string, args []string) error {
	o, err := precedentArgs(sub, args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "aeolian:", err)
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch sub {
	case "list":
		dir, err := precedentsDir()
		if err != nil {
			return err
		}
		return printPrecedents(os.Stdout, host.ListPrecedents(dir, o.stale, time.Now()))
	case "add":
		// Precedents are files, so adding one needs no host.
		dir, err := precedentsDir()
		if err != nil {
			return err
		}
		now := time.Now()
		d, err := host.DraftPrecedent(o.req, nil, now)
		if err != nil {
			return err
		}
		p, err := precedent.Write(dir, d, now)
		if err != nil {
			return err
		}
		return printAdded(p)
	default:
		var p precedent.Precedent
		if err := api("POST", "/api/precedents", o.req, &p); err != nil {
			return err
		}
		return printAdded(p)
	}
}

func printAdded(p precedent.Precedent) error {
	fmt.Printf("%s\n  reaches: %s\n  review by: %s\n  edit: %s\n",
		p.ID, reach(p, ", "), p.ReviewBy.Format(precedent.DateLayout), p.Path)
	return nil
}

func reach(p precedent.Precedent, sep string) string {
	if p.Global() {
		return "every agent"
	}
	var parts []string
	if !p.Wild() {
		parts = append(parts, strings.Join(p.Scope, sep))
	}
	if len(p.Roles) > 0 {
		parts = append(parts, "role "+strings.Join(p.Roles, "|"))
	}
	if len(p.Repos) > 0 {
		parts = append(parts, "repo "+strings.Join(p.Repos, "|"))
	}
	return strings.Join(parts, "; ")
}

func printPrecedents(out io.Writer, l host.PrecedentList) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tKIND\tREVIEW BY\tREACHES\tCORRECTION")
	for _, p := range l.Precedents {
		review := p.ReviewBy.Format(precedent.DateLayout)
		if p.Stale {
			review += " (stale)"
		}
		body, _, _ := strings.Cut(p.Body, "\n")
		if r := []rune(body); len(r) > 60 {
			body = string(r[:59]) + "…"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", p.ID, p.Kind, review, reach(p.Precedent, ","), body)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	for _, e := range l.Errors {
		fmt.Fprintln(os.Stderr, "aeolian:", e)
	}
	return nil
}

// precedentsDir is the folder config.json names in precedents_dir, or the
// home's precedents folder.
func precedentsDir() (string, error) {
	l, err := layout()
	if err != nil {
		return "", err
	}
	cfg, err := loadConfig(l)
	if err != nil {
		return "", err
	}
	if cfg.PrecedentsDir != "" {
		return cfg.PrecedentsDir, nil
	}
	return precedent.Dir(l.Home), nil
}

// resolvePrecedentsDir makes a relative precedents_dir relative to home.
func resolvePrecedentsDir(home, dir string) string {
	if dir == "" || filepath.IsAbs(dir) {
		return dir
	}
	return filepath.Join(home, dir)
}
