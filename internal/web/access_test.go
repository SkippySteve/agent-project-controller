package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	api "git.cluster.aimingupward.com/steve/agent-project-controller/api/v1alpha1"
	"git.cluster.aimingupward.com/steve/agent-project-controller/internal/controller"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func accessFixture(t *testing.T) (*Server, http.Handler, *api.ProjectWorkspace) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = api.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	obj := &api.ProjectWorkspace{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: "control", UID: types.UID("fixture-uid"), Generation: 1}, Spec: api.WorkspaceSpec{Repository: "steve/repo", DesiredState: "Paused"}, Status: api.WorkspaceStatus{Phase: "Paused", Namespace: "apc-fixtureuid", ObservedGeneration: 1}}
	ledger := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "apc-fixtureuid", Namespace: "control", Labels: map[string]string{api.OwnerLabel: string(obj.UID)}}, Data: map[string][]byte{controller.HTTPSLedgerKey: []byte("null")}}
	s := &Server{Client: fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(obj).WithObjects(obj, ledger).Build(), Host: host{}, Namespace: "control", Owner: "steve", Password: strings.Repeat("a", 32), Origin: "https://console.example.com", GitOrigin: "https://git.example.com", AllowBasic: true}
	h, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Client.Get(context.Background(), client.ObjectKeyFromObject(obj), obj); err != nil {
		t.Fatal(err)
	}
	return s, h, obj
}

func accessRequest(h http.Handler, password, version, https string) *httptest.ResponseRecorder {
	body := `{"resourceVersion":"` + version + `","https":` + https + `}`
	req := httptest.NewRequest("PATCH", "/api/v1/workspaces/project/access", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+password)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestProjectAccessRejectsUnstoppedProcessesAndReservedHosts(t *testing.T) {
	s, h, obj := accessFixture(t)
	for _, rules := range []string{`[{"host":"git.example.com","methods":["POST"],"paths":["/"]}]`, `[{"host":"example.com","methods":["CONNECT"],"paths":["/"]}]`, `[{"host":"example.com","methods":["GET"],"paths":["/../private"]}]`, `null`} {
		rec := accessRequest(h, s.Password, obj.ResourceVersion, rules)
		if rec.Code != 400 {
			t.Fatal(rules, rec.Code, rec.Body.String())
		}
	}
	rec := accessRequest(h, s.Password, "stale", `[]`)
	if rec.Code != 409 {
		t.Fatal("stale update", rec.Code)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "still-stopping", Namespace: obj.Status.Namespace, Labels: map[string]string{api.OwnerLabel: string(obj.UID)}}}
	if err := s.Client.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	rec = accessRequest(h, s.Password, obj.ResourceVersion, `[]`)
	if rec.Code != 409 {
		t.Fatal("active process allowed", rec.Code)
	}
	if err := s.Client.Delete(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	rec = accessRequest(h, s.Password, obj.ResourceVersion, `[{"host":"api.example.com","methods":["GET"],"paths":["/v1"]}]`)
	if rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	var updated api.ProjectWorkspace
	if err := json.Unmarshal(rec.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Spec.DesiredState != "Paused" || len(updated.Spec.HTTPS) != 1 {
		t.Fatal("policy update resumed project", updated.Spec)
	}
	// The old ledger prevents a second policy change or resume before the new policy converges.
	rec = accessRequest(h, s.Password, updated.ResourceVersion, `[]`)
	if rec.Code != 409 {
		t.Fatal("unreconciled update accepted", rec.Code)
	}
	req := httptest.NewRequest("PATCH", "/api/v1/workspaces/project", strings.NewReader(`{"resourceVersion":"`+updated.ResourceVersion+`","desiredState":"Running"}`))
	req.Header.Set("Authorization", "Bearer "+s.Password)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 409 {
		t.Fatal("resumed pending policy", rec.Code)
	}
}

func TestProjectAccessAuthenticationAndPausedState(t *testing.T) {
	s, h, obj := accessFixture(t)
	rec := accessRequest(h, "incorrect", obj.ResourceVersion, `[]`)
	if rec.Code != 401 {
		t.Fatal("unauthorized access update", rec.Code)
	}
	req := httptest.NewRequest("PATCH", "/api/v1/workspaces/project/access", strings.NewReader(`{"resourceVersion":"`+obj.ResourceVersion+`","https":[]}`))
	req.SetBasicAuth("operator", s.Password)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatal("cross-origin access update", rec.Code)
	}
	obj.Spec.DesiredState = "Running"
	if err := s.Client.Update(context.Background(), obj); err != nil {
		t.Fatal(err)
	}
	rec = accessRequest(h, s.Password, obj.ResourceVersion, `[]`)
	if rec.Code != 409 {
		t.Fatal("running access edit", rec.Code)
	}
}
