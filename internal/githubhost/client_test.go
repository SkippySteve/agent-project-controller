package githubhost

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
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"git.cluster.aimingupward.com/steve/agent-project-controller/internal/forgejo"
)

var testKey *rsa.PrivateKey

func TestMain(m *testing.M) {
	var err error
	testKey, err = rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

func pemPKCS1(key *rsa.PrivateKey) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

type fakeGitHub struct {
	mu               sync.Mutex
	now              func() time.Time
	appID            string
	installation     string
	slug             string
	tokens           map[string]string
	next             int
	workspaceMints   int
	controllerMints  int
	maxPage          int
	protectionStatus int
	protection       map[string]any
	repo             map[string]any
	extraPermission  bool
	extraRepo        bool
	selection        string
	expiresIn        time.Duration
	emptyToken       bool
	redirects        int
	leak             bool
	keepAfterDelete  bool
	unauthorizedOnce int
	calls            int
	problems         []string
}

func (f *fakeGitHub) problem(message string) {
	f.problems = append(f.problems, message)
}

func newFake(t *testing.T) (*fakeGitHub, *httptest.Server, *Client) {
	t.Helper()
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	fake := &fakeGitHub{
		now:              func() time.Time { return now },
		appID:            "12345",
		installation:     "99",
		slug:             "workspace-app",
		tokens:           map[string]string{},
		protectionStatus: http.StatusOK,
		expiresIn:        time.Hour,
		protection: map[string]any{
			"enforce_admins":                map[string]any{"enabled": true},
			"required_pull_request_reviews": map[string]any{"required_approving_review_count": 1},
			"restrictions":                  map[string]any{"users": []any{map[string]any{"login": "octocat"}}, "teams": []any{}, "apps": []any{map[string]any{"slug": "other-app"}}},
			"allow_force_pushes":            map[string]any{"enabled": false},
			"allow_deletions":               map[string]any{"enabled": false},
		},
		repo: map[string]any{"full_name": "owner/demo", "default_branch": "main", "archived": false, "disabled": false, "private": true},
	}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	client, err := New("owner", 12345, 99, pemPKCS1(testKey))
	if err != nil {
		t.Fatal(err)
	}
	client.api = server.URL
	client.now = func() time.Time { return fake.now() }
	return fake, server, client
}

func (f *fakeGitHub) advance(d time.Duration) {
	current := f.now()
	f.now = func() time.Time { return current.Add(d) }
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.redirects > 0 {
		f.redirects--
		w.Header().Set("Location", "https://evil.example/steal")
		w.WriteHeader(http.StatusFound)
		return
	}
	if r.Header.Get("Accept") != "application/vnd.github+json" || r.Header.Get("X-GitHub-Api-Version") != apiVersion || r.Header.Get("User-Agent") != userAgent {
		f.problem("missing GitHub API headers")
	}
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if strings.Count(bearer, ".") == 2 {
		f.verifyJWT(bearer)
	}
	if f.leak {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "token=ghs_supersecret")
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/app/installations/"+f.installation+"/access_tokens":
		f.mint(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/app":
		_ = json.NewEncoder(w).Encode(map[string]string{"slug": f.slug})
	case r.Method == http.MethodDelete && r.URL.Path == "/installation/token":
		if !f.keepAfterDelete {
			delete(f.tokens, bearer)
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && r.URL.Path == "/installation/repositories":
		f.catalog(w, r, bearer)
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/branches/main/protection"):
		if !f.live(bearer, "controller") {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(f.protectionStatus)
		if f.protectionStatus == http.StatusOK {
			_ = json.NewEncoder(w).Encode(f.protection)
		}
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/"):
		f.repository(w, bearer)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeGitHub) verifyJWT(token string) {
	parts := strings.Split(token, ".")
	headerRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		f.problem("jwt header")
		return
	}
	var header map[string]string
	if json.Unmarshal(headerRaw, &header) != nil || header["alg"] != "RS256" {
		f.problem("jwt alg")
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || rsa.VerifyPKCS1v15(&testKey.PublicKey, crypto.SHA256, sum[:], sig) != nil {
		f.problem("jwt signature")
		return
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		f.problem("jwt payload")
		return
	}
	var claims jwtClaims
	if json.Unmarshal(payload, &claims) != nil || claims.Issuer != f.appID {
		f.problem("jwt issuer")
		return
	}
	if claims.IssuedAt != f.now().Add(-jwtSkew).Unix() || claims.ExpiresAt != f.now().Add(jwtLifetime).Unix() {
		f.problem("jwt lifetime")
	}
}

func (f *fakeGitHub) live(token, kind string) bool {
	if f.unauthorizedOnce > 0 && kind == "controller" && f.tokens[token] == "controller" {
		f.unauthorizedOnce--
		return false
	}
	return f.tokens[token] == kind
}

func (f *fakeGitHub) mint(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if json.Unmarshal(raw, &body) != nil {
		f.problem("token body")
	}
	f.next++
	token := "ghs_" + strings.Repeat("a", 8) + strings.Repeat("b", f.next)
	kind := "controller"
	selection := "all"
	lifetime := time.Hour
	var repos []map[string]string
	if len(body.Repositories) > 0 {
		kind = "owner/demo"
		selection = "selected"
		lifetime = f.expiresIn
		f.workspaceMints++
		if strings.Contains(string(raw), "administration") {
			f.problem("workspace token requested administration")
		}
		if len(body.Repositories) != 1 || body.Repositories[0] != "demo" || body.Permissions["contents"] != "write" || body.Permissions["pull_requests"] != "write" || body.Permissions["metadata"] != "read" || len(body.Permissions) != 3 {
			f.problem("workspace token request was not limited to one repository")
		}
		repos = []map[string]string{{"full_name": "owner/demo"}}
		if f.extraRepo {
			repos = append(repos, map[string]string{"full_name": "owner/other"})
		}
	} else {
		f.controllerMints++
		if strings.Contains(string(raw), "repositories") || strings.Contains(string(raw), "contents") {
			f.problem("controller token was scoped like a workspace token")
		}
		if body.Permissions["administration"] != "read" || body.Permissions["metadata"] != "read" || len(body.Permissions) != 2 {
			f.problem("controller token permissions")
		}
	}
	permissions := map[string]string{}
	for key, value := range body.Permissions {
		permissions[key] = value
	}
	if f.extraPermission && kind == "owner/demo" {
		permissions["administration"] = "write"
	}
	if f.selection != "" && kind == "owner/demo" {
		selection = f.selection
	}
	if f.emptyToken && kind == "owner/demo" {
		token = ""
	} else {
		f.tokens[token] = kind
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"token": token, "expires_at": f.now().Add(lifetime).Format(time.RFC3339),
		"permissions": permissions, "repository_selection": selection, "repositories": repos,
	})
}

func (f *fakeGitHub) catalog(w http.ResponseWriter, r *http.Request, bearer string) {
	if !f.live(bearer, "controller") {
		http.Error(w, "no", http.StatusUnauthorized)
		return
	}
	f.maxPage++
	if r.URL.Query().Get("per_page") != "100" {
		f.problem("catalog page size")
	}
	repos := []map[string]any{}
	if r.URL.Query().Get("page") == "1" {
		for i := 0; i < 100; i++ {
			repos = append(repos, map[string]any{"full_name": "owner/repo-" + strings.Repeat("n", 2) + string(rune('a'+i%26)), "private": true})
		}
	} else {
		repos = []map[string]any{{"full_name": "other/hidden", "private": false}, {"full_name": "owner/last", "private": false}}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 102, "repositories": repos})
}

func (f *fakeGitHub) repository(w http.ResponseWriter, bearer string) {
	kind := f.tokens[bearer]
	if kind != "controller" && kind != "owner/demo" {
		http.Error(w, "no", http.StatusUnauthorized)
		return
	}
	if f.unauthorizedOnce > 0 && kind == "controller" {
		f.unauthorizedOnce--
		http.Error(w, "no", http.StatusUnauthorized)
		return
	}
	_ = json.NewEncoder(w).Encode(f.repo)
}

func (f *fakeGitHub) done(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, problem := range f.problems {
		t.Error(problem)
	}
}

func TestNewAcceptsPKCS1AndPKCS8(t *testing.T) {
	if _, err := New("owner", 12345, 99, pemPKCS1(testKey)); err != nil {
		t.Fatal(err)
	}
	raw, err := x509.MarshalPKCS8PrivateKey(testKey)
	if err != nil {
		t.Fatal(err)
	}
	client, err := New("owner", 12345, 99, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: raw}))
	if err != nil {
		t.Fatal(err)
	}
	if client.api != apiBase {
		t.Fatal("API base must stay GitHub.com")
	}
	short, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = New("owner", 12345, 99, pemPKCS1(short)); err == nil || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatal(err)
	}
	if _, err = New("owner", 0, 99, pemPKCS1(testKey)); err == nil {
		t.Fatal("non-positive app id accepted")
	}
}

