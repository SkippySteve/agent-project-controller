package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	api "git.cluster.aimingupward.com/steve/agent-project-controller/api/v1alpha1"
	"git.cluster.aimingupward.com/steve/agent-project-controller/internal/forgejo"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type host struct{}

func (host) Discover(context.Context) ([]forgejo.Repository, error) {
	return []forgejo.Repository{{FullName: "steve/repo"}}, nil
}
func (host) Grant(context.Context, string, forgejo.Credentials) (string, error) {
	panic("HTTP API must not directly grant")
}
func (host) Revoke(context.Context, string, forgejo.Credentials) error {
	panic("HTTP API must not directly revoke")
}
func TestAuthenticationCSRFAndCreation(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = api.AddToScheme(scheme)
	password := strings.Repeat("a", 32)
	s := &Server{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), Host: host{}, Namespace: "control", Owner: "steve", Password: password, Origin: "https://projects.example.com", AllowBasic: true}
	h, e := s.Handler()
	if e != nil {
		t.Fatal(e)
	}
	tests := []struct {
		auth, origin, body string
		status             int
	}{{"", "", `{"name":"repo","repository":"steve/repo"}`, 401}, {"basic", "", `{"name":"repo","repository":"steve/repo"}`, 403}, {"basic", "https://evil.example", `{"name":"repo","repository":"steve/repo"}`, 403}, {"bearer", "", `{"name":"repo","repository":"other/repo"}`, 400}, {"bearer", "", `{"name":"repo","repository":"steve/repo"}`, 201}, {"bearer", "", `{"name":"repo","repository":"steve/repo"}`, 409}}
	for _, test := range tests {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces", strings.NewReader(test.body))
		req.Header.Set("Content-Type", "application/json")
		if test.auth == "basic" {
			req.SetBasicAuth("operator", password)
		} else if test.auth == "bearer" {
			req.Header.Set("Authorization", "Bearer "+password)
		}
		if test.origin != "" {
			req.Header.Set("Origin", test.origin)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != test.status {
			t.Errorf("%s %s: got %d: %s", test.auth, test.origin, rec.Code, rec.Body.String())
		}
	}
}

func TestRegistrySelection(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = api.AddToScheme(scheme)
	password := strings.Repeat("a", 32)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	s := &Server{Client: c, Host: host{}, Namespace: "control", Owner: "steve", Password: password, Origin: "https://projects.example.com", AllowBasic: true}
	h, e := s.Handler()
	if e != nil {
		t.Fatal(e)
	}
	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+password)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	for _, body := range []string{
		`{"name":"bad-registry","repository":"steve/repo","registries":["maven"]}`,
		`{"name":"repeated","repository":"steve/repo","registries":["go","go"]}`,
		`{"name":"duplicate-host","repository":"steve/repo","registries":["go"],"https":[{"host":"proxy.golang.org","methods":["GET"],"paths":["/"]}]}`,
	} {
		if rec := post(body); rec.Code != 400 {
			t.Fatalf("%s: got %d %s", body, rec.Code, rec.Body.String())
		}
	}
	rec := post(`{"name":"go-project","repository":"steve/repo","registries":["go"]}`)
	if rec.Code != 201 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	var created api.ProjectWorkspace
	if e = json.Unmarshal(rec.Body.Bytes(), &created); e != nil {
		t.Fatal(e)
	}
	if len(created.Spec.HTTPS) != 2 || created.Spec.HTTPS[0].Host != "proxy.golang.org" || created.Spec.HTTPS[1].Host != "sum.golang.org" {
		t.Fatalf("rules = %#v", created.Spec.HTTPS)
	}
	for _, rule := range created.Spec.HTTPS {
		if len(rule.Methods) != 2 || rule.Methods[0] != "GET" || rule.Methods[1] != "HEAD" || len(rule.Paths) != 1 || rule.Paths[0] != "/" {
			t.Fatalf("rule = %#v", rule)
		}
	}
	stored := &api.ProjectWorkspace{}
	if e = c.Get(context.Background(), types.NamespacedName{Namespace: "control", Name: "go-project"}, stored); e != nil {
		t.Fatal(e)
	}
	if len(stored.Spec.HTTPS) != 2 {
		t.Fatal("stored policy dropped registry hosts")
	}
}

func TestOIDCDoesNotEnableEmptyStaticPassword(t *testing.T) {
	s := &Server{OIDC: &OIDC{}, Origin: "https://projects.example.com", AllowBasic: true}
	if _, e := s.Handler(); e == nil {
		t.Fatal("OIDC allowed an explicitly enabled static login with no secret")
	}
	s.AllowBasic = false
	h, e := s.Handler()
	if e != nil {
		t.Fatal(e)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/repositories", nil)
	req.SetBasicAuth("operator", "")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatal("empty password bypassed OIDC")
	}
}

func TestSignedOutPageDoesNotRestartSSOOrExposeAPI(t *testing.T) {
	s := &Server{Origin: "https://projects.example.com", OIDC: &OIDC{}}
	h, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	out := httptest.NewRecorder()
	h.ServeHTTP(out, httptest.NewRequest("GET", "/auth/signed-out", nil))
	if out.Code != 200 || out.Header().Get("Location") != "" || !strings.Contains(out.Body.String(), "Your dashboard session has ended") {
		t.Fatal("signed-out page redirects to SSO")
	}
	out = httptest.NewRecorder()
	h.ServeHTTP(out, httptest.NewRequest("GET", "/api/v1/workspaces", nil))
	if out.Code != 401 {
		t.Fatal("signed-out client can access projects")
	}
	if !strings.Contains(string(app), "location.assign('/auth/signed-out')") {
		t.Fatal("logout reloads the protected dashboard")
	}
}
