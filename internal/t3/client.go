package t3

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"git.cluster.aimingupward.com/steve/agent-project-controller/internal/connect"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func Launch(ctx context.Context, c client.Client, config *rest.Config, control, workspace string) error {
	_, pod, err := connect.Pod(ctx, c, control, workspace)
	if err != nil {
		return err
	}
	instanceBytes := make([]byte, 16)
	if _, err := rand.Read(instanceBytes); err != nil {
		return err
	}
	instance := hex.EncodeToString(instanceBytes)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Worker output can contain bootstrap secrets. Never forward it to the console.
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- connect.Exec(ctx, config, pod.Namespace, pod.Name, []string{"/usr/local/bin/agent-project-controller", "t3-serve", instance}, nil, io.Discard, io.Discard, false)
	}()
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		_ = connect.Exec(stopCtx, config, pod.Namespace, pod.Name, []string{"/usr/local/bin/agent-project-controller", "t3-stop", instance}, nil, io.Discard, io.Discard, false)
	}()
	// Wait inside the worker. Dialing the forwarding listener before the remote
	// server is ready can close the entire forwarding session.
	for i := 0; ; i++ {
		select {
		case <-serverDone:
			return errors.New("T3 server exited; verify the workspace image")
		default:
		}
		probeCtx, probeCancel := context.WithTimeout(ctx, 3*time.Second)
		e := connect.Exec(probeCtx, config, pod.Namespace, pod.Name, []string{"/usr/bin/curl", "--noproxy", "*", "--fail", "--silent", "--max-time", "2", "--output", "/dev/null", "http://127.0.0.1:8788/"}, nil, io.Discard, io.Discard, false)
		probeCancel()
		if e == nil {
			break
		}
		if i == 59 {
			return errors.New("T3 server did not become ready")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-serverDone:
			return errors.New("T3 server exited; verify the workspace image")
		case <-time.After(time.Second):
		}
	}
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return err
	}
	defer listener.Close()
	forwardDone := make(chan error, 1)
	slots := make(chan struct{}, 16)
	go func() {
		for {
			conn, e := listener.AcceptTCP()
			if e != nil {
				forwardDone <- e
				return
			}
			select {
			case slots <- struct{}{}:
				go func() {
					defer func() { conn.Close(); <-slots }()
					_ = connect.Exec(ctx, config, pod.Namespace, pod.Name, []string{"/usr/local/bin/agent-project-controller", "t3-relay"}, conn, conn, io.Discard, false)
				}()
			default:
				conn.Close()
			}
		}
	}()
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	address := "127.0.0.1:" + port
	var pairing bytes.Buffer
	var pairErr error
	for i := 0; i < 30; i++ {
		select {
		case <-serverDone:
			return errors.New("T3 server failed to start; stop any existing launcher first")
		default:
		}
		pairing.Reset()
		pairErr = connect.Exec(ctx, config, pod.Namespace, pod.Name, []string{Binary, "auth", "pairing", "create", "--base-dir", Data, "--base-url", "http://" + address, "--ttl", "10m", "--label", "Project operator", "--json"}, nil, &pairing, io.Discard, false)
		if pairErr == nil {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if pairErr != nil {
		return errors.New("T3 pairing unavailable")
	}
	var link struct {
		PairURL string `json:"pairUrl"`
	}
	if json.Unmarshal(pairing.Bytes(), &link) != nil {
		return errors.New("invalid T3 pairing response")
	}
	u, err := url.Parse(link.PairURL)
	if err != nil || u.Scheme != "http" || u.Host != address || u.User != nil {
		return errors.New("invalid T3 pairing address")
	}
	directory, err := os.UserCacheDir()
	if err != nil {
		return err
	}
	directory = filepath.Join(directory, "agent-project-controller")
	if err = os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(directory, "t3-pairing-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err = file.Chmod(0600); err != nil {
		file.Close()
		return err
	}
	_, err = file.Write(pairing.Bytes())
	file.Close()
	if err != nil {
		return err
	}
	if os.Getenv("APC_T3_NO_BROWSER") != "true" && (os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != "") {
		if opener, e := exec.LookPath("xdg-open"); e == nil {
			page, e := os.CreateTemp(directory, "t3-open-*.html")
			if e == nil {
				defer os.Remove(page.Name())
				_ = page.Chmod(0600)
				_, e = fmt.Fprintf(page, "<!doctype html><meta http-equiv=refresh content=\"0;url=%s\">", html.EscapeString(link.PairURL))
				page.Close()
				if e == nil {
					cmd := exec.Command(opener, page.Name())
					cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
					if cmd.Start() == nil {
						go cmd.Wait()
					}
				}
			}
		}
	}
	fmt.Fprintf(os.Stdout, "T3: http://%s\nOne-time pairing link saved in %s\nKeep this command running. Ctrl+C stops the server and tunnel.\n", address, file.Name())
	select {
	case <-ctx.Done():
		return nil
	case <-serverDone:
		return errors.New("T3 server stopped")
	case <-forwardDone:
		return errors.New("T3 tunnel stopped")
	}
}
