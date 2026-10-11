package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	api "git.cluster.aimingupward.com/steve/agent-project-controller/api/v1alpha1"
	"git.cluster.aimingupward.com/steve/agent-project-controller/internal/forgejo"
	"k8s.io/apimachinery/pkg/runtime"
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
