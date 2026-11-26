package github

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

var ErrNoIdentity = errors.New("no git identity")

type User struct {
	TokenCommand []string
	Git          string
}

func (u *User) Token(ctx context.Context, permissions map[string]string) (string, error) {
	if len(permissions) == 0 {
		return "", errors.New("github: no permissions asked for")
	}
	if len(u.TokenCommand) == 0 || u.TokenCommand[0] == "" {
		return "", errors.New("github: no token command")
	}
	name := strings.Join(u.TokenCommand, " ")
	cmd := exec.CommandContext(ctx, u.TokenCommand[0], u.TokenCommand[1:]...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("github: %s: %w: %s; is gh logged in? run gh auth login", name, err, clip(stderr.String()))
	}
	token := strings.TrimSpace(string(out))
	switch {
	case token == "":
		return "", fmt.Errorf("github: %s printed no token; is gh logged in? run gh auth login", name)
	case strings.ContainsAny(token, " \t\r\n"):
		return "", fmt.Errorf("github: %s printed more than a token; it must print only the token", name)
	}
	return token, nil
}

func (u *User) Identity(ctx context.Context) (Identity, error) {
	name, err := u.gitConfig(ctx, "user.name")
	if err != nil {
		return Identity{}, err
	}
	email, err := u.gitConfig(ctx, "user.email")
	if err != nil {
		return Identity{}, err
	}
	if name == "" || email == "" {
		return Identity{}, fmt.Errorf("github: %w: set git config --global user.name and user.email, agents commit as you", ErrNoIdentity)
	}
	return Identity{Name: name, Email: email}, nil
}

func (u *User) gitConfig(ctx context.Context, key string) (string, error) {
	git := u.Git
	if git == "" {
		git = "git"
	}
	cmd := exec.CommandContext(ctx, git, "config", "--global", "--get", key)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("github: git config --global %s: %w: %s", key, err, clip(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

func clip(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