func TestGrantReusesAndRefreshesOneRepositoryToken(t *testing.T) {
	fake, _, client := newFake(t)
	defer fake.done(t)
	ctx := context.Background()
	first, err := client.Grant(ctx, "owner/demo", forgejo.Credentials{Username: "apc-1"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.Grant(ctx, "owner/demo", forgejo.Credentials{Username: "apc-1", Token: first})
	if err != nil || second != first {
		t.Fatal(err)
	}
	if fake.workspaceMints != 1 || fake.controllerMints != 1 {
		t.Fatalf("mints workspace=%d controller=%d", fake.workspaceMints, fake.controllerMints)
	}
	fake.advance(50 * time.Minute)
	third, err := client.Grant(ctx, "owner/demo", forgejo.Credentials{Username: "apc-1", Token: first})
	if err != nil || third == first {
		t.Fatal(err)
	}
	if code := getRepo(t, fake, first); code != http.StatusUnauthorized {
		t.Fatalf("old token status %d", code)
	}
	if code := getRepo(t, fake, third); code != http.StatusOK {
		t.Fatalf("new token status %d", code)
	}
}

func TestGrantRejectsTheWrongRepositoryAndWeakProtection(t *testing.T) {
	ctx := context.Background()
	fake, _, client := newFake(t)
	if _, err := client.Grant(ctx, "other/demo", forgejo.Credentials{}); err == nil || fake.calls != 0 {
		t.Fatal(err, fake.calls)
	}
	if _, err := client.Grant(ctx, "owner/../demo", forgejo.Credentials{}); err == nil || fake.calls != 0 {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		mutate  func(*fakeGitHub)
		message string
	}{
		{"missing", func(f *fakeGitHub) { f.protectionStatus = http.StatusNotFound }, "rulesets alone are not accepted"},
		{"admins", func(f *fakeGitHub) { f.protection["enforce_admins"] = map[string]any{"enabled": false} }, "restrict pushes"},
		{"reviews", func(f *fakeGitHub) { f.protection["required_pull_request_reviews"] = nil }, "restrict pushes"},
		{"zero reviews", func(f *fakeGitHub) {
			f.protection["required_pull_request_reviews"] = map[string]any{"required_approving_review_count": 0}
		}, "restrict pushes"},
		{"open push", func(f *fakeGitHub) { f.protection["restrictions"] = nil }, "restrict pushes"},
		{"force", func(f *fakeGitHub) { f.protection["allow_force_pushes"] = map[string]any{"enabled": true} }, "restrict pushes"},
		{"delete", func(f *fakeGitHub) { f.protection["allow_deletions"] = map[string]any{"enabled": true} }, "restrict pushes"},
		{"app allowed", func(f *fakeGitHub) {
			f.protection["restrictions"] = map[string]any{"apps": []any{map[string]any{"slug": "workspace-app"}}}
		}, "must not allow this GitHub App"},
		{"archived", func(f *fakeGitHub) { f.repo["archived"] = true }, "default branch main"},
		{"disabled", func(f *fakeGitHub) { f.repo["disabled"] = true }, "default branch main"},
		{"other branch", func(f *fakeGitHub) { f.repo["default_branch"] = "master" }, "default branch main"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake, _, client := newFake(t)
			defer fake.done(t)
			tc.mutate(fake)
			if _, err := client.Grant(ctx, "owner/demo", forgejo.Credentials{}); err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatal(err)
			}
			if fake.workspaceMints != 0 {
				t.Fatal("workspace token minted without protection")
			}
		})
	}
}

