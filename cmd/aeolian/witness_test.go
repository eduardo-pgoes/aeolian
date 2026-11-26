package main

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eduardo-pgoes/aeolian/internal/host"
	"github.com/eduardo-pgoes/aeolian/internal/store"
)

func TestLoadConfigWitness(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, `{"witness": {"all": true, "model": "haiku", "timeout": "5m", "max_claims": 8}}`))
	if err != nil {
		t.Fatal(err)
	}
	if w := cfg.Witness; !w.All || w.Model != "haiku" || w.Timeout != "5m" || w.MaxClaims != 8 {
		t.Fatalf("witness %+v", w)
	}
	if g := cfg.Witness.Gate(); g.Timeout != 5*time.Minute {
		t.Fatalf("gate timeout %s", g.Timeout)
	}
	for _, src := range []string{`{"witness": {"timeout": "soon"}}`, `{"witness": {"timeout": "-1m"}}`, `{"witness": {"max_claims": -1}}`} {
		if _, err := loadConfig(writeConfig(t, src)); err == nil {
			t.Errorf("%s loaded", src)
		}
	}
}

func TestWriteClaimsPutsContradictedFirst(t *testing.T) {
	var b strings.Builder
	err := writeClaims(&b, []store.Claim{
		{Key: "a", Text: "fine", Verdict: store.VerdictHolds, VerdictNote: "seen"},
		{Key: "b", Text: "pending"},
		{Key: "c", Text: "wrong\nclaim", Verdict: store.VerdictContradicted, VerdictNote: "p01 says otherwise"},
		{Key: "d", Text: "nobody looked", Verdict: store.VerdictUnwitnessed},
		{Key: "e", Text: "cannot tell", Verdict: store.VerdictUnverifiable},
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(b.String(), "\n")
	var order []string
	for _, l := range lines[1:6] {
		order = append(order, strings.Fields(l)[1])
	}
	if !slices.Equal(order, []string{"c", "e", "d", "b", "a"}) || !strings.Contains(lines[1], "wrong claim") {
		t.Fatalf("table:\n%s", b.String())
	}
	if !strings.Contains(b.String(), "5 claims: 1 contradicted, 1 unverifiable, 1 unwitnessed, 1 pending, 1 holds") {
		t.Fatalf("summary:\n%s", b.String())
	}
}

func TestParseReplayArgs(t *testing.T) {
	dir := filepath.Join("..", "..", "internal", "host", "testdata", "witness-replay")
	r, err := parseReplayArgs([]string{filepath.Join(dir, "cases")})
	if err != nil {
		t.Fatal(err)
	}
	if r.precedents != filepath.Join(dir, "precedents") || r.bar != 8 || r.parallel != 3 || !slices.Equal(r.require, []string{"w02", "w04", "w09"}) || r.match {
		t.Fatalf("args %+v", r)
	}
	r, err = parseReplayArgs([]string{"--precedents", "none", "--match", "--model", "opus", "--require", "", t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if r.precedents != host.NoPrecedents || !r.match || r.model != "opus" || r.require != nil {
		t.Fatalf("args %+v", r)
	}
	if _, err := parseReplayArgs(nil); err == nil {
		t.Fatal("no folder parsed")
	}
}
