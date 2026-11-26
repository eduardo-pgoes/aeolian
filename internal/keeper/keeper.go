// Package keeper holds agent terminals as tmux sessions on the dedicated
// aeolian socket.
package keeper

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

// DefaultSocket is the tmux server the live host keeps its agents on.
const DefaultSocket = "aeolian"

// SocketEnv overrides the socket, so tests and side-by-side hosts never
// share the live server.
const SocketEnv = "AEOLIAN_TMUX_SOCKET"

// Socket is the only tmux server the keeper talks to: DefaultSocket unless
// SocketEnv names another. Set it once, before any session starts.
var Socket = socketFromEnv()

func socketFromEnv() string {
	if s := os.Getenv(SocketEnv); s != "" {
		return s
	}
	return DefaultSocket
}

// UsePrivateSocket points the keeper, and every child process through the
// environment, at a fresh tmux server of its own. Test mains call it first
// and KillPrivateServer last.
func UsePrivateSocket() string {
	b := make([]byte, 4)
	rand.Read(b)
	Socket = fmt.Sprintf("aeolian-test-%d-%s", os.Getpid(), hex.EncodeToString(b))
	os.Setenv(SocketEnv, Socket)
	return Socket
}

// KillPrivateServer ends the tmux server on Socket. It refuses the default
// socket: that server holds the live agents.
func KillPrivateServer() error {
	if Socket == DefaultSocket {
		return errors.New("refusing to kill the default aeolian tmux server")
	}
	_, err := run(nil, "tmux", "-L", Socket, "kill-server")
	if errors.Is(err, ErrNoSession) {
		err = nil
	}
	// A server that exits on its own can leave its socket file behind.
	dir := os.Getenv("TMUX_TMPDIR")
	if dir == "" {
		dir = "/tmp"
	}
	os.Remove(filepath.Join(dir, fmt.Sprintf("tmux-%d", os.Getuid()), Socket))
	return err
}

// SessionPrefix starts the name of every session the keeper owns.
const SessionPrefix = "aeolian-"

var ErrNoSession = errors.New("no such session")

type Session struct {
	Name    string
	Created string
	Dir     string
	PID     int
	Cols    int
	Rows    int
}

func tmux(stdin []byte, args ...string) (string, error) {
	return retry(stdin, append([]string{"tmux", "-L", Socket}, args...))
}

// scope runs a tmux client in its own transient user scope. A server the
// client forks stays in that scope, so it outlives whatever process asked
// for the session, even one stopped as a systemd unit with its cgroup.
var scope = []string{"systemd-run", "--user", "--scope", "--quiet", "--expand-environment=no", "--"}

func tmuxInScope(args ...string) (string, error) {
	return retry(nil, append(append(append([]string{}, scope...), "tmux", "-L", Socket), args...))
}

func retry(stdin []byte, argv []string) (string, error) {
	out, err := run(stdin, argv...)
	for retry := 0; errors.Is(err, errServerExited) && retry < 3; retry++ {
		time.Sleep(100 * time.Millisecond)
		out, err = run(stdin, argv...)
	}
	return out, err
}

// errServerExited is what a client sees when it reaches a server that is
// shutting down because its last session just ended; the command never ran.
var errServerExited = errors.New("tmux server exited")

func run(stdin []byte, argv ...string) (string, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if isMissing(msg) {
			return "", ErrNoSession
		}
		if strings.Contains(msg, "server exited unexpectedly") {
			return "", errServerExited
		}
		return "", fmt.Errorf("tmux %s: %w: %s", argv[slices.Index(argv, "-L")+2], err, msg)
	}
	return stdout.String(), nil
}

func isMissing(stderr string) bool {
	return strings.Contains(stderr, "no server running") ||
		strings.Contains(stderr, "error connecting to") ||
		strings.Contains(stderr, "can't find session") ||
		strings.Contains(stderr, "can't find pane") ||
		strings.Contains(stderr, "can't find window")
}

func target(name string) string { return "=" + name }

func paneTarget(name string) string { return "=" + name + ":" }

// Start runs argv in a new detached session called name, with dir as its
// working directory. argv is passed to tmux as separate words, so no shell
// quoting is involved.
func Start(name, dir string, argv []string) error {
	if !strings.HasPrefix(name, SessionPrefix) {
		return fmt.Errorf("session name %q lacks prefix %q", name, SessionPrefix)
	}
	args := []string{"new-session", "-d", "-s", name, "-c", dir, "-x", "200", "-y", "50", "--"}
	_, err := tmuxInScope(append(args, argv...)...)
	return err
}

// Type enters text into the session as if typed and submits it. A single line
// goes in literally; text with newlines goes in as one bracketed paste so the
// newlines do not submit it early.
func Type(name, text string) error {
	text = strings.TrimRight(text, "\n")
	if strings.Contains(text, "\n") {
		buf := "aeolian-paste-" + name
		if _, err := tmux([]byte(text), "load-buffer", "-b", buf, "-"); err != nil {
			return err
		}
		if _, err := tmux(nil, "paste-buffer", "-p", "-d", "-b", buf, "-t", paneTarget(name)); err != nil {
			return err
		}
	} else if _, err := tmux(nil, "send-keys", "-t", paneTarget(name), "-l", text); err != nil {
		return err
	}
	_, err := tmux(nil, "send-keys", "-t", paneTarget(name), "Enter")
	return err
}

func Capture(name string) (string, error) {
	return tmux(nil, "capture-pane", "-p", "-t", paneTarget(name))
}

// List returns the keeper's sessions; an absent server means none.
func List() ([]Session, error) {
	out, err := tmux(nil, "list-sessions", "-F", "#{session_name}\t#{session_created}\t#{pane_current_path}\t#{pane_pid}\t#{window_width}\t#{window_height}")
	if errors.Is(err, ErrNoSession) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var sessions []Session
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 6 || !strings.HasPrefix(f[0], SessionPrefix) {
			continue
		}
		s := Session{Name: f[0], Created: f[1], Dir: f[2]}
		fmt.Sscan(f[3], &s.PID)
		fmt.Sscan(f[4], &s.Cols)
		fmt.Sscan(f[5], &s.Rows)
		sessions = append(sessions, s)
	}
	return sessions, nil
}

func Has(name string) (bool, error) {
	_, err := tmux(nil, "has-session", "-t", target(name))
	if errors.Is(err, ErrNoSession) {
		return false, nil
	}
	return err == nil, err
}

// Kill ends the session and waits briefly for its pane process to exit, so
// a resume right after does not race the old process.
func Kill(name string) error {
	out, err := tmux(nil, "list-panes", "-t", target(name), "-F", "#{pane_pid}")
	if err != nil {
		return err
	}
	var pid int
	fmt.Sscan(out, &pid)
	if _, err := tmux(nil, "kill-session", "-t", target(name)); err != nil {
		return err
	}
	for deadline := time.Now().Add(5 * time.Second); pid > 0 && time.Now().Before(deadline); {
		if syscall.Kill(pid, 0) != nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil
}
