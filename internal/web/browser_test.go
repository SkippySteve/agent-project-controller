package web

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"

	api "git.cluster.aimingupward.com/steve/agent-project-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
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
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	password := strings.Repeat("test", 8)
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	origin := "http://" + listener.Addr().String()
	s := &Server{Client: c, Host: host{}, Namespace: "control", Owner: "steve", Password: password, Origin: origin, AllowBasic: true}
	handler, e := s.Handler()
	if e != nil {
		t.Fatal(e)
	}
	server := &http.Server{Handler: handler}
	go server.Serve(listener)
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
	t.Log(string(out))
}
