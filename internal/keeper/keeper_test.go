package keeper

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	UsePrivateSocket()
	code := m.Run()
	if err := KillPrivateServer(); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(code)
}

func TestKillPrivateServerRefusesDefault(t *testing.T) {
	saved := Socket
	t.Cleanup(func() { Socket = saved })
	Socket = DefaultSocket
	if err := KillPrivateServer(); err == nil {
		t.Fatal("killed the default server")
	}
}

func testSession(t *testing.T) (name, dir string) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	b := make([]byte, 4)
	rand.Read(b)
	name = SessionPrefix + "test-" + hex.EncodeToString(b)
	dir = t.TempDir()
	if err := Start(name, dir, []string{"sh", "-c", "cat > out"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { Kill(name) })
	return name, dir
}

func waitFor(t *testing.T, path, want string) {
	t.Helper()
	var got []byte
	for range 50 {
		got, _ = os.ReadFile(path)
		if string(got) == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("got %q, want %q", got, want)
}

func TestTypeOneAndManyLines(t *testing.T) {
	name, dir := testSession(t)
	if err := Type(name, "hello world"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, filepath.Join(dir, "out"), "hello world\n")
	if err := Type(name, "first\nsecond\n"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, filepath.Join(dir, "out"), "hello world\nfirst\nsecond\n")
}

func TestListAndKill(t *testing.T) {
	name, dir := testSession(t)
	sessions, err := List()
	if err != nil {
		t.Fatal(err)
	}
	found := find(t, sessions, name)
	if found.Dir != dir {
		t.Errorf("dir %q, want %q", found.Dir, dir)
	}
	if found.Cols != 200 || found.Rows != 50 {
		t.Errorf("size %dx%d, want 200x50", found.Cols, found.Rows)
	}
	if _, err := tmux(nil, "resize-window", "-t", target(name), "-x", "120", "-y", "40"); err != nil {
		t.Fatal(err)
	}
	sessions, err = List()
	if err != nil {
		t.Fatal(err)
	}
	if found := find(t, sessions, name); found.Cols != 120 || found.Rows != 40 {
		t.Errorf("size after resize %dx%d, want 120x40", found.Cols, found.Rows)
	}
	if err := Kill(name); err != nil {
		t.Fatal(err)
	}
	if ok, err := Has(name); ok || err != nil {
		t.Fatalf("Has after kill = %v, %v", ok, err)
	}
	if err := Kill(name); err != ErrNoSession {
		t.Fatalf("second kill: %v", err)
	}
}

func find(t *testing.T, sessions []Session, name string) Session {
	t.Helper()
	for _, s := range sessions {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("%s not in %v", name, sessions)
	return Session{}
}

func TestStartRejectsForeignName(t *testing.T) {
	if err := Start("other", t.TempDir(), []string{"true"}); err == nil {
		t.Fatal("want error")
	}
}

func TestPrivateSocketIsNotLive(t *testing.T) {
	if Socket == DefaultSocket || os.Getenv(SocketEnv) != Socket {
		t.Fatalf("tests run on socket %q", Socket)
	}
}

func TestStartKeepsDollarBracesLiteral(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	b := make([]byte, 4)
	rand.Read(b)
	name, dir := SessionPrefix+"test-"+hex.EncodeToString(b), t.TempDir()
	args := `${HOME} ${p%.} $HOME`
	if err := Start(name, dir, []string{"sh", "-c", `printf '%s' "$0" > out; sleep 5`, args}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { Kill(name) })
	waitFor(t, filepath.Join(dir, "out"), args)
}

func TestCaptureShowsScreen(t *testing.T) {
	name, _ := testSession(t)
	if err := Type(name, "visible text"); err != nil {
		t.Fatal(err)
	}
	var screen string
	for range 50 {
		screen, _ = Capture(name)
		if strings.Contains(screen, "visible text") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(screen, "visible text") {
		t.Fatalf("screen: %q", screen)
	}
	if _, err := Capture(SessionPrefix + "nobody"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("missing session: %v", err)
	}
}
