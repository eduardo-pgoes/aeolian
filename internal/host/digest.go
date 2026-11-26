package host

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/eduardo-pgoes/aeolian/internal/digest"
	"github.com/eduardo-pgoes/aeolian/internal/precedent"
	"github.com/eduardo-pgoes/aeolian/internal/store"
)

// DefaultDigestSince is how far back a digest looks unless asked.
const DefaultDigestSince = 12 * time.Hour

// deadReasons are the reasons a run or helper ends without delivering and
// without anyone deciding it should. A helper the watchdog stopped as
// stalled ends lost, so it is here; one it closed idle ends closed, with
// its last message as its handoff, so it is not. A run killed or ended on
// purpose is not here: the person already knows. Every failed run is dead
// whatever its reason, which covers an arena's too_few_entries,
// no_judgments and orphaned.
var deadReasons = []string{store.ReasonLost, store.ReasonOOM, store.ReasonExited, store.ReasonStartFailed, store.ReasonBadWorkflow}

func init() {
	extraRoutes = append(extraRoutes, func(h *Host, mux *http.ServeMux) {
		mux.HandleFunc("GET /api/digest", h.getDigest)
		mux.HandleFunc("POST /api/digest/dismiss", h.dismissDigest)
	})
}

// DigestQuery selects a digest: the runs finished since Since, or only run
// RunID whenever it finished, at most Max items (0 means all).
type DigestQuery struct {
	Since time.Time
	RunID string
	Max   int
	All   bool
}

// Digest gathers what needs the person from the runs q selects: blocked
// questions, runs and helpers that died, claims a witness contradicted,
// could not verify or never answered, and stale or broken precedents.
func (h *Host) Digest(q DigestQuery) (digest.View, error) {
	now := time.Now()
	in := digest.Input{Since: q.Since, Now: now, RunID: q.RunID, ShowDismissed: q.All}
	runs, err := h.Store.Runs(store.RunFilter{})
	if err != nil {
		return digest.View{}, err
	}
	var picked []store.Run
	for _, r := range runs {
		switch {
		case q.RunID != "":
			if r.ID == q.RunID && r.DoneAt != nil {
				picked = append(picked, r)
			}
		case r.DoneAt == nil:
			in.Running++
		case !r.DoneAt.Before(q.Since):
			picked = append(picked, r)
		}
	}
	applied, stale, staleAt, err := h.precedentEvents(q, picked)
	if err != nil {
		return digest.View{}, err
	}
	var helpers map[string][]store.Agent
	if len(picked) > 0 {
		ids := make([]string, len(picked))
		for i, r := range picked {
			ids[i] = r.ID
		}
		agents, err := h.Store.AgentsOfRuns(ids)
		if err != nil {
			return digest.View{}, err
		}
		helpers = map[string][]store.Agent{}
		for _, a := range agents {
			if a.ParentID != "" && a.RunID != "" {
				helpers[a.RunID] = append(helpers[a.RunID], a)
			}
		}
	}
	dismissed, err := h.digestDismissals()
	if err != nil {
		return digest.View{}, err
	}
	for _, r := range picked {
		in.Runs = append(in.Runs, digest.Run{
			ID: r.ID, Goal: r.Goal, State: r.State, Reason: r.Reason, Source: r.Source, StartedAt: r.CreatedAt,
			FinishedAt: *r.DoneAt, Precedents: applied[r.ID],
		})
		items, err := h.runItems(r, helpers[r.ID])
		if err != nil {
			return digest.View{}, err
		}
		for i := range items {
			items[i].Dismissed = hidden(items[i], dismissed) || (r.Source == "" && r.AckedAt != nil && !r.AckedAt.Before(items[i].At))
		}
		in.Items = append(in.Items, items...)
	}
	for _, it := range h.precedentItems(stale, staleAt, q.RunID == "", now) {
		it.Dismissed = hidden(it, dismissed)
		in.Items = append(in.Items, it)
	}
	return digest.Build(in, q.Max), nil
}

func hidden(it digest.Item, dismissed map[string]time.Time) bool {
	at, ok := dismissed[it.Key]
	return ok && !at.Before(it.At)
}