func TestGrantDiscardsUnexpectedTokenScope(t *testing.T) {
	ctx := context.Background()
	for _, mutate := range []func(*fakeGitHub){
		func(f *fakeGitHub) { f.extraPermission = true },
		func(f *fakeGitHub) { f.extraRepo = true },
		func(f *fakeGitHub) { f.selection = "all" },
		func(f *fakeGitHub) { f.expiresIn = 5 * time.Minute },
		func(f *fakeGitHub) { f.emptyToken = true },
	} {
		fake, _, client := newFake(t)
		defer fake.done(t)
		mutate(fake)
		token, err := client.Grant(ctx, "owner/demo", forgejo.Credentials{})
		if err == nil || token != "" {
			t.Fatal(err)
		}
		if strings.Contains(err.Error(), "ghs_") || strings.Contains(err.Error(), "supersecret") {
			t.Fatal(err)
		}
		fake.mu.Lock()
		live := 0
		for _, kind := range fake.tokens {
			if kind == "owner/demo" {
				live++
			}
		}
		fake.mu.Unlock()
		if live != 0 {
			t.Fatalf("rejected workspace token still live")
		}
	}
}

func TestRevokeConfirmsAndInterruptedGrantStaysUnlisted(t *testing.T) {
	ctx := context.Background()
	fake, server, client := newFake(t)
	defer fake.done(t)
	token, err := client.Grant(ctx, "owner/demo", forgejo.Credentials{Username: "apc-1"})
	if err != nil {
		t.Fatal(err)
	}
	if err = client.Revoke(ctx, "owner/demo", forgejo.Credentials{}); err != nil {
		t.Fatal(err)
	}
	if code := getRepo(t, fake, token); code != http.StatusUnauthorized {
		t.Fatalf("in-process unsaved token status %d", code)
	}
	token, err = client.Grant(ctx, "owner/demo", forgejo.Credentials{Username: "apc-1"})
	if err != nil {
		t.Fatal(err)
	}
	crashed, err := New("owner", 12345, 99, pemPKCS1(testKey))
	if err != nil {
		t.Fatal(err)
	}
	crashed.api = server.URL
	crashed.now = client.now
	if err = crashed.Revoke(ctx, "owner/demo", forgejo.Credentials{}); err != nil {
		t.Fatal(err)
	}
	if code := getRepo(t, fake, token); code != http.StatusOK {
		t.Fatalf("token lost in a crash status %d", code)
	}
	restarted, err := New("owner", 12345, 99, pemPKCS1(testKey))
	if err != nil {
		t.Fatal(err)
	}
	restarted.api = server.URL
	restarted.now = client.now
	replacement, err := restarted.Grant(ctx, "owner/demo", forgejo.Credentials{Username: "apc-1", Token: token})
	if err != nil || replacement == token {
		t.Fatal(err)
	}
	if code := getRepo(t, fake, token); code != http.StatusUnauthorized {
		t.Fatalf("stored token after restart status %d", code)
	}
	fake.keepAfterDelete = true
	if err = restarted.Revoke(ctx, "owner/demo", forgejo.Credentials{Token: replacement}); err == nil || !strings.Contains(err.Error(), "revocation not confirmed") {
		t.Fatal(err)
	}
	if code := getRepo(t, fake, replacement); code != http.StatusOK {
		t.Fatalf("unconfirmed token status %d", code)
	}
}

