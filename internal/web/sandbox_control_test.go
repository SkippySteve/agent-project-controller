package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExistingSandboxesShareAuthenticationAndKeepCSRFFence(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("X-authentik-username") != "legacy-operator" || r.Header.Get("Authorization") != "" || r.Header.Get("X-authentik-groups") != "" || r.Header.Get("X-Forwarded-User") != "" {
			t.Error("identity boundary leaked", r.Header)
		}
		if _, err := r.Cookie("console-session"); err == nil {
			t.Error("console cookie forwarded")
		}
		if r.Method == "POST" {
			if r.Header.Get("Origin") != "https://old.example.com" {
				t.Error("incorrect backend origin")
			}
			cookie, err := r.Cookie("__Host-sandbox-csrf")
			if err != nil || cookie.Value != "fixture-csrf" || r.ParseForm() != nil || r.Form.Get("csrf") != "fixture-csrf" {
				http.Error(w, "CSRF denied", 403)
				return
			}
			http.Redirect(w, r, "/?workspace=shared-pilot&tab=access", 303)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "__Host-sandbox-csrf", Value: "fixture-csrf", Secure: true, HttpOnly: true, Path: "/"})
		http.SetCookie(w, &http.Cookie{Name: "backend-session", Value: "must-not-forward", Path: "/"})
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, `<!doctype html><html><head><script src="/static/htmx.min.js"></script></head><body><header><h1>Sandbox control</h1></header><main id="workspace-view" hx-get="/status?workspace=shared-pilot"><a href="/?workspace=shared-pilot&amp;tab=access">Access</a><form method="post" action="/access"><input name="csrf" value="fixture-csrf"></form><a href="https://preview.example.com/">Preview</a></main></body></html>`)
	}))
	defer upstream.Close()
	s, _, _ := accessFixture(t)
	s.SandboxControl = &SandboxControl{URL: upstream.URL, Origin: "https://old.example.com", Operator: "legacy-operator"}
	h, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path, origin, body string, auth bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if auth {
			req.SetBasicAuth("operator", s.Password)
		}
		req.Header.Set("X-authentik-username", "spoofed-root")
		req.Header.Set("X-authentik-groups", "admin")
		req.Header.Set("X-Forwarded-User", "spoofed-root")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if method == "POST" {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		req.AddCookie(&http.Cookie{Name: "console-session", Value: "private"})
		req.AddCookie(&http.Cookie{Name: "__Host-sandbox-csrf", Value: "fixture-csrf"})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	rec := request("GET", "/sandboxes/", "", "", false)
	if rec.Code != 401 || calls != 0 {
		t.Fatal("unauthenticated forwarding", rec.Code, calls)
	}
	rec = request("GET", "/sandboxes/?workspace=shared-pilot&tab=access", "", "", true)
	if rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	for _, want := range []string{`action="/sandboxes/access"`, `src="/sandboxes/static/htmx.min.js"`, `hx-get="/sandboxes/status?workspace=shared-pilot"`, `href="/sandboxes/?workspace=shared-pilot&amp;tab=access"`, `href="/"`, `href="/console.css"`, `src="/shell.js"`, `https://preview.example.com/`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatal("mount lost control", want, rec.Body.String())
		}
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != "__Host-sandbox-csrf" {
		t.Fatal("backend cookie leak", cookies)
	}
	before := calls
	for _, origin := range []string{"", "https://evil.example"} {
		rec = request("POST", "/sandboxes/access", origin, "csrf=fixture-csrf", true)
		if rec.Code != 403 || calls != before {
			t.Fatal("cross-origin mutation forwarded", rec.Code, calls)
		}
	}
	rec = request("POST", "/sandboxes/access", s.Origin, "csrf=wrong", true)
	if rec.Code != 403 {
		t.Fatal("backend CSRF bypassed", rec.Code)
	}
	rec = request("POST", "/sandboxes/access", s.Origin, "csrf=fixture-csrf", true)
	if rec.Code != 303 || rec.Header().Get("Location") != "/sandboxes/?workspace=shared-pilot&tab=access" {
		t.Fatal("redirect leaves app", rec.Code, rec.Header())
	}
	before = calls
	for _, path := range []string{"/sandboxes/auth/login", "/sandboxes/admin", "/sandboxes/healthz"} {
		rec = request("GET", path, "", "", true)
		if rec.Code != 404 || calls != before {
			t.Fatal("unexpected backend route", path, rec.Code)
		}
	}
}

func TestSandboxMountRejectsExternalRedirectsAndInvalidConfiguration(t *testing.T) {
	for _, config := range []SandboxControl{{URL: "http://user:password@backend", Origin: "https://old.example", Operator: "operator"}, {URL: "http://backend/path", Origin: "https://old.example", Operator: "operator"}, {URL: "http://backend", Origin: "https://old.example"}, {URL: "file:///etc/passwd", Origin: "https://old.example", Operator: "operator"}} {
		if config.Validate() == nil {
			t.Fatal("invalid configuration", config)
		}
	}
	config := SandboxControl{URL: "http://backend", Origin: "https://old.example", Operator: "operator"}
	for _, location := range []string{"https://evil.example/", "//evil.example/", "https://old.example.evil/"} {
		if _, err := config.mountURL(location); err == nil {
			t.Fatal("external redirect", location)
		}
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "https://evil.example/", 303) }))
	defer upstream.Close()
	s, _, _ := accessFixture(t)
	s.SandboxControl = &SandboxControl{URL: upstream.URL, Origin: "https://old.example", Operator: "operator"}
	h, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/sandboxes/", nil)
	req.SetBasicAuth("operator", s.Password)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 502 || rec.Header().Get("Location") != "" {
		t.Fatal("external redirect forwarded", rec.Code, rec.Header())
	}
}
