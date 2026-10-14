package sap

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	apisap "git.cluster.aimingupward.com/steve/agent-project-controller/pkg/sap"
	"golang.org/x/term"
)

// Options configures the local SAP prototype.
type Options struct {
	Addr    string
	Token   string
	Command []string
	Ready   func(addr string)
}

// CLI serves the local prototype or calls a running access plane.
func CLI(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "serve" {
		if len(args) > 1 {
			return errors.New("usage: agent-project-controller sap serve")
		}
		return Serve(ctx, Options{})
	}
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return errors.New("usage: agent-project-controller sap list")
		}
		return List(ctx)
	case "attach":
		if len(args) != 3 {
			return errors.New("usage: agent-project-controller sap attach <namespace> <name>")
		}
		return Attach(ctx, args[1], args[2])
	default:
		return errors.New("usage: agent-project-controller sap serve|list|attach")
	}
}

// Serve listens on loopback and attaches a local PTY for the sandbox named local/local.
func Serve(ctx context.Context, opts Options) error {
	token := opts.Token
	if token == "" {
		var err error
		token, err = readToken()
		if err != nil {
			return err
		}
	}
	if len(token) < 32 {
		return errors.New("SAP token must be at least 32 characters")
	}
	addr := opts.Addr
	if addr == "" {
		addr = os.Getenv("SAP_LISTEN")
	}
	if addr == "" {
		addr = "127.0.0.1:8090"
	}
	if !loopback(addr) {
		return errors.New("sap serve listens on loopback only")
	}
	command := opts.Command
	if len(command) == 0 {
		command = []string{"/bin/sh"}
	}
	memory := &apisap.Memory{}
	if err := memory.Put(apisap.Sandbox{Namespace: "local", Name: "local", Phase: apisap.PhaseReady, Project: "local/demo", Owner: "operator", Channels: []string{apisap.ChannelTerminal}}); err != nil {
		return err
	}
	plane := &apisap.Plane{Catalog: memory, Attach: func(ctx context.Context, sandbox apisap.Sandbox, channel string) (apisap.Session, error) {
		if channel != apisap.ChannelTerminal || sandbox.Name != "local" {
			return nil, apisap.ErrChannel
		}
		return StartPTY(ctx, command)
	}}
	server := &http.Server{
		Handler:           apisap.Bearer(token, plane.Handler()),
		ReadHeaderTimeout: 10 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	errc := make(chan error, 1)
	go func() { errc <- server.Serve(listener) }()
	actual := listener.Addr().String()
	if opts.Ready != nil {
		opts.Ready(actual)
	} else {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
			"listen": actual,
			"mode":   "local-pty",
			"sandboxes": []map[string]string{{
				"namespace": "local",
				"name":      "local",
			}},
		})
	}
	select {
	case <-ctx.Done():
		plane.Close()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		err = <-errc
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case err = <-errc:
		plane.Close()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// List prints the sandboxes visible to SAP_TOKEN_FILE at SAP_URL.
func List(ctx context.Context) error {
	client, err := clientFromEnv()
	if err != nil {
		return err
	}
	list, err := client.List(ctx)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(list)
}

// Attach connects the local terminal to one sandbox.
func Attach(ctx context.Context, namespace, name string) error {
	client, err := clientFromEnv()
	if err != nil {
		return err
	}
	stream, err := client.Attach(ctx, namespace, name, apisap.ChannelTerminal)
	if err != nil {
		return err
	}
	defer stream.Close()
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		width, height, sizeErr := term.GetSize(fd)
		state, rawErr := term.MakeRaw(fd)
		if rawErr == nil {
			defer term.Restore(fd, state)
		}
		if sizeErr == nil && width > 0 && height > 0 && width <= 512 && height <= 512 {
			_ = stream.Resize(uint16(width), uint16(height))
		}
	}
	errc := make(chan error, 2)
	go func() {
		buf := make([]byte, 1024)
		for {
			n, readErr := os.Stdin.Read(buf)
			if n > 0 {
				if writeErr := stream.Input(buf[:n]); writeErr != nil {
					errc <- writeErr
					return
				}
			}
			if readErr != nil {
				if errors.Is(readErr, io.EOF) {
					errc <- nil
					return
				}
				errc <- readErr
				return
			}
		}
	}()
	go func() {
		for {
			frame, readErr := stream.Read()
			if readErr != nil {
				errc <- readErr
				return
			}
			switch frame.Type {
			case "output":
				raw, decErr := base64.StdEncoding.DecodeString(frame.Data)
				if decErr != nil {
					errc <- decErr
					return
				}
				if _, writeErr := os.Stdout.Write(raw); writeErr != nil {
					errc <- writeErr
					return
				}
			case "error":
				fmt.Fprintf(os.Stderr, "sap: %s\n", frame.Message)
			case "exit":
				if frame.Code != nil && *frame.Code != 0 {
					errc <- fmt.Errorf("session exit %d", *frame.Code)
					return
				}
				errc <- nil
				return
			}
		}
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err = <-errc:
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
}

func clientFromEnv() (*apisap.Client, error) {
	token, err := readToken()
	if err != nil {
		return nil, err
	}
	base := os.Getenv("SAP_URL")
	if base == "" {
		base = "http://127.0.0.1:8090"
	}
	return &apisap.Client{BaseURL: base, Token: token, HTTP: &http.Client{Timeout: 15 * time.Second}}, nil
}

func readToken() (string, error) {
	path := os.Getenv("SAP_TOKEN_FILE")
	if path == "" {
		return "", errors.New("SAP_TOKEN_FILE is required")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return "", errors.New("unable to read SAP_TOKEN_FILE")
	}
	token := strings.TrimSpace(string(body))
	if len(token) < 32 || strings.ContainsAny(token, " \r\n\t") {
		return "", errors.New("SAP token must be at least 32 characters")
	}
	return token, nil
}

func loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