func TestDiscoverFiltersOwnerAndStopsAtTheCatalogCap(t *testing.T) {
	fake, _, client := newFake(t)
	defer fake.done(t)
	repos, err := client.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 101 || repos[len(repos)-1].FullName != "owner/last" || repos[len(repos)-1].Private {
		t.Fatalf("catalog %+v", repos[len(repos)-1])
	}
	for _, repo := range repos {
		if !strings.HasPrefix(repo.FullName, "owner/") {
			t.Fatal(repo.FullName)
		}
	}
	if fake.maxPage != 2 {
		t.Fatal(fake.maxPage)
	}
	capped, _, client := newFake(t)
	always := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/installation/repositories" {
			repos := make([]map[string]string, 100)
			for i := range repos {
				repos[i] = map[string]string{"full_name": "owner/repo"}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"repositories": repos})
			return
		}
		capped.ServeHTTP(w, r)
	}))
	defer always.Close()
	client.api = always.URL
	if _, err = client.Discover(context.Background()); err == nil || !strings.Contains(err.Error(), "pagination limit") {
		t.Fatal(err)
	}
}

func TestGitHubErrorsHideResponseBodiesAndRedirects(t *testing.T) {
	fake, _, client := newFake(t)
	defer fake.done(t)
	fake.leak = true
	_, err := client.Grant(context.Background(), "owner/demo", forgejo.Credentials{})
	if err == nil || strings.Contains(err.Error(), "ghs_supersecret") || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatal(err)
	}
	redirected, _, client := newFake(t)
	redirected.redirects = 1
	_, err = client.Grant(context.Background(), "owner/demo", forgejo.Credentials{})
	if err == nil || !strings.Contains(err.Error(), "HTTP 302") {
		t.Fatal(err)
	}
}

func TestControllerTokenRetriesOnceAfterUnauthorized(t *testing.T) {
	fake, _, client := newFake(t)
	defer fake.done(t)
	fake.unauthorizedOnce = 1
	if _, err := client.Grant(context.Background(), "owner/demo", forgejo.Credentials{}); err != nil {
		t.Fatal(err)
	}
	if fake.controllerMints < 2 {
		t.Fatalf("controller mints %d", fake.controllerMints)
	}
}

func getRepo(t *testing.T, fake *fakeGitHub, token string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "https://api.github.com/repos/owner/demo", nil)
	if err != nil {
		t.Fatal(err)
	}
	rec := &statusRecorder{}
	fake.ServeHTTP(rec, reqWithToken(req, token))
	return rec.code
}

type statusRecorder struct{ code int }

func (s *statusRecorder) Header() http.Header { return http.Header{} }
func (s *statusRecorder) Write(p []byte) (int, error) {
	if s.code == 0 {
		s.code = http.StatusOK
	}
	return len(p), nil
}
func (s *statusRecorder) WriteHeader(code int) { s.code = code }

func reqWithToken(req *http.Request, token string) *http.Request {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", userAgent)
	return req
}
