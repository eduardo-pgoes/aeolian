package host

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/eduardo-pgoes/aeolian/internal/agent"
	"github.com/eduardo-pgoes/aeolian/internal/store"
)

// DefaultProgressInterval is how often a blocked spawn reports progress when
// ProgressInterval is unset. Claude Code's HTTP client gives up on a call
// after five minutes without a message.
const DefaultProgressInterval = 15 * time.Second

func (h *Host) progressInterval() time.Duration {
	if h.ProgressInterval > 0 {
		return h.ProgressInterval
	}
	return DefaultProgressInterval
}

// RoleResolver decides which helpers an agent may spawn and what each is
// first told.
type RoleResolver interface {
	// Resolve returns the first prompt of a helper in role, started by
	// parent with handoff and extra instructions. An error refuses the
	// spawn; the parent reads it as a tool error.
	Resolve(parent store.Agent, role string, handoff store.Handoff, extra string) (string, error)
}

// AnyRole allows every role and gives the helper the handoff as it came.
type AnyRole struct{}

func (AnyRole) Resolve(_ store.Agent, role string, handoff store.Handoff, extra string) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "You are a %s helper. Another agent handed you this work:\n\n%s%s", role, formatHandoff(handoff), formatParts(handoff.Parts))
	if extra != "" {
		fmt.Fprintf(&b, "\n\n%s", extra)
	}
	b.WriteString("\n\nWhen you are finished, call done with your result and every worktree you created or changed.")
	return b.String(), nil
}

func (h *Host) roles() RoleResolver {
	if h.Roles != nil {
		return h.Roles
	}
	return RunRoles{Store: h.Store, Workflow: h.wfOf}
}

func formatHandoff(h store.Handoff) string {
	var b strings.Builder
	b.WriteString(h.Text)
	if len(h.Worktrees) > 0 {
		b.WriteString("\n\nWorktrees:")
		for _, w := range h.Worktrees {
			fmt.Fprintf(&b, "\n- %s (branch %s of %s)", w.Path, w.Branch, w.Repo)
		}
	}
	return b.String()
}

// helperFinished is how a helper's handoff reaches its parent, as the spawn
// result or typed into the parent's terminal. A handoff the host wrote, for
// a helper that ended without done, says so.
func (h *Host) helperFinished(role string, handoff store.Handoff) string {
	text := fmt.Sprintf("Helper %s finished:\n\n%s", role, formatHandoff(handoff))
	if handoff.Synthesized {
		text = fmt.Sprintf("Helper %s ended without calling done:\n\n%s", role, formatHandoff(handoff))
	}
	if s := h.partsSummary(handoff.Parts); s != "" {
		text += "\n\n" + s
	}
	return text
}

func toHandoff(text string, worktrees []WorktreeInput) store.Handoff {
	h := store.Handoff{Text: text}
	for _, w := range worktrees {
		h.Worktrees = append(h.Worktrees, store.Worktree(w))
	}
	return h
}

type SpawnInput struct {
	Role              string           `json:"role" jsonschema:"the helper role to start"`
	Handoff           SpawnHandoff     `json:"handoff" jsonschema:"the work you hand the helper"`
	ExtraInstructions string           `json:"extra_instructions,omitempty" jsonschema:"anything else the helper should know; it goes into the helper's first prompt as is"`
	Worktree          *WorktreeRequest `json:"worktree,omitempty" jsonschema:"have the host make a new worktree on a new branch and start the helper in it; leave handoff worktrees empty when you set it"`
	Wait              *bool            `json:"wait,omitempty" jsonschema:"false returns the helper's id as soon as it is starting, so you can start several at once; collect their handoffs with await. Defaults to true"`
}

// SpawnHandoff has the shape of DoneInput.
type SpawnHandoff struct {
	Text      string          `json:"text" jsonschema:"what the helper should do and what it needs to know"`
	Worktrees []WorktreeInput `json:"worktrees" jsonschema:"the git worktrees the helper works in; it starts in the first one, or in an empty scratch folder when there are none"`
	Parts     []PartInput     `json:"parts,omitempty" jsonschema:"optional structured input for the helper, such as claims for it to check; the same shape as done's parts"`
}

// spawnWait is a parent blocked in spawn on one helper.
type spawnWait struct {
	parent, role string
	handoff      chan store.Handoff
}

func (h *Host) wait(helperID, parentID, role string) *spawnWait {
	w := &spawnWait{parent: parentID, role: role, handoff: make(chan store.Handoff, 1)}
	h.mu.Lock()
	if h.spawns == nil {
		h.spawns = map[string]*spawnWait{}
	}
	h.spawns[helperID] = w
	h.mu.Unlock()
	h.admit()
	return w
}

