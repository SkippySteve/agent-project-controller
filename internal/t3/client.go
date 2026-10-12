package t3

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"git.cluster.aimingupward.com/steve/agent-project-controller/internal/connect"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
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
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return err
	}
	endpoint := clientset.CoreV1().RESTClient().Post().Resource("pods").Namespace(pod.Namespace).Name(pod.Name).SubResource("portforward").URL()
	transport, upgrader, err := spdy.RoundTripperFor(config)
	if err != nil {
		return err
	}
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, "POST", endpoint)
	stop := make(chan struct{})
	defer close(stop)
	ready := make(chan struct{})
	forward, err := portforward.NewOnAddresses(dialer, []string{"127.0.0.1"}, []string{"0:8788"}, stop, ready, io.Discard, io.Discard)
	if err != nil {
		return err
	}
	forwardDone := make(chan error, 1)
	go func() { forwardDone <- forward.ForwardPorts() }()
	select {
	case <-ready:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(30 * time.Second):
		return errors.New("T3 local tunnel did not start")
	case <-forwardDone:
		return errors.New("T3 local tunnel failed")
	}
	ports, err := forward.GetPorts()
	if err != nil || len(ports) != 1 {
		return errors.New("T3 local port unavailable")
	}
	port := strconv.Itoa(int(ports[0].Local))
	address := "127.0.0.1:" + port
	for i := 0; i < 60; i++ {
		conn, e := net.DialTimeout("tcp", address, time.Second)
		if e == nil {
			conn.Close()
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-serverDone:
			return errors.New("T3 server exited; verify the workspace image")
		case <-time.After(time.Second):
		}
		if i == 59 {
			return errors.New("T3 server did not become ready")
		}
	}
	// A port-forward listener accepts connections before the remote server is ready.
	// Pairing creation retries until T3's database has initialized.
	var pairing bytes.Buffer
	var pairErr error
	for i := 0; i < 30; i++ {
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
	if err = file.Chmod(0600); err != nil {
		file.Close()
		return err
	}
	_, err = file.Write(pairing.Bytes())
	file.Close()
	if err != nil {
		return err
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
