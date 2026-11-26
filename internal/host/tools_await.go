package host

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/eduardo-pgoes/aeolian/internal/store"
)

const (
	// DefaultAwaitTimeout is how long await waits when the call names no
	// timeout.
	DefaultAwaitTimeout = 10 * time.Minute
	// MaxAwaitTimeout caps the timeout an await call may ask for.
	MaxAwaitTimeout = time.Hour

	AwaitAll = "all"
	AwaitAny = "any"
)

// awaitRegistered, when set, is called once an await call is registered
// and before it reads the store; tests use it to finish a helper there.
var awaitRegistered func(parentID string)

type AwaitInput struct {
	Helpers  []string `json:"helpers,omitempty" jsonschema:"ids of the helpers to wait for, as spawn returned them; empty waits for every helper you started that has not handed back yet"`
	Mode     string   `json:"mode,omitempty" jsonschema:"all, the default, returns once every helper waited for has finished; any returns as soon as one has"`
	TimeoutS int      `json:"timeout_s,omitempty" jsonschema:"seconds to wait before returning with whatever has finished; defaults to 600, at most 3600"`
}

func init() {
	extraTools = append(extraTools, func(h *Host, s *mcp.Server, id string, a store.Agent) {
		// An arena agent spawns nothing, so it has nothing to await.
		if !isArenaAgent(a) {
			h.addAwaitTool(s, id)
		}
	})
}

func (h *Host) addAwaitTool(s *mcp.Server, id string) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "await",
		Description: "Wait for helpers you started with spawn and wait false, and get their handoffs. " +
			"Each handoff is returned once; a helper that died or stalled comes back as a handoff the host wrote. " +
			"Start several helpers without waiting, then await them all, or await any to take each as it lands.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in AwaitInput) (*mcp.CallToolResult, any, error) {
		return h.await(ctx, req, id, in)
	})
}

