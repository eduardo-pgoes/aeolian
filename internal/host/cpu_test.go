package host

import (
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eduardo-pgoes/aeolian/internal/store"
)

func TestCgroupCPUReadsOwnScope(t *testing.T) {
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		t.Skip("no cgroup v2")
	}
	a, err := cgroupCPU(os.Getpid())
	if err != nil {
		t.Skip("cgroup cpu.stat unreadable: ", err)
	}
	for end := time.Now().Add(30 * time.Millisecond); time.Now().Before(end); {
	}
	b, err := cgroupCPU(os.Getpid())
	if err != nil || b < a || a <= 0 {
		t.Fatalf("usage %v then %v: %v", a, b, err)
	}
	if _, err := cgroupCPU(0); err == nil {
		t.Fatal("pid 0 read")
	}
}

// TestWatchdogSparesQuietHelperUsingCPU: a helper in one long tool call
// shows no hook and no transcript write, but its scope burns CPU; it is
// stopped only once that CPU use stops too.
func TestWatchdogSparesQuietHelperUsingCPU(t *testing.T) {
	f := newFixture(t)
	f.host.StallAfter = 50 * time.Millisecond
	f.host.IdleCloseAfter = -1
	var usage atomic.Int64
	f.host.CPUUsage = func(int) (time.Duration, error) { return time.Duration(usage.Load()), nil }
	_, build := f.startHeadless(t, headlessWorkflow)
	helper := f.spawnLater(t, f.connect(t, build.ID), "reviewer", "x")
	f.waitStarted(t, helper)
	time.Sleep(80 * time.Millisecond)
	for range 3 {
		f.host.Sweep()
		noKill(t, f.keeper)
		usage.Add(int64(time.Second)) // a build running between sweeps
		time.Sleep(10 * time.Millisecond)
	}
	f.host.Sweep() // sees the last second of CPU
	noKill(t, f.keeper)
	if f.state(t, helper) != store.StateBusy {
		t.Fatal("working helper stopped")
	}
	time.Sleep(10 * time.Millisecond)
	f.host.Sweep() // no CPU since: hung
	killed(t, f.keeper)
	if a := f.agent(t, helper); a.State != store.StateKilled || a.Reason != store.ReasonLost {
		t.Fatalf("helper %s/%s", a.State, a.Reason)
	}
}
