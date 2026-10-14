package sap

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

func (p *Plane) serve(ctx context.Context, conn *websocket.Conn, session Session, namespace, name string) {
	clearDeadlines(conn)
	conn.SetReadLimit(maxFrame)
	var closeOnce sync.Once
	closeConn := func() { closeOnce.Do(func() { _ = conn.Close() }) }
	var writeMu sync.Mutex
	write := func(frame Frame) error {
		body, err := json.Marshal(frame)
		if err != nil {
			return err
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		err = conn.WriteMessage(websocket.TextMessage, body)
		_ = conn.SetWriteDeadline(time.Time{})
		return err
	}
	key := namespace + "/" + name
	p.book().touch(key)
	view := p.decorate(Sandbox{Namespace: namespace, Name: name, Phase: PhaseReady, Channels: []string{ChannelTerminal}})
	if err := write(Frame{Type: "status", Activity: view.Activity, Channel: ChannelTerminal, LastActivity: formatTime(view.LastActivity)}); err != nil {
		closeConn()
		_ = session.Close()
		return
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 12*1024)
		for {
			n, err := session.Read(buf)
			if n > 0 {
				if writeErr := write(Frame{Type: "output", Stream: "stdout", Data: base64.StdEncoding.EncodeToString(buf[:n])}); writeErr != nil {
					closeConn()
					return
				}
			}
			if err != nil {
				code := sessionCode(session, err)
				if code != 0 {
					_ = write(Frame{Type: "error", Message: "session ended"})
				}
				_ = write(Frame{Type: "exit", Code: &code})
				closeConn()
				return
			}
		}
	}()
	go func() {
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				closeConn()
				return
			case <-ticker.C:
				writeMu.Lock()
				_ = conn.WriteControl(websocket.PingMessage, []byte("sap"), time.Now().Add(5*time.Second))
				writeMu.Unlock()
			}
		}
	}()
	defer func() {
		close(done)
		_ = session.Close()
		closeConn()
		wg.Wait()
		p.book().idle(key)
	}()
	for {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			return
		}
		frame, err := ParseClient(payload)
		if err != nil {
			_ = write(Frame{Type: "error", Message: "invalid frame"})
			return
		}
		switch frame.Type {
		case "input":
			if frame.Data == "" {
				continue
			}
			raw, err := base64.StdEncoding.DecodeString(frame.Data)
			if err != nil {
				_ = write(Frame{Type: "error", Message: "invalid frame"})
				return
			}
			if _, err = session.Write(raw); err != nil {
				return
			}
			p.book().input(key)
		case "resize":
			if err = session.Resize(frame.Cols, frame.Rows); err != nil {
				return
			}
		case "close":
			return
		}
	}
}

func clearDeadlines(conn *websocket.Conn) {
	if raw := conn.UnderlyingConn(); raw != nil {
		_ = raw.SetDeadline(time.Time{})
	}
}

func sessionCode(session Session, err error) int {
	if coder, ok := session.(ExitCoder); ok {
		if code, known := coder.ExitCode(); known {
			return code
		}
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) {
		return 0
	}
	return 1
}

func formatTime(value *time.Time) string {
	if value == nil || value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}
