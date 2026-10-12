package t3

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"
)

func TestRelayPreservesBinaryAndPropagatesInputEOF(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	fixture := bytes.Repeat([]byte{0, 255, 13, 10, 42}, 16384)
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		input, err := io.ReadAll(conn)
		if err == nil && !bytes.Equal(input, fixture) {
			err = io.ErrUnexpectedEOF
		}
		if err == nil {
			_, err = conn.Write(input)
		}
		done <- err
	}()
	conn, err := net.Dial("tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	var output bytes.Buffer
	if err := relayStreams(conn, bytes.NewReader(fixture), &output); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output.Bytes(), fixture) {
		t.Fatal("binary relay changed output")
	}
}
