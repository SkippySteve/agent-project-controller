package sap

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	api "git.cluster.aimingupward.com/steve/agent-project-controller/api/v1alpha1"
	apisap "git.cluster.aimingupward.com/steve/agent-project-controller/pkg/sap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestWorkspaceAttachUsesFixedShell(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	workspace := &api.ProjectWorkspace{ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "control", UID: types.UID("uid-1")}, Spec: api.WorkspaceSpec{Repository: "steve/demo", DesiredState: "Running"}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "proj-demo", Labels: map[string]string{api.OwnerLabel: "uid-1", "projects.agentwork.dev/role": "worker"}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(workspace).WithObjects(workspace, pod).Build()
	workspace.Status.Phase = "Ready"
	workspace.Status.Namespace = "proj-demo"
	if err := c.Status().Update(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	var command []string
	var container string
	exec := &PodExec{Client: c, ControlNamespace: "control", Stream: func(ctx context.Context, namespace, podName, gotContainer string, gotCommand []string, stdin io.Reader, stdout io.Writer, sizes remotecommand.TerminalSizeQueue) error {
		if namespace != "proj-demo" || podName != "worker" {
			return errors.New("unexpected pod")
		}
		command = append([]string{}, gotCommand...)
		container = gotContainer
		if sizes.Next() == nil {
			return errors.New("missing terminal size")
		}
		_, err := io.Copy(stdout, stdin)
		return err
	}}
	plane := &apisap.Plane{Catalog: &Workspaces{Client: c, Namespace: "control"}, Attach: exec.Attach}
	defer plane.Close()
	server := httptest.NewServer(plane.Handler())
	defer server.Close()
	sapClient := &apisap.Client{BaseURL: server.URL}
	list, err := sapClient.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0].Namespace != "proj-demo" || list.Items[0].Project != "steve/demo" || list.Items[0].Owner != "steve" || list.Items[0].Phase != apisap.PhaseReady {
		t.Fatalf("list: %+v", list.Items)
	}
	if _, err = sapClient.Get(t.Context(), "control", "demo"); err == nil {
		t.Fatal("control namespace was accepted for a sandbox that lives in proj-demo")
	}
	preparing := Convert(&api.ProjectWorkspace{ObjectMeta: metav1.ObjectMeta{Name: "new", Namespace: "control"}, Spec: api.WorkspaceSpec{Repository: "steve/new"}})
	if preparing.Phase != apisap.PhasePending || preparing.Namespace != "control" {
		t.Fatalf("new workspace: %+v", preparing)
	}
	stream, err := sapClient.Attach(t.Context(), "proj-demo", "demo", apisap.ChannelTerminal)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if err = stream.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = stream.Read(); err != nil {
		t.Fatal(err)
	}
	if err = stream.Input([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	frame, err := stream.Read()
	if err != nil || frame.Type != "output" || frame.Data == "" {
		t.Fatalf("frame %+v %v", frame, err)
	}
	if container != "workspace" || len(command) != 1 || command[0] != "/bin/sh" {
		t.Fatalf("exec %s %v", container, command)
	}
}

func TestServeRejectsRemoteListenAndShortToken(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/token"
	if err := os.WriteFile(path, []byte(strings.Repeat("d", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SAP_TOKEN_FILE", path)
	t.Setenv("SAP_LISTEN", "0.0.0.0:8090")
	if err := Serve(context.Background(), Options{}); err == nil {
		t.Fatal("accepted a non-loopback listen address")
	}
	if err := os.WriteFile(path, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SAP_LISTEN", "127.0.0.1:0")
	if err := Serve(context.Background(), Options{}); err == nil {
		t.Fatal("accepted a short token")
	}
}

func TestLocalPrototypeRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("cat"); err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	path := dir + "/token"
	token := strings.Repeat("e", 32)
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SAP_TOKEN_FILE", path)
	t.Setenv("SAP_LISTEN", "127.0.0.1:0")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ready := make(chan string, 1)
	errc := make(chan error, 1)
	go func() {
		errc <- Serve(ctx, Options{Command: []string{"cat"}, Ready: func(addr string) { ready <- addr }})
	}()
	var addr string
	select {
	case addr = <-ready:
	case err := <-errc:
		t.Fatal(err)
	case <-time.After(2 * time.Second):
		t.Fatal("prototype did not listen")
	}
	client := &apisap.Client{BaseURL: "http://" + addr, Token: token}
	stream, err := client.Attach(t.Context(), "local", "local", apisap.ChannelTerminal)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if err = stream.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err = stream.Input([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	var got strings.Builder
	for !strings.Contains(got.String(), "hi") {
		frame, err := stream.Read()
		if err != nil {
			t.Fatal(err)
		}
		if frame.Type != "output" {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(frame.Data)
		if err != nil {
			t.Fatal(err)
		}
		got.Write(raw)
	}
}