func (h *Host) digestDismissals() (map[string]time.Time, error) {
	out := map[string]time.Time{}
	for after := int64(0); ; {
		evs, err := h.Store.Events(store.EventQuery{After: after, Kinds: []string{store.KindDigestDismissed}})
		if err != nil {
			return nil, err
		}
		for _, e := range evs {
			after = e.ID
			var d store.DigestDismissed
			if json.Unmarshal(e.Data, &d) != nil || d.Key == "" {
				continue
			}
			if e.At.After(out[d.Key]) {
				out[d.Key] = e.At
			}
		}
		if len(evs) < store.DefaultEventLimit {
			return out, nil
		}
	}
}

var errNoKeys = errors.New("no keys to dismiss")

func (h *Host) DismissDigest(keys []string) error {
	at := time.Now()
	var evs []store.Event
	for _, k := range keys {
		if k = strings.TrimSpace(k); k == "" {
			continue
		}
		b, _ := json.Marshal(store.DigestDismissed{V: store.EventVersion, Key: k})
		evs = append(evs, store.Event{At: at, Kind: store.KindDigestDismissed, Data: b})
	}
	if len(evs) == 0 {
		return errNoKeys
	}
	return h.Store.Append(evs...)
}

type DismissRequest struct {
	Keys []string `json:"keys"`
}

func (h *Host) dismissDigest(w http.ResponseWriter, r *http.Request) {
	var req DismissRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "want {\"keys\": [...]}: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.DismissDigest(req.Keys); errors.Is(err, errNoKeys) {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	} else if err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, req)
}

// runItems is what one finished run needs from the person.
func (h *Host) runItems(r store.Run, helpers []store.Agent) ([]digest.Item, error) {
	var items []digest.Item
	switch {
	case r.State == store.StateBlocked:
		e := h.inboxEntry(r)
		q := strings.TrimSpace(e.Handoff)
		if q == "" {
			q = "(the step ended blocked without saying why)"
		}
		items = append(items, digest.Item{Kind: digest.KindBlocked, RunID: r.ID, Text: q, Note: "step " + e.Step, Key: digest.ItemKey(digest.KindBlocked, r.ID, e.Step)})
	case r.State == store.StateFailed || slices.Contains(deadReasons, r.Reason):
		e := h.inboxEntry(r)
		reason := r.Reason
		if reason == "" {
			reason = r.State
		}
		text := "the run ended " + reason
		if e.Step != "" {
			text = fmt.Sprintf("step %s ended %s", e.Step, reason)
		}
		tail := e.LogTail
		if len(tail) > 3 {
			tail = tail[len(tail)-3:]
		}
		items = append(items, digest.Item{Kind: digest.KindDead, RunID: r.ID, Text: text, Evidence: tail, Key: digest.ItemKey(digest.KindDead, r.ID, "")})
	}

	if it, ok := h.pickItem(r); ok {
		items = append(items, it)
	}

	var died []string
	witnesses := 0
	for _, a := range helpers {
		if slices.Contains(deadReasons, a.Reason) {
			died = append(died, fmt.Sprintf("%s (%s)", a.Role, a.Reason))
			if isWitness(a) {
				witnesses++
			}
		}
	}
	// An arena seat that ran out of time had its agent killed by the arena,
	// so only its step says it never answered.
	for _, st := range r.Steps {
		if st.State == store.StateFailed && st.Reason == arenaReasonTimedOut {
			died = append(died, fmt.Sprintf("%s (%s)", st.Name, st.Reason))
		}
	}
	if len(died) > 0 {
		items = append(items, digest.Item{
			Kind: digest.KindHelperDied, RunID: r.ID, Count: len(died),
			Text: fmt.Sprintf("%d %s ended without done: %s", len(died), pluralWord(len(died), "helper", "helpers"), strings.Join(died, ", ")),
			Note: diedNote(len(died), witnesses), Key: digest.ItemKey(digest.KindHelperDied, r.ID, ""),
		})
	}

	claims, err := h.Store.Claims(r.ID)
	if err != nil {
		return nil, err
	}
	unwitnessed := 0
	var unwitnessedAt time.Time
	for _, c := range claims {
		switch c.Verdict {
		case store.VerdictContradicted:
			items = append(items, digest.Item{
				Kind: digest.KindContradicted, RunID: r.ID, Text: c.Text, Note: c.VerdictNote, Evidence: c.Evidence, Ref: c.ID,
				Action: fmt.Sprintf(`aeolian precedent from-claim %s "<what is true>"`, c.ID),
				Key:    digest.ItemKey(digest.KindContradicted, r.ID, c.ID), At: verdictAt(c, r),
			})
		case store.VerdictUnverifiable:
			items = append(items, digest.Item{
				Kind: digest.KindUnverifiable, RunID: r.ID, Text: c.Text, Note: c.VerdictNote, Evidence: c.Evidence, Ref: c.ID,
				Key: digest.ItemKey(digest.KindUnverifiable, r.ID, c.ID), At: verdictAt(c, r),
			})
		case store.VerdictUnwitnessed:
			unwitnessed++
			if at := verdictAt(c, r); at.After(unwitnessedAt) {
				unwitnessedAt = at
			}
		}
	}
	if unwitnessed > 0 {
		items = append(items, digest.Item{
			Kind: digest.KindUnwitnessed, RunID: r.ID, Count: unwitnessed,
			Text: fmt.Sprintf("%d %s went unchecked: no witness answered in time", unwitnessed, pluralWord(unwitnessed, "claim", "claims")),
			Ref:  "/api/runs/" + r.ID + "/claims",
			Key:  digest.ItemKey(digest.KindUnwitnessed, r.ID, ""), At: unwitnessedAt,
		})
	}
	for i := range items {
		if items[i].At.IsZero() {
			items[i].At = *r.DoneAt
		}
	}
	return items, nil
}

