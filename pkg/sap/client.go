package sap

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// StatusError is a non-2xx response from an access plane.
type StatusError struct {
	Code    int
	Message string
}

func (e *StatusError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return http.StatusText(e.Code)
}

// Client calls a SAP access plane.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// List returns the sandboxes this token may see.
func (c *Client) List(ctx context.Context) (List, error) {
	var list List
	err := c.get(ctx, "/sap/v1/sandboxes", &list)
	return list, err
}

// Get returns one sandbox.
func (c *Client) Get(ctx context.Context, namespace, name string) (Sandbox, error) {
	var item Sandbox
	err := c.get(ctx, "/sap/v1/sandboxes/"+url.PathEscape(namespace)+"/"+url.PathEscape(name), &item)
	return item, err
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	endpoint, err := c.url(path)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body := io.LimitReader(resp.Body, 1<<20)
	if resp.StatusCode != http.StatusOK {
		return statusFrom(resp.StatusCode, body)
	}
	if err := json.NewDecoder(body).Decode(out); err != nil {
		return errors.New("invalid sandbox response")
	}
	return nil
}

// Attach opens a terminal or other advertised channel.
func (c *Client) Attach(ctx context.Context, namespace, name, channel string) (*Stream, error) {
	if channel == "" {
		channel = ChannelTerminal
	}
	endpoint, err := c.url("/sap/v1/sandboxes/" + url.PathEscape(namespace) + "/" + url.PathEscape(name) + "/attach")
	if err != nil {
		return nil, err
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	switch parsed.Scheme {
	case "https":
		parsed.Scheme = "wss"
	case "http":
		parsed.Scheme = "ws"
	default:
		return nil, errors.New("unsupported access plane url")
	}
	if channel != ChannelTerminal {
		query := parsed.Query()
		query.Set("channel", channel)
		parsed.RawQuery = query.Encode()
	}
	header := http.Header{}
	if c.Token != "" {
		header.Set("Authorization", "Bearer "+c.Token)
	}
	dialer := websocket.Dialer{Proxy: http.ProxyFromEnvironment, HandshakeTimeout: 10 * time.Second, Subprotocols: []string{Subprotocol}}
	conn, resp, err := dialer.DialContext(ctx, parsed.String(), header)
	if err != nil {
		if resp != nil {
			defer resp.Body.Close()
			return nil, statusFrom(resp.StatusCode, io.LimitReader(resp.Body, 4096))
		}
		return nil, err
	}
	if proto := conn.Subprotocol(); proto != "" && proto != Subprotocol {
		_ = conn.Close()
		return nil, errors.New("unexpected subprotocol")
	}
	return &Stream{conn: conn}, nil
}

func (c *Client) url(path string) (string, error) {
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		return "", errors.New("access plane url required")
	}
	return base + path, nil
}

func statusFrom(code int, body io.Reader) error {
	var payload struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(body).Decode(&payload)
	if payload.Error == "" {
		payload.Error = http.StatusText(code)
	}
	return &StatusError{Code: code, Message: payload.Error}
}

// Stream is one attached SAP session.
type Stream struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

// Read returns the next server frame.
func (s *Stream) Read() (Frame, error) {
	_, payload, err := s.conn.ReadMessage()
	if err != nil {
		return Frame{}, err
	}
	var frame Frame
	if err := json.Unmarshal(payload, &frame); err != nil {
		return Frame{}, errors.New("invalid frame")
	}
	return frame, nil
}

// Input sends raw terminal bytes.
func (s *Stream) Input(data []byte) error {
	if len(data) > MaxData {
		return errors.New("input exceeds frame limit")
	}
	return s.send(Frame{Type: "input", Data: base64.StdEncoding.EncodeToString(data)})
}

// Resize sets the terminal window.
func (s *Stream) Resize(cols, rows uint16) error {
	return s.send(Frame{Type: "resize", Cols: cols, Rows: rows})
}

// Close sends a close frame and closes the socket.
func (s *Stream) Close() error {
	_ = s.send(Frame{Type: "close"})
	return s.conn.Close()
}

// SetReadDeadline bounds the next read. A zero time clears it.
func (s *Stream) SetReadDeadline(deadline time.Time) error {
	return s.conn.SetReadDeadline(deadline)
}

func (s *Stream) send(frame Frame) error {
	body, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn.WriteMessage(websocket.TextMessage, body)
}
