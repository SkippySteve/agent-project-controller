package t3

import (
	"io"
	"net"
	"os"
	"time"
)

// Relay targets only the worker-local T3 socket. Kubernetes credentials and
// authentication remain on the operator machine; no worker ingress is created.
func Relay() error {
	if err := ensureWorker(); err != nil {
		return err
	}
	conn, err := net.DialTimeout("tcp4", "127.0.0.1:8788", 3*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	return relayStreams(conn, os.Stdin, os.Stdout)
}

func relayStreams(conn net.Conn, input io.Reader, output io.Writer) error {
	go func() {
		_, _ = io.Copy(conn, input)
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		} else {
			_ = conn.Close()
		}
	}()
	_, err := io.Copy(output, conn)
	return err
}