func verdictAt(c store.Claim, r store.Run) time.Time {
	if c.VerdictAt != nil {
		return *c.VerdictAt
	}
	return *r.DoneAt
}

// precedentEvents reads the precedent events of the digest's window: the
// ids applied per run, and for each stale id the runs it matched.
func (h *Host) precedentEvents(q DigestQuery, runs []store.Run) (applied map[string][]string, stale map[string][]string, staleAt map[string]time.Time, err error) {
	applied, stale, staleAt = map[string][]string{}, map[string][]string{}, map[string]time.Time{}
	picked := map[string]bool{}
	for _, r := range runs {
		picked[r.ID] = true
	}
	for after := int64(0); ; {
		evs, err := h.Store.Events(store.EventQuery{After: after, RunID: q.RunID, Kinds: []string{store.KindPrecedentApplied, store.KindPrecedentStale}})
		if err != nil {
			return nil, nil, nil, err
		}
		for _, e := range evs {
			after = e.ID
			var p store.PrecedentIDs
			if json.Unmarshal(e.Data, &p) != nil {
				continue
			}
			switch {
			case e.Kind == store.KindPrecedentApplied && picked[e.RunID]:
				applied[e.RunID] = addAll(applied[e.RunID], p.IDs)
			case e.Kind == store.KindPrecedentStale && (q.RunID != "" || !e.At.Before(q.Since)):
				for _, id := range p.IDs {
					key := e.RunID
					if key == "" {
						key = "agent " + e.AgentID
					}
					stale[id] = addAll(stale[id], []string{key})
					if e.At.After(staleAt[id]) {
						staleAt[id] = e.At
					}
				}
			}
		}
		if len(evs) < store.DefaultEventLimit {
			return applied, stale, staleAt, nil
		}
	}
}

// precedentItems flags the stale precedents that matched a run, as long as
// they are still stale (fixing the file clears the item), and, when global,
// the precedent files that do not parse.
func (h *Host) precedentItems(stale map[string][]string, staleAt map[string]time.Time, global bool, now time.Time) []digest.Item {
	ps, errs := precedent.Load(h.PrecedentsDir())
	var items []digest.Item
	for _, p := range ps {
		matched := stale[p.ID]
		if len(matched) == 0 || !p.Stale(now) {
			continue
		}
		items = append(items, digest.Item{
			Kind: digest.KindStalePrecedent, Text: p.ID + ": " + firstLine(p.Body),
			Note: fmt.Sprintf("review by %s passed; matched %s and was left out. Still true? Move review_by on; wrong now? Delete it.",
				p.ReviewBy.Format(precedent.DateLayout), pluralWord(len(matched), "1 run", fmt.Sprintf("%d runs", len(matched)))),
			Ref: p.Path, Key: digest.ItemKey(digest.KindStalePrecedent, "", p.ID), At: staleAt[p.ID],
		})
	}
	if global && len(errs) > 0 {
		items = append(items, digest.Item{
			Kind: digest.KindBadPrecedent, Count: len(errs), Text: errs[0].Error(),
			Note: fmt.Sprintf("%s never applied until fixed", pluralWord(len(errs), "this file is", fmt.Sprintf("%d files are", len(errs)))),
			Ref:  h.PrecedentsDir(), Key: digest.ItemKey(digest.KindBadPrecedent, "", ""), At: newestFile(h.PrecedentsDir()),
		})
	}
	return items
}

