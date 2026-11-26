package host

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// busyCPU is the share of one CPU an agent's scope must have used since
// the last sweep for the watchdog to call a quiet agent working: a long
// build or test run, as against a process hung on a lock or a dead socket.
const busyCPU = 0.01

// cpuSample is an agent's scope CPU time at a sweep.
type cpuSample struct {
	usage time.Duration
	at    time.Time
}

// cgroupCPU returns the CPU time used by the cgroup of process pid, which
// counts every process the agent started in its systemd scope: the pane's
// process execs claude inside the scope systemd-run made.
func cgroupCPU(pid int) (time.Duration, error) {
	path, err := cgroupPath(pid)
	if err != nil {
		return 0, err
	}
	stat, err := os.ReadFile(filepath.Join("/sys/fs/cgroup", path, "cpu.stat"))
	if err != nil {
		return 0, err
	}
	sc := bufio.NewScanner(bytes.NewReader(stat))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "usage_usec "); ok {
			usec, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return 0, err
			}
			return time.Duration(usec) * time.Microsecond, nil
		}
	}
	return 0, errors.New("no usage_usec in cpu.stat")
}

// What the watchdog learns from an agent's scope CPU time.
const (
	cpuUnknown = iota // unreadable: the watchdog goes by hooks and transcript alone
	cpuFirst          // first sample: ask again next sweep
	cpuIdle           // under busyCPU since the last sample
	cpuWorking        // at least busyCPU since the last sample
)

// cpuState samples agent id's scope, whose pane process is pid, and
// compares it with the sample before.
func (h *Host) cpuState(id string, pid int, now time.Time) int {
	read := h.CPUUsage
	if read == nil {
		read = cgroupCPU
	}
	usage, err := read(pid)
	h.mu.Lock()
	defer h.mu.Unlock()
	if err != nil {
		delete(h.cpu, id)
		return cpuUnknown
	}
	if h.cpu == nil {
		h.cpu = map[string]cpuSample{}
	}
	prev, ok := h.cpu[id]
	h.cpu[id] = cpuSample{usage: usage, at: now}
	switch {
	case !ok || !now.After(prev.at):
		return cpuFirst
	case float64(usage-prev.usage) >= busyCPU*float64(now.Sub(prev.at)):
		return cpuWorking
	}
	return cpuIdle
}

// forgetCPU drops the samples of agents not in keep.
func (h *Host) forgetCPU(keep map[string]bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id := range h.cpu {
		if !keep[id] {
			delete(h.cpu, id)
		}
	}
}
