package store

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestPartsRideOnTheHandoff(t *testing.T) {
	s := open(t)
	if err := s.CreateAgent(Agent{ID: "a1", State: StateBusy}); err != nil {
		t.Fatal(err)
	}
	parts := []Part{
		{Kind: "artifact", Body: json.RawMessage(`{"path":"/x/report.md"}`)},
		{Kind: "claims", Body: json.RawMessage(`{"claims":[{"text":"t"}]}`)},
	}
	if err := s.Done("a1", Handoff{Text: "ok", Parts: parts}, ReasonDelivered, time.Now()); err != nil {
		t.Fatal(err)
	}
	a, err := s.Agent("a1")
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Handoff.Parts) != 2 || a.Handoff.Parts[0].Kind != "artifact" || string(a.Handoff.Parts[1].Body) != `{"claims":[{"text":"t"}]}` {
		t.Fatalf("parts = %+v", a.Handoff.Parts)
	}
	all, err := s.Agents()
	if err != nil {
		t.Fatal(err)
	}
	if all[0].Handoff.Parts != nil {
		t.Fatalf("Agents loaded parts: %+v", all[0].Handoff.Parts)
	}

	// A reopened agent's second done replaces its parts.
	if err := s.Reopen("a1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Done("a1", Handoff{Text: "again", Parts: parts[:1]}, ReasonDelivered, time.Now()); err != nil {
		t.Fatal(err)
	}
	a, _ = s.Agent("a1")
	if len(a.Handoff.Parts) != 1 {
		t.Fatalf("parts after second done = %+v", a.Handoff.Parts)
	}
	if err := s.Reopen("a1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Done("a1", Handoff{Text: "none"}, ReasonDelivered, time.Now()); err != nil {
		t.Fatal(err)
	}
	a, _ = s.Agent("a1")
	if a.Handoff.Parts != nil {
		t.Fatalf("parts after a done without any = %+v", a.Handoff.Parts)
	}
}

func TestDoneAfterKillStoresNoParts(t *testing.T) {
	s := open(t)
	s.CreateAgent(Agent{ID: "a1", State: StateBusy})
	if err := s.EndAgent("a1", End{State: StateKilled, Reason: ReasonKilled}); err != nil {
		t.Fatal(err)
	}
	err := s.Done("a1", Handoff{Text: "late", Parts: []Part{{Kind: "artifact", Body: json.RawMessage(`{}`)}}}, ReasonDelivered, time.Now())
	if !errors.Is(err, ErrFinished) {
		t.Fatalf("err = %v", err)
	}
	if ps, _ := partsOf(s.(*sqliteStore).db, "a1"); ps != nil {
		t.Fatalf("parts stored: %+v", ps)
	}
}

func TestClaimsRoundTrip(t *testing.T) {
	s := open(t)
	t0 := time.Now().Add(-time.Minute)
	err := s.AddClaims("a1", []Claim{
		{RunID: "r1", Key: "c1", Text: "cap is 2", Kind: "fact", Confidence: "high", Evidence: []string{"cap.go:12"}, CreatedAt: t0},
		{RunID: "r1", Key: "c2", Text: "it is fine", Kind: "judgment", CreatedAt: t0},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddClaims("a2", []Claim{{RunID: "r1", Text: "later"}, {RunID: "r2", Text: "other run"}}); err != nil {
		t.Fatal(err)
	}
	cs, err := s.Claims("r1")
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 3 || cs[0].ID != "a1-0" || cs[1].ID != "a1-1" || cs[2].ID != "a2-0" {
		t.Fatalf("claims = %+v", cs)
	}
	if cs[0].Evidence[0] != "cap.go:12" || cs[1].Evidence == nil || len(cs[1].Evidence) != 0 || cs[0].Key != "c1" {
		t.Fatalf("claim fields = %+v", cs[:2])
	}

	at := time.Now()
	if err := s.SetVerdict("a1-0", VerdictContradicted, "it is 5 in config", "w1", at); err != nil {
		t.Fatal(err)
	}
	c, err := s.Claim("a1-0")
	if err != nil {
		t.Fatal(err)
	}
	if c.Verdict != VerdictContradicted || c.VerdictNote != "it is 5 in config" || c.WitnessID != "w1" || c.VerdictAt == nil {
		t.Fatalf("claim after verdict = %+v", c)
	}
	if err := s.SetVerdict("a1-0", "maybe", "", "w1", at); err == nil {
		t.Fatal("unknown verdict accepted")
	}
	if err := s.SetVerdict("nope-0", VerdictHolds, "", "w1", at); !errors.Is(err, ErrNotFound) {
		t.Fatalf("verdict on a missing claim: %v", err)
	}
	if _, err := s.Claim("nope-0"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing claim: %v", err)
	}

	// Adding again replaces the agent's claims, verdicts included.
	if err := s.AddClaims("a1", []Claim{{RunID: "r1", Text: "only one"}}); err != nil {
		t.Fatal(err)
	}
	mine, err := s.AgentClaims("a1")
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 1 || mine[0].Text != "only one" || mine[0].Verdict != "" {
		t.Fatalf("claims after replace = %+v", mine)
	}
	if err := s.AddClaims("a1", nil); err != nil {
		t.Fatal(err)
	}
	if mine, _ := s.AgentClaims("a1"); len(mine) != 0 {
		t.Fatalf("claims after clearing = %+v", mine)
	}
}
