package github

import (
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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	keyOnce sync.Once
	testKey *rsa.PrivateKey
)

func key(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	keyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		testKey = k
	})
	return testKey
}

type fakeGitHub struct {
	t        *testing.T
	pub      *rsa.PublicKey
	appID    string
	installs map[string]int64
	status   int
	slug     string
	users    map[string]string
	userCode int
	mu       sync.Mutex
	bodies   []map[string]any
	paths    []string
	userAuth []string
}

func (f *fakeGitHub) verify(r *http.Request) bool {
	auth, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}
	parts := strings.Split(auth, ".")
	if len(parts) != 3 {
		return false
	}
	enc := base64.RawURLEncoding
	var header map[string]string
	var claims map[string]any
	hb, _ := enc.DecodeString(parts[0])
	cb, _ := enc.DecodeString(parts[1])
	sig, err := enc.DecodeString(parts[2])
	if err != nil || json.Unmarshal(hb, &header) != nil || json.Unmarshal(cb, &claims) != nil {
		return false
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(f.pub, crypto.SHA256, sum[:], sig) != nil {
		return false
	}
	now := float64(time.Now().Unix())
	iat, _ := claims["iat"].(float64)
	exp, _ := claims["exp"].(float64)
	return header["alg"] == "RS256" && claims["iss"] == f.appID && iat <= now && exp > now && exp-iat <= 600
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.paths = append(f.paths, r.Method+" "+r.URL.Path)
	f.mu.Unlock()
	if login, ok := strings.CutPrefix(r.URL.Path, "/users/"); ok && r.Method == http.MethodGet {
		f.mu.Lock()
		f.userAuth = append(f.userAuth, r.Header.Get("Authorization"))
		f.mu.Unlock()
		if r.Header.Get("Authorization") != "" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"message":"Bad credentials"}`))
			return
		}
		if f.userCode != 0 {
			w.WriteHeader(f.userCode)
			w.Write([]byte(`{"message":"API rate limit exceeded"}`))
			return
		}
		body, ok := f.users[login]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"Not Found"}`))
			return
		}
		w.Write([]byte(body))
		return
	}
	if !f.verify(r) || r.Header.Get("X-GitHub-Api-Version") == "" {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"A JSON web token could not be decoded"}`))
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/app" {
		json.NewEncoder(w).Encode(map[string]any{"id": 1234, "slug": f.slug, "name": "Acme Agent"})
		return
	}
	if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/installation") {
		repo := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/repos/"), "/installation")
		id, ok := f.installs[repo]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"Not Found"}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id": id})
		return
	}
	if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/app/installations/") {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.bodies = append(f.bodies, body)
		f.mu.Unlock()
		if f.status != 0 {
			w.WriteHeader(f.status)
			w.Write([]byte(`{"message":"The permissions requested are not granted to this installation."}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"token":"ghs_minted","expires_at":"2099-01-01T00:00:00Z"}`))
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

func newFake(t *testing.T) (*fakeGitHub, *httptest.Server) {
	f := &fakeGitHub{
		t: t, pub: &key(t).PublicKey, appID: "1234",
		installs: map[string]int64{"acme/api": 42, "acme/web": 42, "other/lib": 7},
		slug:     "acme-agent",
		users: map[string]string{
			"acme-agent[bot]": `{"login":"acme-agent[bot]","id":55501,"type":"Bot"}`,
			"plain-user[bot]": `{"login":"plain-user[bot]","id":77,"type":"User"}`,
		},
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func TestTokenMintsScopedToken(t *testing.T) {
	f, srv := newFake(t)
	app := &App{ID: 1234, Key: key(t), Repos: []string{"acme/api", "acme/web"}, API: srv.URL}
	tok, err := app.Token(context.Background(), WritePermissions)
	if err != nil {
		t.Fatal(err)
	}
	if tok != "ghs_minted" {
		t.Fatalf("token %q", tok)
	}
	want := []string{"GET /repos/acme/api/installation", "GET /repos/acme/web/installation", "POST /app/installations/42/access_tokens"}
	if !slices.Equal(f.paths, want) {
		t.Fatalf("calls %q", f.paths)
	}
	b, _ := json.Marshal(f.bodies[0])
	if string(b) != `{"permissions":{"contents":"write","pull_requests":"write"},"repositories":["api","web"]}` {
		t.Fatalf("body %s", b)
	}
}

func TestTokenAsksForThePermissionsGiven(t *testing.T) {
	f, srv := newFake(t)
	app := &App{ID: 1234, Key: key(t), Repos: []string{"acme/api"}, API: srv.URL}
	if _, err := app.Token(context.Background(), ReadPermissions); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(f.bodies[0])
	if string(b) != `{"permissions":{"contents":"read"},"repositories":["api"]}` {
		t.Fatalf("body %s", b)
	}
	if _, err := app.Token(context.Background(), nil); err == nil || len(f.bodies) != 1 {
		t.Fatalf("minted without permissions: %v", err)
	}
}

func TestTokenRefusesTwoInstallations(t *testing.T) {
	f, srv := newFake(t)
	app := &App{ID: 1234, Key: key(t), Repos: []string{"acme/api", "other/lib"}, API: srv.URL}
	_, err := app.Token(context.Background(), WritePermissions)
	if err == nil || !strings.Contains(err.Error(), "span installations 42 and 7") {
		t.Fatalf("err %v", err)
	}
	if len(f.bodies) != 0 {
		t.Fatal("minted anyway")
	}
}

func TestTokenErrorsCarryNoSecret(t *testing.T) {
	f, srv := newFake(t)
	f.status = http.StatusUnprocessableEntity
	app := &App{ID: 1234, Key: key(t), Repos: []string{"acme/api"}, API: srv.URL}
	jwt, _ := app.JWT()
	_, err := app.Token(context.Background(), WritePermissions)
	if err == nil || !strings.Contains(err.Error(), "422") || !strings.Contains(err.Error(), "permissions requested") {
		t.Fatalf("err %v", err)
	}
	sig := jwt[strings.LastIndex(jwt, ".")+1:]
	if strings.Contains(err.Error(), sig[:20]) || strings.Contains(err.Error(), "Bearer") {
		t.Fatalf("error leaks the JWT: %v", err)
	}

	wrongApp := &App{ID: 999, Key: key(t), Repos: []string{"acme/api"}, API: srv.URL}
	if _, err := wrongApp.Token(context.Background(), WritePermissions); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("wrong app id: %v", err)
	}
	missing := &App{ID: 1234, Key: key(t), Repos: []string{"acme/ghost"}, API: srv.URL}
	if _, err := missing.Token(context.Background(), WritePermissions); err == nil || !strings.Contains(err.Error(), "acme/ghost") {
		t.Fatalf("missing repo: %v", err)
	}
	for _, repos := range [][]string{nil, {"noslash"}, {"a/b/c"}} {
		app := &App{ID: 1234, Key: key(t), Repos: repos, API: srv.URL}
		if _, err := app.Token(context.Background(), WritePermissions); err == nil {
			t.Errorf("repos %q: minted", repos)
		}
	}
}

func TestTokenUnreachable(t *testing.T) {
	app := &App{ID: 1234, Key: key(t), Repos: []string{"acme/api"}, API: "http://127.0.0.1:1"}
	if _, err := app.Token(context.Background(), WritePermissions); err == nil {
		t.Fatal("no error")
	}
}

func TestParseKeyFormats(t *testing.T) {
	k := key(t)
	pkcs1 := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8 := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	for name, b := range map[string][]byte{"pkcs1": pkcs1, "pkcs8": pkcs8} {
		got, err := ParseKey(b)
		if err != nil || !got.Equal(k) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, b := range map[string][]byte{
		"empty":  nil,
		"public": pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte("x")}),
		"junk":   pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: []byte("x")}),
	} {
		if _, err := ParseKey(b); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}

func TestLoadKeyRefusesLoosePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.pem")
	pkcs1 := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key(t))})
	if err := os.WriteFile(path, pkcs1, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKey(path); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("0644: %v", err)
	}
	os.Chmod(path, 0o640)
	if _, err := LoadKey(path); err == nil {
		t.Fatal("0640 loaded")
	}
	os.Chmod(path, 0o600)
	if _, err := LoadKey(path); err != nil {
		t.Fatal(err)
	}
	os.Chmod(path, 0o400)
	if _, err := LoadKey(path); err != nil {
		t.Fatal(err)
	}
}

func TestIdentityIsTheAppsBotUser(t *testing.T) {
	f, srv := newFake(t)
	app := &App{ID: 1234, Key: key(t), Repos: []string{"acme/api"}, API: srv.URL}
	for range 3 {
		id, err := app.Identity(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if id.Name != "acme-agent[bot]" || id.Email != "55501+acme-agent[bot]@users.noreply.github.com" {
			t.Fatalf("identity %+v", id)
		}
	}
	if want := []string{"GET /app", "GET /users/acme-agent[bot]"}; !slices.Equal(f.paths, want) {
		t.Fatalf("calls %q, want %q once each", f.paths, want)
	}
	if !slices.Equal(f.userAuth, []string{""}) {
		t.Fatalf("users call sent Authorization %q", f.userAuth)
	}
}

func TestIdentityFailuresAreRetried(t *testing.T) {
	f, srv := newFake(t)
	f.userCode = http.StatusForbidden
	app := &App{ID: 1234, Key: key(t), Repos: []string{"acme/api"}, API: srv.URL}
	_, err := app.Identity(context.Background())
	if err == nil || !strings.Contains(err.Error(), "rate limit") || Permanent(err) {
		t.Fatalf("err %v, permanent %v", err, Permanent(err))
	}
	f.userCode = 0
	id, err := app.Identity(context.Background())
	if err != nil || id.Name != "acme-agent[bot]" {
		t.Fatalf("retry: %+v %v", id, err)
	}
	if len(f.paths) != 4 {
		t.Fatalf("calls %q", f.paths)
	}
}

func TestIdentityRefusals(t *testing.T) {
	f, srv := newFake(t)
	wrongApp := &App{ID: 999, Key: key(t), API: srv.URL}
	if _, err := wrongApp.Identity(context.Background()); err == nil || !strings.Contains(err.Error(), "401") || !Permanent(err) {
		t.Fatalf("wrong app id: %v", err)
	}
	f.slug = "ghost-agent"
	app := &App{ID: 1234, Key: key(t), API: srv.URL}
	if _, err := app.Identity(context.Background()); err == nil || !strings.Contains(err.Error(), "404") || !Permanent(err) {
		t.Fatalf("missing bot user: %v", err)
	}
	f.slug = "plain-user"
	if _, err := app.Identity(context.Background()); !errors.Is(err, ErrNotBot) || !Permanent(err) {
		t.Fatalf("user not a bot: %v", err)
	}
	f.slug = ""
	if _, err := app.Identity(context.Background()); err == nil || !strings.Contains(err.Error(), "no slug") {
		t.Fatalf("empty slug: %v", err)
	}
	unreachable := &App{ID: 1234, Key: key(t), API: "http://127.0.0.1:1"}
	if _, err := unreachable.Identity(context.Background()); err == nil || Permanent(err) {
		t.Fatalf("unreachable: %v", err)
	}
}
