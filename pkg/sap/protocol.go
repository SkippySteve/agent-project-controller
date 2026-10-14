// Package sap is a reference Sandbox Access Protocol (SAP) v1alpha1.
//
// SAP sits above a sandbox runtime such as sandboxd and below a product UI.
// A client lists sandboxes it may see, reads status, and attaches one
// bidirectional session. This package does not create sandboxes or decide
// which process runs inside an attach; the access plane does.
package sap

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"time"
)

const (
	APIVersion      = "sandboxaccess.agents.x-k8s.io/v1alpha1"
	Version         = "v1alpha1"
	Subprotocol     = "sap.v1"
	ChannelTerminal = "terminal"

	PhasePending     = "Pending"
	PhaseReady       = "Ready"
	PhasePaused      = "Paused"
	PhaseBlocked     = "Blocked"
	PhaseTerminating = "Terminating"

	ActivityIdle    = "idle"
	ActivityWorking = "working"
	ActivityWaiting = "waiting"
	ActivityBlocked = "blocked"

	// MaxData is the largest decoded input or output chunk, in bytes.
	MaxData  = 32 * 1024
	maxFrame = 64 * 1024
)

// Sandbox is one discoverable agent sandbox. ID is the DNS-1123 name carried
// by X-Sandbox-ID. Namespace and name together identify the attach target.
type Sandbox struct {
	APIVersion   string            `json:"apiVersion"`
	Kind         string            `json:"kind"`
	ID           string            `json:"id"`
	Namespace    string            `json:"namespace"`
	Name         string            `json:"name"`
	Labels       map[string]string `json:"labels,omitempty"`
	Phase        string            `json:"phase"`
	Activity     string            `json:"activity"`
	LastActivity *time.Time        `json:"lastActivity,omitempty"`
	Project      string            `json:"project,omitempty"`
	Owner        string            `json:"owner,omitempty"`
	Channels     []string          `json:"channels"`
}

// List is a discovery response.
type List struct {
	APIVersion string    `json:"apiVersion"`
	Kind       string    `json:"kind"`
	Items      []Sandbox `json:"items"`
}

// Frame is one JSON text message on an attach WebSocket.
// Data is standard base64 of the raw bytes, not a JSON string of text.
type Frame struct {
	Type         string `json:"type"`
	Data         string `json:"data,omitempty"`
	Stream       string `json:"stream,omitempty"`
	Cols         uint16 `json:"cols,omitempty"`
	Rows         uint16 `json:"rows,omitempty"`
	Code         *int   `json:"code,omitempty"`
	Activity     string `json:"activity,omitempty"`
	LastActivity string `json:"lastActivity,omitempty"`
	Message      string `json:"message,omitempty"`
	Channel      string `json:"channel,omitempty"`
}

// Session is the byte stream behind one attach. The access plane chooses the
// command. Clients cannot supply one.
type Session interface {
	Read([]byte) (int, error)
	Write([]byte) (int, error)
	Resize(cols, rows uint16) error
	Close() error
}

// ExitCoder optionally reports the process status after the stream ends.
type ExitCoder interface {
	ExitCode() (int, bool)
}

var (
	ErrNotFound    = errors.New("sandbox not found")
	ErrNotReady    = errors.New("sandbox is not ready")
	ErrBusy        = errors.New("sandbox session already attached")
	ErrChannel     = errors.New("channel is not available")
	ErrUnavailable = errors.New("sandbox session unavailable")
)

var labelPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// ValidLabel reports whether value is a DNS-1123 label.
func ValidLabel(value string) bool {
	return len(value) <= 63 && labelPattern.MatchString(value)
}

// IsAttach reports whether path is a SAP attach route with valid labels.
func IsAttach(path string) bool {
	rest, ok := cutPrefix(path, "/sap/v1/sandboxes/")
	if !ok {
		return false
	}
	namespace, rest, ok := cut(rest, "/")
	if !ok {
		return false
	}
	name, ok := cutSuffix(rest, "/attach")
	if !ok || len(name) == 0 || containsSlash(name) {
		return false
	}
	return ValidLabel(namespace) && ValidLabel(name)
}

func cutPrefix(value, prefix string) (string, bool) {
	if len(value) < len(prefix) || value[:len(prefix)] != prefix {
		return "", false
	}
	return value[len(prefix):], true
}

func cutSuffix(value, suffix string) (string, bool) {
	if len(value) < len(suffix) || value[len(value)-len(suffix):] != suffix {
		return "", false
	}
	return value[:len(value)-len(suffix)], true
}

func cut(value, sep string) (string, string, bool) {
	for i := 0; i+len(sep) <= len(value); i++ {
		if value[i:i+len(sep)] == sep {
			return value[:i], value[i+len(sep):], true
		}
	}
	return "", "", false
}

func containsSlash(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] == '/' {
			return true
		}
	}
	return false
}

// ParseClient validates one client attach frame.
func ParseClient(payload []byte) (Frame, error) {
	if len(payload) == 0 || len(payload) > maxFrame {
		return Frame{}, errors.New("invalid frame")
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	var frame Frame
	if err := dec.Decode(&frame); err != nil {
		return Frame{}, errors.New("invalid frame")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return Frame{}, errors.New("invalid frame")
	}
	switch frame.Type {
	case "input":
		if frame.Stream != "" || frame.Cols != 0 || frame.Rows != 0 || frame.Code != nil || frame.Activity != "" || frame.LastActivity != "" || frame.Message != "" || frame.Channel != "" {
			return Frame{}, errors.New("invalid frame")
		}
		if frame.Data == "" {
			return frame, nil
		}
		raw, err := base64.StdEncoding.DecodeString(frame.Data)
		if err != nil || len(raw) > MaxData {
			return Frame{}, errors.New("invalid frame")
		}
	case "resize":
		if frame.Data != "" || frame.Stream != "" || frame.Code != nil || frame.Activity != "" || frame.Message != "" || frame.Channel != "" || frame.Cols < 1 || frame.Cols > 512 || frame.Rows < 1 || frame.Rows > 512 {
			return Frame{}, errors.New("invalid frame")
		}
	case "close":
		if frame.Data != "" || frame.Stream != "" || frame.Cols != 0 || frame.Rows != 0 || frame.Code != nil || frame.Activity != "" || frame.Message != "" || frame.Channel != "" {
			return Frame{}, errors.New("invalid frame")
		}
	default:
		return Frame{}, errors.New("invalid frame")
	}
	return frame, nil
}