// stopWaiting ends the wait on helperID and reports whether it was still
// open. Once it returns false the handoff is in, or on its way into, the
// wait's channel.
func (h *Host) stopWaiting(helperID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.spawns[helperID]
	delete(h.spawns, helperID)
	return ok
}

// deliver hands a finished helper's handoff to its waiting parent and
// reports whether one was waiting.
func (h *Host) deliver(helperID string, handoff store.Handoff) bool {
	h.mu.Lock()
	w, ok := h.spawns[helperID]
	if ok {
		delete(h.spawns, helperID)
		w.handoff <- handoff
	}
	h.mu.Unlock()
	if ok {
		var runID string
		if a, err := h.Store.Agent(helperID); err == nil {
			runID = a.RunID
		}
		h.emit(runID, helperID, store.KindHandoffDelivered, store.HandoffDelivered{V: store.EventVersion, HelperID: helperID, ParentID: w.parent})
	}
	return ok
}

// refuse records a spawn parent asked for and did not get, and returns err
// for the parent to read.
func (h *Host) refuse(parent store.Agent, role string, err error) error {
	h.emit(parent.RunID, parent.ID, store.KindSpawnRefused, store.SpawnRefused{V: store.EventVersion, Role: role, Error: err.Error()})
	return err
}

func (h *Host) spawn(ctx context.Context, req *mcp.CallToolRequest, parentID string, in SpawnInput) (*mcp.CallToolResult, any, error) {
	parent, err := h.Store.Agent(parentID)
	if err != nil {
		return nil, nil, err
	}
	if isArenaAgent(parent) {
		return nil, nil, h.refuse(parent, in.Role, errors.New("an arena agent cannot spawn helpers"))
	}
	handoff := toHandoff(in.Handoff.Text, in.Handoff.Worktrees)
	if handoff.Parts, err = h.toParts(parent, handoff, in.Handoff.Parts); err != nil {
		return nil, nil, err
	}
	id := agent.NewSessionID()
	var plan *worktreePlan
	if in.Worktree != nil {
		if len(in.Handoff.Worktrees) > 0 {
			return nil, nil, h.refuse(parent, in.Role, errors.New("pass either worktree or handoff worktrees, not both: the helper starts in one folder"))
		}
		p, err := h.planWorktree(parent, in.Role+"-"+short(id), *in.Worktree)
		if err != nil {
			return nil, nil, h.refuse(parent, in.Role, err)
		}
		plan = &p
		handoff.Worktrees = []store.Worktree{p.worktree()}
	}
	prompt, err := h.roles().Resolve(parent, in.Role, handoff, in.ExtraInstructions)
	if err != nil {
		return nil, nil, h.refuse(parent, in.Role, err)
	}
	dir := h.Layout.ScratchDir(id)
	if plan != nil {
		if _, err := h.makeWorktree(parent, *plan); err != nil {
			return nil, nil, h.refuse(parent, in.Role, err)
		}
		dir = plan.path
	} else if len(in.Handoff.Worktrees) > 0 {
		dir = in.Handoff.Worktrees[0].Path
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			return nil, nil, h.refuse(parent, in.Role, fmt.Errorf("worktree %q is not a directory", dir))
		}
	} else {
		rd, err := h.roleDir(parent, in.Role)
		if err != nil {
			return nil, nil, h.refuse(parent, in.Role, err)
		}
		if rd != "" {
			if fi, err := os.Stat(rd); err != nil || !fi.IsDir() {
				return nil, nil, h.refuse(parent, in.Role, fmt.Errorf("role dir %q is not a directory", rd))
			}
			dir = rd
		}
	}
	h.emit(parent.RunID, parentID, store.KindSpawnRequested, store.SpawnRequested{
		V: store.EventVersion, ParentID: parentID, Role: in.Role, HelperID: id, Wait: in.Wait == nil || *in.Wait, Dir: dir, Worktrees: handoff.Worktrees,
	})

	helper := store.Agent{
		ID: id, Dir: dir, Prompt: prompt, ParentID: parentID, Role: in.Role,
		RunID: parent.RunID, Step: parent.Step, Headless: h.helperHeadless(parent, in.Role),
	}
	// A confined helper may read the worktrees it was handed, the one
	// made for it included.
	var roots []string
	for _, w := range handoff.Worktrees {
		roots = append(roots, w.Path)
	}
	launchCtx := context.Background()
	if parent.RunID != "" {
		launchCtx = h.runContext(parent.RunID)
	}
	if in.Wait != nil && !*in.Wait {
		h.expect(parentID, id, in.Role)
		go func() {
			if _, err := h.launch(launchCtx, helper, roots...); err != nil {
				log.Printf("spawn %s for %s: %v", in.Role, parentID, err)
				failed := store.Handoff{Text: fmt.Sprintf("The helper could not start: %v", err), Worktrees: []store.Worktree{}, Synthesized: true}
				switch {
				case h.post(parent.Headless, letter{helperID: id, role: in.Role, handoff: failed}, parentID):
				case parent.RunID != "" && h.isEnding(parent.RunID):
					// The lead is being asked to wrap up; its run's end
					// cancelled the launch.
					h.emit(parent.RunID, parentID, store.KindHandoffDropped, store.HandoffDropped{V: store.EventVersion, Target: parentID, Why: store.DroppedRunEnding, Len: len(failed.Text)})
				default:
					h.tell(parentID, fmt.Sprintf("Helper %s could not start: %v", in.Role, err))
				}
			}
		}()
		later := "collect its handoff with await"
		if !parent.Headless {
			later += ", or it arrives as a new message when it calls done"
		}
		return textResult(fmt.Sprintf("Helper %s is starting as %s; %s. Carry on meanwhile.", in.Role, id, later)), nil, nil
	}
	w := h.wait(helper.ID, parentID, in.Role)
	started := make(chan error, 1)
	go func() {
		_, err := h.launch(launchCtx, helper, roots...)
		if err != nil {
			h.stopWaiting(helper.ID)
			log.Printf("spawn %s for %s: %v", in.Role, parentID, err)
		}
		started <- err
	}()

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
	begun := time.Now()
	for {
		select {
		case err := <-started:
			if err != nil {
				return nil, nil, fmt.Errorf("could not start the helper: %w", err)
			}
			started = nil
		case got := <-w.handoff:
			return textResult(h.helperFinished(in.Role, got)), nil, nil
		case <-tick:
			err := req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
				ProgressToken: token,
				Progress:      time.Since(begun).Seconds(),
				Message:       "waiting for helper " + in.Role,
			})
			if err != nil {
				log.Printf("spawn progress for %s: %v", parentID, err)
			}
		case <-cancelled:
			h.detach(helper.ID, w)
			return nil, nil, errors.New("spawn cancelled; the helper keeps running: collect its handoff with await, or in an interactive session it arrives as a message")
		case <-ctx.Done():
			h.detach(helper.ID, w)
			return nil, nil, ctx.Err()
		}
	}
}

