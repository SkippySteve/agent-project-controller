package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"

	api "git.cluster.aimingupward.com/steve/agent-project-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestBrowserDashboard(t *testing.T) {
	if os.Getenv("APC_BROWSER_TEST") != "true" {
		t.Skip("set APC_BROWSER_TEST=true with node, Playwright and Chromium installed")
	}
	scheme := runtime.NewScheme()
	_ = api.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	blocked := &api.ProjectWorkspace{ObjectMeta: metav1.ObjectMeta{Name: "blocked-project", Namespace: "control", UID: types.UID("blocked"), Generation: 1}, Spec: api.WorkspaceSpec{Repository: "steve/repo", DesiredState: "Running"}, Status: api.WorkspaceStatus{Phase: "Preparing", ObservedGeneration: 1, Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionFalse, Reason: "PreparationPending", Message: "main branch protection needs operator review: require at least 1 approval (currently 0). Edit Settings > Branches; preparation retries automatically", ObservedGeneration: 1, LastTransitionTime: metav1.Now()}}}}
	paused := &api.ProjectWorkspace{ObjectMeta: metav1.ObjectMeta{Name: "paused-project", Namespace: "control", UID: types.UID("paused"), Generation: 1}, Spec: api.WorkspaceSpec{Repository: "steve/repo", DesiredState: "Paused"}, Status: api.WorkspaceStatus{Phase: "Paused", Namespace: "apc-paused", ObservedGeneration: 1}}
	ledger := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "apc-paused", Namespace: "control", Labels: map[string]string{api.OwnerLabel: "paused"}}, Data: map[string][]byte{"https-policy": []byte("null")}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(blocked, paused, ledger).Build()
	var legacyChanges atomic.Int32
	legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-authentik-username") != "operator" {
			http.Error(w, "unauthenticated", 401)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "__Host-sandbox-csrf", Value: "fixture-csrf", Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
		if r.Method == "POST" {
			cookie, err := r.Cookie("__Host-sandbox-csrf")
			if err != nil || cookie.Value != "fixture-csrf" || r.ParseForm() != nil || r.Form.Get("csrf") != "fixture-csrf" || r.Header.Get("Origin") != "https://legacy.example.com" {
				http.Error(w, "CSRF denied", 403)
				return
			}
			legacyChanges.Add(1)
			http.Redirect(w, r, "/?workspace=shared-pilot&tab=access", 303)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<!doctype html><html><head><title>Existing controls</title></head><body><header><h1>Existing controls</h1></header><main><nav><a href="/?workspace=shared-pilot&amp;tab=access">shared-pilot</a><a href="/?workspace=codex-a&amp;tab=access">codex-a</a><a href="/?workspace=grok-a&amp;tab=access">grok-a</a><a href="/?workspace=cursor-a&amp;tab=access">cursor-a</a></nav><h2>Existing sandbox access</h2><form method="post" action="/access"><input name="csrf" type="hidden" value="fixture-csrf"><button>Save existing access</button></form></main></body></html>`))
	}))
	defer legacy.Close()
	password := strings.Repeat("test", 8)
	server := httptest.NewUnstartedServer(nil)
	origin := "https://" + server.Listener.Addr().String()
	s := &Server{Client: c, Host: host{}, Namespace: "control", Owner: "steve", Password: password, Origin: origin, AllowBasic: true}
	s.SandboxControl = &SandboxControl{URL: legacy.URL, Origin: "https://legacy.example.com", Operator: "operator"}
	handler, e := s.Handler()
	if e != nil {
		t.Fatal(e)
	}
	server.Config.Handler = handler
	server.StartTLS()
	defer server.Close()
	cmd := exec.Command("node", "../../scripts/qualify-dashboard.cjs")
	cmd.Env = append(os.Environ(), "APC_TEST_WEB_ORIGIN="+origin, "APC_TEST_WEB_PASSWORD="+password)
	out, e := cmd.CombinedOutput()
	if e != nil {
		t.Fatalf("browser qualification failed: %v\n%s", e, out)
	}
	w := &api.ProjectWorkspace{}
	if e = c.Get(context.Background(), types.NamespacedName{Namespace: "control", Name: "browser-project"}, w); e != nil {
		t.Fatal(e)
	}
	if w.Spec.DesiredState != "Removed" {
		t.Fatal("browser removal did not persist terminal state")
	}
	if legacyChanges.Load() != 1 {
		t.Fatal("legacy sandbox mutation did not use the combined app")
	}
	if e = c.Get(context.Background(), types.NamespacedName{Namespace: "control", Name: "paused-project"}, w); e != nil {
		t.Fatal(e)
	}
	if len(w.Spec.HTTPS) != 1 || w.Spec.HTTPS[0].Host != "api.example.com" || w.Spec.DesiredState != "Paused" {
		b, _ := json.Marshal(w.Spec)
		t.Fatal("browser access update failed", string(b))
	}
	t.Log(string(out))
}