func newestFile(dir string) time.Time {
	var newest time.Time
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if fi, err := e.Info(); err == nil && fi.ModTime().After(newest) {
			newest = fi.ModTime()
		}
	}
	return newest
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return l
}

func pluralWord(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func addAll(to, ids []string) []string {
	for _, id := range ids {
		if !slices.Contains(to, id) {
			to = append(to, id)
		}
	}
	return to
}

// ParseSince reads a digest's since: an RFC 3339 time, a YYYY-MM-DD day,
// or a span back from now such as 90m, 12h or 2d. Empty means
// DefaultDigestSince.
func ParseSince(s string, now time.Time) (time.Time, error) {
	if s == "" {
		return now.Add(-DefaultDigestSince), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation(time.DateOnly, s, now.Location()); err == nil {
		return t, nil
	}
	if days, ok := strings.CutSuffix(s, "d"); ok {
		if n, err := strconv.Atoi(days); err == nil && n >= 0 {
			return now.AddDate(0, 0, -n), nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil && d >= 0 {
		return now.Add(-d), nil
	}
	return time.Time{}, fmt.Errorf("since %q is not a time (RFC 3339), a day (YYYY-MM-DD) or a span such as 12h or 2d", s)
}

func (h *Host) getDigest(w http.ResponseWriter, r *http.Request) {
	q := DigestQuery{RunID: r.URL.Query().Get("run"), Max: digest.DefaultMax}
	var err error
	if a := r.URL.Query().Get("all"); a != "" {
		if q.All, err = strconv.ParseBool(a); err != nil {
			http.Error(w, fmt.Sprintf("all %q is not true or false", a), http.StatusBadRequest)
			return
		}
	}
	if q.Since, err = ParseSince(r.URL.Query().Get("since"), time.Now()); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if m := r.URL.Query().Get("max"); m != "" {
		if q.Max, err = strconv.Atoi(m); err != nil || q.Max < 0 {
			http.Error(w, fmt.Sprintf("max %q is not a count", m), http.StatusBadRequest)
			return
		}
	}
	v, err := h.Digest(q)
	if err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// diedNote says what n helpers that died, witnesses of them witnesses,
// cost: a lead carries on without a helper's handoff, while a witness's
// claims go unwitnessed and the run moves on.
func diedNote(n, witnesses int) string {
	const lead, witness = "the lead carried on without their handoff", "the claims a witness was checking went unwitnessed"
	switch witnesses {
	case 0:
		return lead
	case n:
		return witness + " and the run carried on"
	default:
		return lead + "; " + witness
	}
}

// pickItem is the item of an arena run with a human final that ranked at
// least two entries and has no pick.md yet: the decision it exists to
// hand the person.
func (h *Host) pickItem(r store.Run) (digest.Item, bool) {
	if r.Source != SourceArena {
		return digest.Item{}, false
	}
	a, err := h.Arena(r.ID)
	if err != nil || a.Final != FinalHuman || a.State != ArenaDone || len(a.Standings) < 2 {
		return digest.Item{}, false
	}
	pick := filepath.Join(a.Dir, "pick.md")
	if fi, err := os.Stat(pick); err == nil && fi.Size() > 0 {
		return digest.Item{}, false
	}
	top, next := a.Standings[0].Label, a.Standings[1].Label
	note := "the judges ranked " + top + " first"
	if a.Report != "" {
		note += "; read both in " + a.Report
	}
	return digest.Item{
		Kind: digest.KindPick, RunID: r.ID, Ref: pick, Note: note, Key: digest.ItemKey(digest.KindPick, r.ID, ""),
		Text:   fmt.Sprintf("%s or %s: %s", top, next, a.Question),
		Action: fmt.Sprintf(`echo "<%s or %s>: <why>" > %s`, top, next, pick),
	}, true
}
