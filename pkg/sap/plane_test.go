package sap

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDiscoveryAttachAndActivity(t *testing.T) {
	memory := &Memory{}
	if err := memory.Put(Sandbox{Namespace: "local", Name: "local", Phase: PhaseReady, Project: "local/demo", Owner: "operator", Channels: []string{ChannelTerminal}}); err != nil {
		t.Fatal(err)
	}
	if err := memory.Put(Sandbox{Namespace: "local", Name: "paused", Phase: PhasePaused}); err != nil {
		t.Fatal(err)
	}
	plane := &Plane{Catalog: memory, Quiet: 400 * time.Millisecond, Attach: func(context.Context, Sandbox, string) (Session, error) {
		return newEcho(), nil
	}}
	defer plane.Close()
	token := strings.Repeat("a", 32)
	server := httptest.NewServer(Bearer(token, plane.Handler()))
	defer server.Close()
	client := &Client{BaseURL: server.URL, Token: token}
	ctx := t.Context()
	denied := &Client{BaseURL: server.URL, Token: "wrong-token-wrong-token-wrong-token"}
	if _, err := denied.List(ctx); err == nil {
		t.Fatal("rejected token was accepted")
	} else {
		var status *StatusError
		if !errors.As(err, &status) || status.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated list: %v", err)
		}
	}

	list, err := client.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if list.APIVersion != APIVersion || list.Kind != "SandboxList" || len(list.Items) != 2 || list.Items[0].Name != "local" || list.Items[0].ID != "local" || list.Items[0].Activity != ActivityIdle {
		t.Fatalf("list: %+v", list)
	}
	if _, err = client.Get(ctx, "local", "missing"); err == nil || !errors.As(err, new(*StatusError)) {
		t.Fatal(err)
	} else {
		var status *StatusError
		if !errors.As(err, &status) || status.Code != http.StatusNotFound {
			t.Fatal(status)
		}
	}

	stream, err := client.Attach(ctx, "local", "local", "")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if err = stream.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	statusFrame, err := stream.Read()
	if err != nil || statusFrame.Type != "status" || statusFrame.Channel != ChannelTerminal {
		t.Fatalf("status frame: %+v %v", statusFrame, err)
	}
	busy, err := client.Attach(ctx, "local", "local", ChannelTerminal)
	var busyStatus *StatusError
	if !errors.As(err, &busyStatus) || busyStatus.Code != http.StatusConflict {
		if busy != nil {
			busy.Close()
		}
		t.Fatalf("second attach: %v", err)
	}
	if _, err = client.Attach(ctx, "local", "paused", ChannelTerminal); err == nil {
		t.Fatal("paused sandbox accepted attach")
	} else if !errors.As(err, &busyStatus) || busyStatus.Code != http.StatusConflict {
		t.Fatal(err)
	}
	if err = stream.Resize(80, 24); err != nil {
		t.Fatal(err)
	}
	if err = stream.Input([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool {
		item, err := client.Get(ctx, "local", "local")
		return err == nil && item.Activity == ActivityWorking && item.LastActivity != nil
	})
	output := readOutput(t, stream, "hi")
	if output != "hi" {
		t.Fatalf("output %q", output)
	}
	waitUntil(t, func() bool {
		item, err := client.Get(ctx, "local", "local")
		return err == nil && item.Activity == ActivityIdle
	})
}

func TestAttachRejectsCommandAndSurvivesServerDeadline(t *testing.T) {
	memory := &Memory{}
	if err := memory.Put(Sandbox{Namespace: "local", Name: "local", Phase: PhaseReady, Channels: []string{ChannelTerminal}}); err != nil {
		t.Fatal(err)
	}
	called := false
	plane := &Plane{Catalog: memory, Attach: func(context.Context, Sandbox, string) (Session, error) {
		called = true
		return newEcho(), nil
	}}
	defer plane.Close()
	req := httptest.NewRequest(http.MethodGet, "/sap/v1/sandboxes/local/local/attach?command=id", nil)
	rec := httptest.NewRecorder()
	plane.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || called {
		t.Fatalf("command query: %d called=%v body=%s", rec.Code, called, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/sap/v1/sandboxes/local/local/attach?channel=control", nil)
	rec = httptest.NewRecorder()
	plane.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("control channel: %d %s", rec.Code, rec.Body.String())
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: plane.Handler(), ReadHeaderTimeout: 50 * time.Millisecond, ReadTimeout: 150 * time.Millisecond, WriteTimeout: 150 * time.Millisecond}
	go server.Serve(listener)
	defer server.Close()
	client := &Client{BaseURL: "http://" + listener.Addr().String()}
	stream, err := client.Attach(t.Context(), "local", "local", ChannelTerminal)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	time.Sleep(400 * time.Millisecond)
	if err = stream.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = stream.Read(); err != nil {
		t.Fatal(err)
	}
	if err = stream.Input([]byte("ok")); err != nil {
		t.Fatal(err)
	}
	if got := readOutput(t, stream, "ok"); got != "ok" {
		t.Fatalf("output after deadline: %q", got)
	}
}

func TestParseClientAndAttachPath(t *testing.T) {
	if !IsAttach("/sap/v1/sandboxes/local/local/attach") || IsAttach("/sap/v1/sandboxes/Local/local/attach") || IsAttach("/sap/v1/sandboxes/local/local") {
		t.Fatal("attach path classification")
	}
	frame, err := ParseClient([]byte(`{"type":"input","data":"aGk="}`))
	if err != nil || frame.Type != "input" {
		t.Fatal(err)
	}
	if _, err = ParseClient([]byte(`{"type":"input","data":"aGk=","command":"id"}`)); err == nil {
		t.Fatal("accepted an unknown field")
	}
	if _, err = ParseClient([]byte(`{"type":"resize","cols":0,"rows":24}`)); err == nil {
		t.Fatal("accepted a zero column count")
	}
	raw, err := base64.StdEncoding.DecodeString("aGk=")
	if err != nil || string(raw) != "hi" {
		t.Fatal(raw, err)
	}
	var list List
	if err := json.Unmarshal([]byte(`{"apiVersion":"sandboxaccess.agents.x-k8s.io/v1alpha1","kind":"SandboxList","items":[]}`), &list); err != nil || list.Kind != "SandboxList" {
		t.Fatal(err)
	}
}

type pipeSession struct {
	reader *io.PipeReader
	writer *io.PipeWriter
	once   sync.Once
}

func newEcho() *pipeSession {
	reader, writer := io.Pipe()
	return &pipeSession{reader: reader, writer: writer}
}
func (s *pipeSession) Read(p []byte) (int, error)  { return s.reader.Read(p) }
func (s *pipeSession) Write(p []byte) (int, error) { return s.writer.Write(p) }
func (s *pipeSession) Resize(uint16, uint16) error { return nil }
func (s *pipeSession) Close() error {
	s.once.Do(func() {
		_ = s.writer.Close()
		_ = s.reader.Close()
	})
	return nil
}

func readOutput(t *testing.T, stream *Stream, want string) string {
	t.Helper()
	var got strings.Builder
	for got.Len() < len(want) {
		frame, err := stream.Read()
		if err != nil {
			t.Fatal(err)
		}
		if frame.Type == "exit" || frame.Type == "error" {
			t.Fatalf("frame %+v before %q, have %q", frame, want, got.String())
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
	return got.String()
}

func waitUntil(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met")
}