// detach stops the parent waiting on helperID; the helper's done then types
// its handoff into the parent. If the handoff already came in, nobody will
// read the spawn result, so it is typed in now.
func (h *Host) detach(helperID string, w *spawnWait) {
	if h.stopWaiting(helperID) {
		h.expect(w.parent, helperID, w.role)
		return
	}
	handoff := <-w.handoff
	headless := false
	if parent, err := h.Store.Agent(w.parent); err == nil {
		headless = parent.Headless
	}
	if !h.post(headless, letter{helperID: helperID, role: w.role, handoff: handoff}, w.parent) {
		h.collected(store.CollectedTyped, helperID)
		go h.handBack(w.parent, w.role, handoff)
	}
}

// handBack types a helper's handoff into its parent's terminal as a new
// message.
func (h *Host) handBack(parentID, role string, handoff store.Handoff) {
	h.tell(parentID, h.helperFinished(role, handoff))
}

// tell types text into agent parentID's terminal as a new message. What
// it typed, or why it typed nothing, goes into the event log.
func (h *Host) tell(parentID, text string) {
	parent, err := h.Store.Agent(parentID)
	if err != nil {
		log.Printf("tell %s: %v", parentID, err)
		return
	}
	dropped := func(why string, err error) {
		d := store.HandoffDropped{V: store.EventVersion, Target: parentID, Why: why, Len: len(text)}
		if err != nil {
			d.Error = err.Error()
		}
		h.emit(parent.RunID, parentID, store.KindHandoffDropped, d)
	}
	if store.Finished(parent.State) {
		log.Printf("tell %s: parent is %s", parentID, parent.State)
		dropped(store.DroppedFinished, nil)
		return
	}
	if parent.Headless {
		log.Printf("tell %s: parent is headless; nothing typed", parentID)
		dropped(store.DroppedHeadless, nil)
		return
	}
	sum := promptSum(text)
	h.typing.Lock()
	// Noted before typing: the prompt's hook can arrive before Type returns.
	h.hooks.noteTyped(parentID, sum, time.Now())
	err = h.Keeper.Type(parent.TmuxSession, text)
	h.typing.Unlock()
	if err != nil {
		log.Printf("tell %s: %v", parentID, err)
		dropped(store.DroppedError, err)
		return
	}
	h.emit(parent.RunID, parentID, store.KindHandoffTyped, store.HandoffTyped{V: store.EventVersion, Target: parentID, SHA256: sum, Len: len(text)})
}

func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}
