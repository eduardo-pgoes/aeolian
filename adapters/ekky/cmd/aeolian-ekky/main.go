package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/eduardo-pgoes/aeolian/adapters/ekky/internal/qa"
)

const defaultAddr = "127.0.0.1:7431"

const usage = `usage:
  aeolian-ekky          poll ekky until stopped and start a QA run for each work task that reaches done
  aeolian-ekky due      print the QA runs the next poll would start, and start none

reads $AEOLIAN_HOME/ekky-adapter.json (optional) and the repos and accounts of
$AEOLIAN_HOME/config.json; the host is at $AEOLIAN_ADDR, or 127.0.0.1:7431
`

type Config struct {
	EkkyRoot   string `json:"ekky_root"`
	Workflow   string `json:"workflow"`
	BundleRoot string `json:"bundle_root"`
	Interval   string `json:"interval"`
}

type hostConfig struct {
	Repos map[string]struct {
		Path string `json:"path"`
	} `json:"repos"`
	Accounts map[string]string `json:"accounts"`
}

func main() {
	dry := false
	switch {
	case len(os.Args) == 2 && os.Args[1] == "due":
		dry = true
	case len(os.Args) > 1:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err := run(dry); err != nil {
		fmt.Fprintln(os.Stderr, "aeolian-ekky:", err)
		os.Exit(1)
	}
}

func home() (string, error) {
	if h := os.Getenv("AEOLIAN_HOME"); h != "" {
		return filepath.Abs(h)
	}
	u, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(u, ".local", "share", "aeolian"), nil
}

func addr() (string, error) {
	a := os.Getenv("AEOLIAN_ADDR")
	if a == "" {
		a = defaultAddr
	}
	h, _, err := net.SplitHostPort(a)
	if err != nil {
		return "", fmt.Errorf("AEOLIAN_ADDR: %w", err)
	}
	if ip := net.ParseIP(h); ip == nil || !ip.IsLoopback() {
		return "", fmt.Errorf("AEOLIAN_ADDR %q is not a loopback IP address", a)
	}
	return a, nil
}

func watcher(h, a string) (*qa.Watcher, error) {
	u, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	cfg := Config{
		EkkyRoot:   filepath.Join(u, "ekky"),
		Workflow:   filepath.Join(h, "workflows", "qa-ekky.yaml"),
		BundleRoot: filepath.Join(h, "qa-runs"),
		Interval:   "30s",
	}
	if v := os.Getenv("EKKY_ROOT"); v != "" {
		cfg.EkkyRoot = v
	}
	path := filepath.Join(h, "ekky-adapter.json")
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &cfg); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	interval, err := time.ParseDuration(cfg.Interval)
	if err != nil || interval < 5*time.Second {
		return nil, fmt.Errorf("%s: interval must be a duration of at least 5s", path)
	}
	var hc hostConfig
	hpath := filepath.Join(h, "config.json")
	b, err := os.ReadFile(hpath)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &hc); err != nil {
		return nil, fmt.Errorf("%s: %w", hpath, err)
	}
	w := &qa.Watcher{
		HostURL: "http://" + a, EkkyRoot: cfg.EkkyRoot, Workflow: cfg.Workflow, BundleRoot: cfg.BundleRoot,
		WorkRepos: map[string]bool{}, Accounts: map[string]bool{}, Interval: interval, Logf: log.Printf,
	}
	for _, r := range hc.Repos {
		if r.Path != "" {
			w.WorkRepos[filepath.Clean(r.Path)] = true
		}
	}
	for name := range hc.Accounts {
		w.Accounts[name] = true
	}
	if len(w.WorkRepos) == 0 {
		return nil, fmt.Errorf("%s lists no repos, so no ekky task counts as work", hpath)
	}
	return w, nil
}

func run(dry bool) error {
	h, err := home()
	if err != nil {
		return err
	}
	a, err := addr()
	if err != nil {
		return err
	}
	w, err := watcher(h, a)
	if err != nil {
		return err
	}
	if dry {
		tasks, err := qa.Scan(w.EkkyRoot)
		if err != nil {
			return err
		}
		existing, err := w.Existing(context.Background())
		if err != nil {
			return err
		}
		for _, s := range w.Due(tasks, existing) {
			fmt.Println(s.Ref)
		}
		return nil
	}
	if _, err := os.Stat(w.Workflow); err != nil {
		return fmt.Errorf("workflow: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("aeolian-ekky for the host on http://%s: watching %s every %s for work tasks done, %d work repos, running %s", a, w.EkkyRoot, w.Interval, len(w.WorkRepos), w.Workflow)
	w.Poll(ctx)
	return nil
}