func (h *Host) await(ctx context.Context, req *mcp.CallToolRequest, parentID string, in AwaitInput) (*mcp.CallToolResult, any, error) {
	mode := in.Mode
	switch mode {
	case "":
		mode = AwaitAll
	case AwaitAll, AwaitAny:
	default:
		return nil, nil, fmt.Errorf("mode %q is neither %s nor %s", in.Mode, AwaitAll, AwaitAny)
	}
	timeout := DefaultAwaitTimeout
	if in.TimeoutS > 0 {
		timeout = min(time.Duration(in.TimeoutS)*time.Second, MaxAwaitTimeout)
	}
	parent, err := h.Store.Agent(parentID)
	if err != nil {
		return nil, nil, err
	}

	// The await is registered before the store is read: a helper that
	// finishes after this point posts its letter here, one that finished
	// before is in the store.
	w := &awaitWait{mode: mode}
	h.mu.Lock()
	mb := h.mailboxLocked(parentID)
	if len(in.Helpers) == 0 {
		mb.watchAll++
	} else {
		for _, id := range in.Helpers {
			mb.watch[id]++
		}
	}
	mb.waits = append(mb.waits, w)
	pending := maps.Clone(mb.pending)
	var ready []letter
	if len(in.Helpers) == 0 {
		ready = slices.Clone(mb.ready)
	}
	h.mu.Unlock()
	h.admit()
	if awaitRegistered != nil {
		awaitRegistered(parentID)
	}
	var flush []letter
	defer func() {
		h.mu.Lock()
		mb.waits = slices.DeleteFunc(mb.waits, func(o *awaitWait) bool { return o == w })
		if len(in.Helpers) == 0 {
			mb.watchAll--
		} else {
			for _, id := range in.Helpers {
				if mb.watch[id]--; mb.watch[id] <= 0 {
					delete(mb.watch, id)
				}
			}
		}
		if !parent.Headless && mb.watchAll == 0 {
			// Letters nobody awaits any more go to the terminal, as they
			// would have without the await.
			keep := mb.ready[:0]
			for _, l := range mb.ready {
				if mb.watch[l.helperID] > 0 {
					keep = append(keep, l)
				} else {
					flush = append(flush, l)
				}
			}
			mb.ready = keep
		}
		h.mu.Unlock()
		for _, l := range flush {
			h.collected(store.CollectedTyped, l.helperID)
			go h.handBack(parentID, l.role, l.handoff)
		}
	}()

	// stored are the helpers waited for that had finished already: a
	// named one's handoff is returned even if it was collected before.
	stored := map[string]letter{}
	roles := map[string]string{}
	finished := func(a store.Agent) letter {
		l := letter{helperID: a.ID, role: a.Role, handoff: store.Handoff{Text: "(it ended with no handoff)", Worktrees: []store.Worktree{}}}
		if a.Handoff != nil {
			l.handoff = *a.Handoff
		}
		return l
	}
	want := slices.Clone(in.Helpers)
	if len(in.Helpers) > 0 {
		for _, id := range in.Helpers {
			if role, ok := pending[id]; ok {
				// Started without waiting and maybe still queued for a
				// slot, so it may have no row yet.
				roles[id] = role
				continue
			}
			a, err := h.Store.Agent(id)
			if errors.Is(err, store.ErrNotFound) || (err == nil && a.ParentID != parentID) {
				return nil, nil, fmt.Errorf("%s is not a helper you started", id)
			}
			if err != nil {
				return nil, nil, err
			}
			roles[id] = a.Role
			if store.Finished(a.State) {
				stored[id] = finished(a)
			}
		}
	} else {
		for id, role := range pending {
			want = append(want, id)
			roles[id] = role
		}
		for _, l := range ready {
			want = append(want, l.helperID)
			roles[l.helperID] = l.role
		}
		// After a host restart the mailbox is empty: the live helpers, and
		// the finished ones whose handoff never reached the parent, are
		// read from the store. Live first, so one that finishes in between
		// is uncollected or posted here.
		live, err := h.Store.LiveAgents()
		if err != nil {
			return nil, nil, err
		}
		owed, err := h.Store.Uncollected(parentID)
		if err != nil {
			return nil, nil, err
		}
		h.mu.Lock()
		for _, a := range live {
			if _, blocked := h.spawns[a.ID]; a.ParentID == parentID && !blocked {
				want = append(want, a.ID)
				roles[a.ID] = a.Role
			}
		}
		for _, a := range owed {
			if _, blocked := h.spawns[a.ID]; !blocked {
				want = append(want, a.ID)
				roles[a.ID] = a.Role
				stored[a.ID] = finished(a)
			}
		}
		h.mu.Unlock()
		slices.Sort(want)
		want = slices.Compact(want)
	}
	h.mu.Lock()
	w.want = want
	w.have = map[string]bool{}
	for id := range stored {
		w.have[id] = true
	}
	h.mu.Unlock()
	if len(want) == 0 {
		return textResult("No helpers are outstanding: every helper you started has handed back already."), nil, nil
	}

	var tick <-chan time.Time
	token := req.Params.GetProgressToken()
	if token != nil {
		t := time.NewTicker(h.progressInterval())
		defer t.Stop()
		tick = t.C
	}
	var cancelled <-chan struct{}
	if req.Extra != nil {
		cancelled = h.callDone(req.Extra.Header.Get(callHeader))
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	begun := time.Now()
	for {
		got, wake := h.collect(mb, want, stored, mode, false)
		if got != nil {
			h.collectedLetters(got)
			return textResult(h.awaitText(got, want, roles, false)), nil, nil
		}
		select {
		case <-wake:
		case <-deadline.C:
			got, _ := h.collect(mb, want, stored, mode, true)
			h.collectedLetters(got)
			return textResult(h.awaitText(got, want, roles, true)), nil, nil
		case <-tick:
			err := req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
				ProgressToken: token,
				Progress:      time.Since(begun).Seconds(),
				Message:       "waiting for helpers",
			})
			if err != nil {
				log.Printf("await progress for %s: %v", parentID, err)
			}
		case <-cancelled:
			return nil, nil, errors.New("await cancelled; the helpers keep running, call await again to collect them")
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
}

// collectedLetters records that await returned got.
func (h *Host) collectedLetters(got []letter) {
	ids := make([]string, len(got))
	for i, l := range got {
		ids[i] = l.helperID
	}
	h.collected(store.CollectedAwait, ids...)
}

// collect takes the letters of want out of mb once mode is met, or at once
// when final, and returns them in want's order with nil wake. Otherwise it
// takes nothing and returns the channel that signals the next letter.
func (h *Host) collect(mb *mailbox, want []string, stored map[string]letter, mode string, final bool) ([]letter, <-chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	have := map[string]letter{}
	for id, l := range stored {
		have[id] = l
	}
	for _, l := range mb.ready {
		if slices.Contains(want, l.helperID) {
			have[l.helperID] = l
		}
	}
	met := len(have) == len(want) || (mode == AwaitAny && len(have) > 0)
	if !met && !final {
		return nil, mb.wake
	}
	keep := mb.ready[:0]
	for _, l := range mb.ready {
		if _, ok := have[l.helperID]; !ok {
			keep = append(keep, l)
		}
	}
	mb.ready = keep
	got := []letter{}
	for _, id := range want {
		if l, ok := have[id]; ok {
			got = append(got, l)
		}
	}
	return got, nil
}

func (h *Host) awaitText(got []letter, want []string, roles map[string]string, timedOut bool) string {
	var b strings.Builder
	done := map[string]bool{}
	for i, l := range got {
		if i > 0 {
			b.WriteString("\n\n---\n\n")
		}
		b.WriteString(l.text(h.partsSummary(l.handoff.Parts)))
		done[l.helperID] = true
	}
	var running []string
	for _, id := range want {
		if !done[id] {
			running = append(running, fmt.Sprintf("%s (%s)", roles[id], id))
		}
	}
	if len(running) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n\n---\n\n")
		}
		if timedOut {
			b.WriteString("Timed out. ")
		}
		fmt.Fprintf(&b, "Still running: %s. Call await again to collect them.", strings.Join(running, ", "))
	}
	return b.String()
}
