package host

import (
	"fmt"
	"log"
	"slices"
	"time"

	"github.com/eduardo-pgoes/aeolian/internal/store"
)

// letter is a finished helper's handoff, waiting for its parent to collect
// it with await.
type letter struct {
	helperID, role string
	handoff        store.Handoff
}

// text is the letter as await returns it; summary, if any, describes the
// handoff's parts.
func (l letter) text(summary string) string {
	text := fmt.Sprintf("Helper %s (%s) finished:\n\n%s", l.role, l.helperID, formatHandoff(l.handoff))
	if l.handoff.Synthesized {
		text = fmt.Sprintf("Helper %s (%s) ended without calling done:\n\n%s", l.role, l.helperID, formatHandoff(l.handoff))
	}
	if summary != "" {
		text += "\n\n" + summary
	}
	return text
}

// mailbox holds one parent's helpers that report later: the ones started
// without waiting, or whose spawn call was cancelled, while they run, and
// their handoffs once they finish until await collects them. A headless
// parent has no terminal, so every such handoff lands here. An interactive
// parent gets them typed in, except while an await covers the helper.
type mailbox struct {
	pending map[string]string // helper id -> role, still running
	ready   []letter
	// wake is closed, and replaced, whenever a letter arrives.
	wake chan struct{}
	// waits are the open await calls; watchAll counts those that cover
	// every outstanding helper, watch those that name the helper.
	waits    []*awaitWait
	watchAll int
	watch    map[string]int
}

// awaitWait is one open await call. want is nil until the call has worked
// out which helpers it waits for; have are those of them that had finished
// already. woken is set, under the host's lock, as soon as the letters in
// ready meet mode, so the parent counts as working again before whoever
// posted the letter frees a slot.
type awaitWait struct {
	mode  string
	want  []string
	have  map[string]bool
	woken bool
}

// metLocked reports whether w's helpers, as ready holds them, satisfy its
// mode.
func (w *awaitWait) metLocked(ready []letter) bool {
	if w.want == nil {
		return false
	}
	n := 0
	for _, id := range w.want {
		if w.have[id] || slices.ContainsFunc(ready, func(l letter) bool { return l.helperID == id }) {
			n++
		}
	}
	return n == len(w.want) || (w.mode == AwaitAny && n > 0)
}

// blockedLocked reports whether some await call of mb is still waiting.
func (mb *mailbox) blockedLocked() bool {
	for _, w := range mb.waits {
		if !w.woken {
			return true
		}
	}
	return false
}

// mailboxLocked returns parentID's mailbox, making it if need be.
func (h *Host) mailboxLocked(parentID string) *mailbox {
	if h.mail == nil {
		h.mail = map[string]*mailbox{}
	}
	mb, ok := h.mail[parentID]
	if !ok {
		mb = &mailbox{pending: map[string]string{}, watch: map[string]int{}, wake: make(chan struct{})}
		h.mail[parentID] = mb
	}
	return mb
}

// forgetMailbox drops the mailbox of parentID, which has finished.
func (h *Host) forgetMailbox(parentID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.mail, parentID)
}

// expect records that helperID, of parentID, reports later.
func (h *Host) expect(parentID, helperID, role string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.mailboxLocked(parentID).pending[helperID] = role
}

// post leaves a finished helper's handoff in its parent's mailbox and
// reports whether it did: always for a headless parent, and for an
// interactive one only while an await covers the helper. Otherwise the
// caller types it in.
func (h *Host) post(parentHeadless bool, l letter, parentID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	mb := h.mailboxLocked(parentID)
	delete(mb.pending, l.helperID)
	if !parentHeadless && mb.watchAll == 0 && mb.watch[l.helperID] == 0 {
		return false
	}
	mb.ready = append(mb.ready, l)
	for _, w := range mb.waits {
		if !w.woken && w.metLocked(mb.ready) {
			w.woken = true
		}
	}
	close(mb.wake)
	mb.wake = make(chan struct{})
	return true
}

// helperDone routes a finished helper's handoff to its parent: to its
// blocked spawn call, to its mailbox, or typed into its terminal. While the
// helper's run is ending nothing is typed: the lead is being asked to wrap
// up.
func (h *Host) helperDone(a store.Agent, handoff store.Handoff) {
	if h.deliver(a.ID, handoff) {
		by := store.CollectedSpawn
		if isWitness(a) {
			by = store.CollectedHold
		}
		h.collected(by, a.ID)
		return
	}
	if isArenaAgent(a) {
		// An arena agent's parent is its arena, which waits on deliver;
		// there is no agent to post to or type at.
		return
	}
	parent, err := h.Store.Agent(a.ParentID)
	if err != nil {
		log.Printf("helper %s: parent %s: %v", a.ID, a.ParentID, err)
		return
	}
	if store.Finished(parent.State) {
		// Nobody is left to read it; it stays as the helper's own handoff.
		h.emit(a.RunID, a.ID, store.KindHandoffDropped, store.HandoffDropped{V: store.EventVersion, Target: a.ParentID, Why: store.DroppedFinished, Len: len(handoff.Text)})
		if isWitness(a) {
			// A witness reports to its producer's done gate, never to the
			// producer itself: should the producer be reopened, its await
			// must not hand it the witness's handoff.
			h.collected(store.CollectedHold, a.ID)
		}
		return
	}
	if h.post(parent.Headless, letter{helperID: a.ID, role: a.Role, handoff: handoff}, a.ParentID) {
		return
	}
	if a.RunID != "" && h.isEnding(a.RunID) {
		h.emit(a.RunID, a.ID, store.KindHandoffDropped, store.HandoffDropped{V: store.EventVersion, Target: a.ParentID, Why: store.DroppedRunEnding, Len: len(handoff.Text)})
		return
	}
	// Marked before typing, so an await that starts meanwhile does not
	// return it as well.
	h.collected(store.CollectedTyped, a.ID)
	go h.handBack(a.ParentID, a.Role, handoff)
}

// collected records that the handoffs of helperIDs reached their parent,
// so await does not owe them after a host restart.
func (h *Host) collected(by string, helperIDs ...string) {
	if err := h.Store.CollectHandoffs(helperIDs, by, time.Now()); err != nil {
		log.Printf("collect handoffs %v: %v", helperIDs, err)
	}
}
