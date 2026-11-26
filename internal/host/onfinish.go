package host

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"github.com/eduardo-pgoes/aeolian/internal/store"
)

// DefaultOnFinishTimeout is how long an on_finish script may run before the
// host kills it.
const DefaultOnFinishTimeout = 60 * time.Second

const onFinishWaitDelay = 5 * time.Second

func (h *Host) onFinishTimeout() time.Duration {
	if h.OnFinishTimeout > 0 {
		return h.OnFinishTimeout
	}
	return DefaultOnFinishTimeout
}

// onFinish hands run runID's inbox entry to the on_finish script, if there
// is one, without waiting for it.
func (h *Host) onFinish(runID string) {
	if len(h.OnFinish) == 0 {
		return
	}
	go h.runOnFinish(runID)
}

func (h *Host) runOnFinish(runID string) {
	fail := func(reason string, stderr []byte) {
		msg := fmt.Sprintf("on_finish run %s: %s", runID, reason)
		for _, l := range stderrTail(stderr) {
			msg += "\n  " + redact(l)
		}
		log.Print(msg)
	}
	run, err := h.Store.Run(runID)
	if err != nil {
		fail(err.Error(), nil)
		return
	}
	entry, err := json.Marshal(h.inboxEntry(run))
	if err != nil {
		fail(err.Error(), nil)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), h.onFinishTimeout())
	defer cancel()
	cmd := exec.CommandContext(ctx, h.OnFinish[0], h.OnFinish[1:]...)
	cmd.Stdin = bytes.NewReader(entry)
	var stderr tailBuffer
	cmd.Stderr = &stderr
	cmd.WaitDelay = onFinishWaitDelay
	begun := time.Now()
	err = cmd.Run()
	result := store.OnFinishResult{V: store.EventVersion, DurationMS: time.Since(begun).Milliseconds()}
	switch {
	case err == nil:
	case ctx.Err() == context.DeadlineExceeded:
		result.Error = fmt.Sprintf("killed after %s", h.onFinishTimeout())
		fail(result.Error, stderr.Bytes())
	default:
		result.Error = err.Error()
		fail(err.Error(), stderr.Bytes())
	}
	result.Exit = -1
	if cmd.ProcessState != nil {
		result.Exit = cmd.ProcessState.ExitCode()
	}
	if err != nil {
		for _, l := range stderrTail(stderr.Bytes()) {
			result.StderrTail = append(result.StderrTail, redact(l))
		}
	}
	h.emit(runID, "", store.KindOnFinishResult, result)
}

// tailBuffer keeps the last tailBufferSize bytes written to it.
type tailBuffer struct{ b []byte }

const tailBufferSize = 64 << 10

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if over := len(t.b) - tailBufferSize; over > 0 {
		t.b = t.b[over:]
	}
	return len(p), nil
}

func (t *tailBuffer) Bytes() []byte { return t.b }

func stderrTail(b []byte) []string {
	text := strings.TrimRight(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	return lines[max(0, len(lines)-logTailLines):]
}
