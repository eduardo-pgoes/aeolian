package github

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func script(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

const fakeToken = "gho_fakefakefake"

func TestUserToken(t *testing.T) {
	u := &User{TokenCommand: []string{script(t, `[ "$1 $2" = "auth token" ] || exit 9; echo `+fakeToken), "auth", "token"}}
	got, err := u.Token(context.Background(), ReadPermissions)
	if err != nil || got != fakeToken {
		t.Fatalf("token %q, err %v", got, err)
	}
	if _, err := u.Token(context.Background(), nil); err == nil {
		t.Fatal("no permissions")
	}
}

func TestUserTokenFailsLoudlyWithoutTheToken(t *testing.T) {
	for name, c := range map[string]struct {
		body, want string
	}{
		"logged out":   {`echo "no oauth token found for github.com" >&2; exit 1`, "no oauth token found"},
		"empty":        {`exit 0`, "printed no token"},
		"many lines":   {`echo ` + fakeToken + `; echo ` + fakeToken, "only the token"},
		"prints, dies": {`echo ` + fakeToken + `; exit 4`, "exit status 4"},
	} {
		t.Run(name, func(t *testing.T) {
			u := &User{TokenCommand: []string{script(t, c.body)}}
			_, err := u.Token(context.Background(), WritePermissions)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err %v, want %q", err, c.want)
			}
			if strings.Contains(err.Error(), fakeToken) {
				t.Fatalf("error leaks the token: %v", err)
			}
		})
	}
	u := &User{TokenCommand: []string{filepath.Join(t.TempDir(), "missing")}}
	if _, err := u.Token(context.Background(), ReadPermissions); err == nil || !strings.Contains(err.Error(), "gh auth login") {
		t.Fatalf("missing command: %v", err)
	}
}

func TestUserIdentity(t *testing.T) {
	git := script(t, `[ "$1 $2 $3" = "config --global --get" ] || exit 9
case "$4" in
user.name) echo "Ada Lovelace";;
user.email) echo "ada@example.com";;
*) exit 1;;
esac`)
	id, err := (&User{Git: git}).Identity(context.Background())
	if err != nil || id != (Identity{Name: "Ada Lovelace", Email: "ada@example.com"}) {
		t.Fatalf("identity %+v, err %v", id, err)
	}
}

func TestUserIdentityUnsetIsPermanent(t *testing.T) {
	git := script(t, `[ "$4" = user.name ] && echo "Ada"; [ "$4" = user.name ]`)
	_, err := (&User{Git: git}).Identity(context.Background())
	if !errors.Is(err, ErrNoIdentity) || !Permanent(err) {
		t.Fatalf("err %v", err)
	}
	broken := script(t, `echo "fatal: bad config" >&2; exit 128`)
	_, err = (&User{Git: broken}).Identity(context.Background())
	if err == nil || Permanent(err) || !strings.Contains(err.Error(), "bad config") {
		t.Fatalf("broken git: %v", err)
	}
}
