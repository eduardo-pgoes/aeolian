package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const DefaultAPI = "https://api.github.com"

var (
	WritePermissions = map[string]string{"contents": "write", "pull_requests": "write"}
	ReadPermissions  = map[string]string{"contents": "read"}
)

type App struct {
	ID    int64
	Key   *rsa.PrivateKey
	Repos []string
	API   string
	HTTP  *http.Client
	Now   func() time.Time

	mu  sync.Mutex
	bot *Identity
}

type Identity struct {
	Name  string
	Email string
}

var ErrNotBot = errors.New("not a bot account")

type StatusError struct {
	Method  string
	Path    string
	Status  string
	Code    int
	Message string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s %s: %s: %q", e.Method, e.Path, e.Status, e.Message)
}

func Permanent(err error) bool {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code == http.StatusUnauthorized || se.Code == http.StatusNotFound
	}
	return errors.Is(err, ErrNotBot) || errors.Is(err, ErrNoIdentity)
}

func LoadKey(path string) (*rsa.PrivateKey, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("%s has mode %04o; the app key must not be readable by group or others, run chmod 600 %s", path, perm, path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	key, err := ParseKey(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return key, nil
}

func ParseKey(b []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("no PEM block")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		return x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("PKCS#8 key is %T, not RSA", k)
		}
		return rk, nil
	}
	return nil, fmt.Errorf("PEM block is %q, not an RSA private key", block.Type)
}

func (a *App) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *App) JWT() (string, error) {
	now := a.now()
	header, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": strconv.FormatInt(a.ID, 10),
	})
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	signed := enc.EncodeToString(header) + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.Key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signed + "." + enc.EncodeToString(sig), nil
}

func (a *App) Token(ctx context.Context, permissions map[string]string) (string, error) {
	if len(permissions) == 0 {
		return "", errors.New("github: no permissions asked for")
	}
	if len(a.Repos) == 0 {
		return "", errors.New("github: no repos configured to scope the token to")
	}
	jwt, err := a.JWT()
	if err != nil {
		return "", fmt.Errorf("github: sign app JWT: %w", err)
	}
	var install int64
	var names []string
	for _, repo := range a.Repos {
		owner, name, ok := strings.Cut(repo, "/")
		if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
			return "", fmt.Errorf("github: repo %q is not owner/name", repo)
		}
		var got struct {
			ID int64 `json:"id"`
		}
		path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/installation"
		if err := a.call(ctx, http.MethodGet, path, jwt, nil, http.StatusOK, &got); err != nil {
			return "", fmt.Errorf("github: installation for %s: %w", repo, err)
		}
		if install != 0 && got.ID != install {
			return "", fmt.Errorf("github: the configured repos span installations %d and %d; one installation is supported", install, got.ID)
		}
		install = got.ID
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	body := map[string]any{"repositories": names, "permissions": permissions}
	var got struct {
		Token string `json:"token"`
	}
	path := "/app/installations/" + strconv.FormatInt(install, 10) + "/access_tokens"
	if err := a.call(ctx, http.MethodPost, path, jwt, body, http.StatusCreated, &got); err != nil {
		return "", fmt.Errorf("github: installation %d token: %w", install, err)
	}
	if got.Token == "" {
		return "", fmt.Errorf("github: installation %d token: empty token in response", install)
	}
	return got.Token, nil
}

func (a *App) Identity(ctx context.Context) (Identity, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.bot != nil {
		return *a.bot, nil
	}
	jwt, err := a.JWT()
	if err != nil {
		return Identity{}, fmt.Errorf("github: sign app JWT: %w", err)
	}
	var app struct {
		Slug string `json:"slug"`
	}
	if err := a.call(ctx, http.MethodGet, "/app", jwt, nil, http.StatusOK, &app); err != nil {
		return Identity{}, fmt.Errorf("github: app %d: %w", a.ID, err)
	}
	if app.Slug == "" {
		return Identity{}, fmt.Errorf("github: app %d: no slug in response", a.ID)
	}
	login := app.Slug + "[bot]"
	var user struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Type  string `json:"type"`
	}
	if err := a.call(ctx, http.MethodGet, "/users/"+url.PathEscape(login), "", nil, http.StatusOK, &user); err != nil {
		return Identity{}, fmt.Errorf("github: user %s: %w", login, err)
	}
	if user.Type != "Bot" || user.ID < 1 || !strings.EqualFold(user.Login, login) {
		return Identity{}, fmt.Errorf("github: user %s: %w: login %q, type %q, id %d", login, ErrNotBot, user.Login, user.Type, user.ID)
	}
	id := Identity{Name: login, Email: strconv.FormatInt(user.ID, 10) + "+" + login + "@users.noreply.github.com"}
	a.bot = &id
	return id, nil
}

func (a *App) call(ctx context.Context, method, path, jwt string, body any, want int, out any) error {
	api := a.API
	if api == "" {
		api = DefaultAPI
	}
	var in io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		in = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(api, "/")+path, in)
	if err != nil {
		return err
	}
	if jwt != "" {
		req.Header.Set("Authorization", "Bearer "+jwt)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := a.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		var uerr *url.Error
		if errors.As(err, &uerr) {
			return fmt.Errorf("%s %s: %w", method, path, uerr.Err)
		}
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != want {
		var e struct {
			Message string `json:"message"`
		}
		json.Unmarshal(b, &e)
		msg := e.Message
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return &StatusError{Method: method, Path: path, Status: resp.Status, Code: resp.StatusCode, Message: msg}
	}
	return json.Unmarshal(b, out)
}
