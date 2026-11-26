package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"gorm.io/gorm"
)

// Part is one structured result an agent handed back beside its text, such
// as an artifact or a list of claims. Body is shaped by Kind; the host
// checks it before it is stored.
type Part struct {
	Kind string          `json:"kind"`
	Body json.RawMessage `json:"body"`
}

type partRow struct {
	AgentID string `gorm:"primaryKey"`
	Idx     int    `gorm:"primaryKey;autoIncrement:false"`
	Kind    string
	Body    string
}

// Text is the body as JSON text, for templates: a json.RawMessage prints
// as bytes there.
func (p Part) Text() string { return string(p.Body) }

func (partRow) TableName() string { return "handoff_parts" }

// Claim is one statement an agent made in a claims part, with the evidence
// it gave and, once a witness has looked, the verdict. ID is the store's
// key; Key is the id the agent gave the claim, unique within its part.
type Claim struct {
	ID          string     `json:"id"`
	RunID       string     `json:"run_id,omitempty"`
	AgentID     string     `json:"agent_id"`
	Idx         int        `json:"idx"`
	Key         string     `json:"key,omitempty"`
	Text        string     `json:"text"`
	Kind        string     `json:"kind,omitempty"`
	Confidence  string     `json:"confidence,omitempty"`
	Evidence    []string   `json:"evidence"`
	Verdict     string     `json:"verdict,omitempty"`
	VerdictNote string     `json:"verdict_note,omitempty"`
	WitnessID   string     `json:"witness_id,omitempty"`
	VerdictAt   *time.Time `json:"verdict_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

// Verdicts a witness gives a claim. Unwitnessed is what the host records
// when no witness answered in time.
const (
	VerdictHolds        = "holds"
	VerdictContradicted = "contradicted"
	VerdictUnverifiable = "unverifiable"
	VerdictUnwitnessed  = "unwitnessed"
)

// Verdicts lists every verdict SetVerdict accepts.
var Verdicts = []string{VerdictHolds, VerdictContradicted, VerdictUnverifiable, VerdictUnwitnessed}

// ClaimID is the store key of the claim at idx of agentID's claims.
func ClaimID(agentID string, idx int) string { return fmt.Sprintf("%s-%d", agentID, idx) }

type claimRow struct {
	ID          string `gorm:"primaryKey"`
	RunID       string `gorm:"index"`
	AgentID     string `gorm:"index"`
	Idx         int
	Key         string
	Text        string
	Kind        string
	Confidence  string
	Evidence    string
	Verdict     string `gorm:"not null;default:''"`
	VerdictNote string `gorm:"not null;default:''"`
	WitnessID   string `gorm:"not null;default:''"`
	VerdictAt   *time.Time
	CreatedAt   time.Time
}

func (claimRow) TableName() string { return "claims" }

func (s *sqliteStore) AddClaims(agentID string, cs []Claim) error {
	rows := make([]claimRow, len(cs))
	now := time.Now()
	for i, c := range cs {
		ev, err := json.Marshal(nonNil(c.Evidence))
		if err != nil {
			return err
		}
		at := c.CreatedAt
		if at.IsZero() {
			at = now
		}
		rows[i] = claimRow{
			ID: ClaimID(agentID, i), RunID: c.RunID, AgentID: agentID, Idx: i, Key: c.Key, Text: c.Text,
			Kind: c.Kind, Confidence: c.Confidence, Evidence: string(ev), CreatedAt: at,
		}
	}
	// The delete comes first: a transaction that writes before it reads
	// never trips over a stale snapshot.
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("agent_id = ?", agentID).Delete(&claimRow{}).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		return tx.Create(&rows).Error
	})
}

func (s *sqliteStore) Claims(runID string) ([]Claim, error) {
	var rows []claimRow
	if err := s.db.Where("run_id = ?", runID).Order("created_at").Order("agent_id").Order("idx").Find(&rows).Error; err != nil {
		return nil, err
	}
	return fromClaimRows(rows), nil
}

func (s *sqliteStore) AgentClaims(agentID string) ([]Claim, error) {
	var rows []claimRow
	if err := s.db.Where("agent_id = ?", agentID).Order("idx").Find(&rows).Error; err != nil {
		return nil, err
	}
	return fromClaimRows(rows), nil
}

func (s *sqliteStore) Claim(id string) (Claim, error) {
	var r claimRow
	err := s.db.First(&r, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Claim{}, ErrNotFound
	}
	if err != nil {
		return Claim{}, err
	}
	return fromClaimRow(r), nil
}

func (s *sqliteStore) SetVerdict(claimID, verdict, note, witnessID string, at time.Time) error {
	if !slices.Contains(Verdicts, verdict) {
		return fmt.Errorf("verdict %q is not one of %v", verdict, Verdicts)
	}
	res := s.db.Model(&claimRow{}).Where("id = ?", claimID).Updates(map[string]any{
		"verdict": verdict, "verdict_note": note, "witness_id": witnessID, "verdict_at": at,
	})
	if res.Error == nil && res.RowsAffected == 0 {
		return ErrNotFound
	}
	return res.Error
}

func fromClaimRows(rows []claimRow) []Claim {
	out := make([]Claim, len(rows))
	for i, r := range rows {
		out[i] = fromClaimRow(r)
	}
	return out
}

func fromClaimRow(r claimRow) Claim {
	c := Claim{
		ID: r.ID, RunID: r.RunID, AgentID: r.AgentID, Idx: r.Idx, Key: r.Key, Text: r.Text, Kind: r.Kind,
		Confidence: r.Confidence, Evidence: []string{}, Verdict: r.Verdict, VerdictNote: r.VerdictNote,
		WitnessID: r.WitnessID, VerdictAt: r.VerdictAt, CreatedAt: r.CreatedAt,
	}
	if r.Evidence != "" {
		json.Unmarshal([]byte(r.Evidence), &c.Evidence)
	}
	return c
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// partsOf loads the parts of the handoff of agentID, in order.
func partsOf(db *gorm.DB, agentID string) ([]Part, error) {
	var rows []partRow
	if err := db.Where("agent_id = ?", agentID).Order("idx").Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	parts := make([]Part, len(rows))
	for i, r := range rows {
		parts[i] = Part{Kind: r.Kind, Body: json.RawMessage(r.Body)}
	}
	return parts, nil
}
