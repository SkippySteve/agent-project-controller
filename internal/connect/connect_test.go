package connect

import (
	"context"
	"io"
	"k8s.io/client-go/rest"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	api "git.cluster.aimingupward.com/steve/agent-project-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestCommand(t *testing.T) {
	got := Command("apc-123", "worker-0")
	want := "kubectl -n apc-123 exec -it worker-0 -c workspace -- /bin/sh"
	if got != want {
		t.Fatal(got)
	}
	line, e := ExecCommand("apc-123", "project", []string{"git", "status"})
	if e != nil || line != "kubectl -n apc-123 exec project -c workspace -- git status" {
		t.Fatal(line, e)
	}
	if _, e = ExecCommand("apc-123", "project", nil); e == nil {
		t.Fatal("empty command accepted")
	}
}

func TestPodRequiresReadySingleton(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = api.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	const uid = types.UID("12345678-1234-1234-1234-123456789012")
	ns := "apc-" + strings.ReplaceAll(string(uid), "-", "")
	workspace := &api.ProjectWorkspace{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: "control", UID: uid}, Spec: api.WorkspaceSpec{Repository: "steve/project", DesiredState: "Running"}, Status: api.WorkspaceStatus{Phase: "Preparing", Namespace: ns}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(workspace).Build()
	if _, _, e := Pod(context.Background(), c, "control", "project"); e == nil || !strings.Contains(e.Error(), "not ready") {
		t.Fatal(e)
	}
	workspace.Status.Phase = "Ready"
	if e := c.Update(context.Background(), workspace); e != nil {
		t.Fatal(e)
	}
	if _, _, e := Pod(context.Background(), c, "control", "project"); e == nil || !strings.Contains(e.Error(), "exactly one") {
		t.Fatal(e)
	}
	pod := func(name string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{api.OwnerLabel: string(uid), "projects.agentwork.dev/role": "worker"}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "workspace", Image: "example.com/worker:test"}}}}
	}
	if e := c.Create(context.Background(), pod("worker-a")); e != nil {
		t.Fatal(e)
	}
	_, found, e := Pod(context.Background(), c, "control", "project")
	if e != nil || found.Name != "worker-a" {
		t.Fatal(found, e)
	}
	if e := c.Create(context.Background(), pod("worker-b")); e != nil {
		t.Fatal(e)
	}
	if _, _, e = Pod(context.Background(), c, "control", "project"); e == nil || !strings.Contains(e.Error(), "exactly one") {
		t.Fatal(e)
	}
}

func TestExecRequestsOnlySuppliedStdinStream(t *testing.T) {
	for _, supplied := range []bool{false, true} {
		t.Run(map[bool]string{false: "headless", true: "interactive"}[supplied], func(t *testing.T) {
			requests := make(chan *http.Request, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests <- r
				http.Error(w, "fixture rejects upgrade", http.StatusBadRequest)
			}))
			defer server.Close()
			var input io.Reader
			if supplied {
				input = strings.NewReader("fixture")
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := Exec(ctx, &rest.Config{Host: server.URL}, "control", "worker", []string{"true"}, input, io.Discard, io.Discard, false); err == nil {
				t.Fatal("fixture upgrade unexpectedly accepted")
			}
			select {
			case r := <-requests:
				if (r.URL.Query().Get("stdin") == "true") != supplied {
					t.Fatalf("stdin request mismatches supplied input: %v", r.URL.Query())
				}
				if r.URL.Query().Get("container") != "workspace" {
					t.Fatal("wrong worker container")
				}
			case <-ctx.Done():
				t.Fatal("exec request did not reach fixture")
			}
		})
	}
}
