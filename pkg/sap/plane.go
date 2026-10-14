package sap

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Catalog lists sandboxes the caller is allowed to see. Implementations must
// already have applied authorization; the plane does not widen it.
type Catalog interface {
	List(context.Context) ([]Sandbox, error)
	Get(context.Context, string, string) (Sandbox, error)
}

// AttachFunc opens the stream for one advertised channel.
type AttachFunc func(context.Context, Sandbox, string) (Session, error)

// Plane is the reference discovery and attach HTTP surface.
type Plane struct {
	Catalog Catalog
	Attach  AttachFunc
	// Quiet is how long after the latest input an attach returns to idle.
	// Zero uses 15 seconds. Set it before the first request.
	Quiet time.Duration

	mu       sync.Mutex
	live     map[string]struct{}
	conns    map[*websocket.Conn]struct{}
	activity *activityBook
}

// Register adds the SAP routes to mux.
func (p *Plane) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /sap/v1/sandboxes", p.list)
	mux.HandleFunc("GET /sap/v1/sandboxes/{namespace}/{name}", p.get)
	mux.HandleFunc("GET /sap/v1/sandboxes/{namespace}/{name}/attach", p.attach)
}

// Handler serves SAP on its own mux. Callers that already authenticate can
// mount Register on their mux instead.
func (p *Plane) Handler() http.Handler {
	mux := http.NewServeMux()
	p.Register(mux)
	return mux
}

// Close ends active attach streams.
func (p *Plane) Close() {
	p.mu.Lock()
	conns := make([]*websocket.Conn, 0, len(p.conns))
	for conn := range p.conns {
		conns = append(conns, conn)
	}
	book := p.activity
	p.mu.Unlock()
	if book != nil {
		book.stop()
	}
	for _, conn := range conns {
		_ = conn.Close()
	}
}

func (p *Plane) list(w http.ResponseWriter, r *http.Request) {
	if p.Catalog == nil {
		writeErr(w, http.StatusServiceUnavailable, "sandbox catalog unavailable")
		return
	}
	items, err := p.Catalog.List(r.Context())
	if err != nil {
		slog.Warn("sandbox catalog unavailable")
		writeErr(w, http.StatusServiceUnavailable, "sandbox catalog unavailable")
		return
	}
	out := make([]Sandbox, 0, len(items))
	for _, item := range items {
		if !ValidLabel(item.Namespace) || !ValidLabel(item.Name) {
			continue
		}
		out = append(out, p.decorate(item))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	writeJSON(w, List{APIVersion: APIVersion, Kind: "SandboxList", Items: out})
}

func (p *Plane) get(w http.ResponseWriter, r *http.Request) {
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	if !ValidLabel(namespace) || !ValidLabel(name) {
		writeErr(w, http.StatusBadRequest, "invalid sandbox name")
		return
	}
	item, err := p.lookup(r.Context(), namespace, name)
	if err != nil {
		status, message := statusFor(err)
		writeErr(w, status, message)
		return
	}
	writeJSON(w, p.decorate(item))
}

func (p *Plane) attach(w http.ResponseWriter, r *http.Request) {
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	if !ValidLabel(namespace) || !ValidLabel(name) {
		writeErr(w, http.StatusBadRequest, "invalid sandbox name")
		return
	}
	if unexpectedQuery(r.URL.Query()) {
		writeErr(w, http.StatusBadRequest, "invalid attach request")
		return
	}
	channel := r.URL.Query().Get("channel")
	if channel == "" {
		channel = ChannelTerminal
	}
	item, err := p.lookup(r.Context(), namespace, name)
	if err != nil {
		status, message := statusFor(err)
		writeErr(w, status, message)
		return
	}
	item = p.decorate(item)
	if !hasChannel(item.Channels, channel) {
		writeErr(w, http.StatusBadRequest, "channel is not available")
		return
	}
	if item.Phase != PhaseReady {
		writeErr(w, http.StatusConflict, "sandbox is not ready")
		return
	}
	if p.Attach == nil {
		writeErr(w, http.StatusServiceUnavailable, "sandbox session unavailable")
		return
	}
	key := namespace + "/" + name + "/" + channel
	if !p.acquire(key) {
		writeErr(w, http.StatusConflict, "sandbox session already attached")
		return
	}
	defer p.release(key)
	session, err := p.Attach(r.Context(), item, channel)
	if err != nil {
		status, message := statusFor(err)
		if status == http.StatusServiceUnavailable {
			slog.Warn("sandbox attach unavailable", "namespace", namespace, "sandbox", name, "reason", safeErr(err))
		}
		writeErr(w, status, message)
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		_ = session.Close()
		return
	}
	p.track(conn)
	defer p.untrack(conn)
	p.serve(r.Context(), conn, session, namespace, name)
}

func (p *Plane) lookup(ctx context.Context, namespace, name string) (Sandbox, error) {
	if p.Catalog == nil {
		return Sandbox{}, ErrUnavailable
	}
	item, err := p.Catalog.Get(ctx, namespace, name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Sandbox{}, ErrNotFound
		}
		slog.Warn("sandbox lookup unavailable", "namespace", namespace, "sandbox", name)
		return Sandbox{}, ErrUnavailable
	}
	if item.Namespace != namespace || item.Name != name || !ValidLabel(item.Namespace) || !ValidLabel(item.Name) {
		return Sandbox{}, ErrNotFound
	}
	return item, nil
}

func (p *Plane) decorate(item Sandbox) Sandbox {
	item.APIVersion = APIVersion
	item.Kind = "Sandbox"
	item.ID = item.Name
	if item.Phase == "" {
		item.Phase = PhasePending
	}
	if len(item.Channels) == 0 {
		item.Channels = []string{ChannelTerminal}
	}
	switch item.Activity {
	case ActivityIdle, ActivityWorking, ActivityWaiting, ActivityBlocked:
	default:
		item.Activity = ActivityIdle
	}
	if item.Phase == PhaseBlocked {
		item.Activity = ActivityBlocked
		return item
	}
	if activity, at, ok := p.book().current(item.Namespace + "/" + item.Name); ok {
		item.Activity = activity
		if !at.IsZero() {
			item.LastActivity = &at
		}
	}
	return item
}

func (p *Plane) acquire(key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.live == nil {
		p.live = map[string]struct{}{}
	}
	if _, ok := p.live[key]; ok {
		return false
	}
	p.live[key] = struct{}{}
	return true
}

func (p *Plane) release(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.live, key)
}

func (p *Plane) track(conn *websocket.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conns == nil {
		p.conns = map[*websocket.Conn]struct{}{}
	}
	p.conns[conn] = struct{}{}
}

func (p *Plane) untrack(conn *websocket.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.conns, conn)
}

func (p *Plane) book() *activityBook {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.activity == nil {
		quiet := p.Quiet
		if quiet <= 0 {
			quiet = 15 * time.Second
		}
		p.activity = &activityBook{quiet: quiet, items: map[string]*activity{}}
	}
	return p.activity
}

func unexpectedQuery(values map[string][]string) bool {
	if len(values) == 0 {
		return false
	}
	if len(values) == 1 && len(values["channel"]) == 1 && values["channel"][0] != "" {
		return false
	}
	return true
}

func hasChannel(channels []string, channel string) bool {
	for _, item := range channels {
		if item == channel {
			return true
		}
	}
	return false
}

func statusFor(err error) (int, string) {
	switch {
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound, "sandbox not found"
	case errors.Is(err, ErrNotReady):
		return http.StatusConflict, "sandbox is not ready"
	case errors.Is(err, ErrBusy):
		return http.StatusConflict, "sandbox session already attached"
	case errors.Is(err, ErrChannel):
		return http.StatusBadRequest, "channel is not available"
	default:
		return http.StatusServiceUnavailable, "sandbox session unavailable"
	}
}

func safeErr(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	lower := strings.ToLower(message)
	for _, word := range []string{"token", "password", "secret", "authorization", "bearer"} {
		if strings.Contains(lower, word) {
			return "redacted"
		}
	}
	if len(message) > 180 {
		message = message[:180]
	}
	return message
}

func writeErr(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-SAP-Version", Version)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-SAP-Version", Version)
	_ = json.NewEncoder(w).Encode(value)
}

var upgrader = websocket.Upgrader{
	Subprotocols: []string{Subprotocol},
	CheckOrigin:  func(*http.Request) bool { return true },
}

type activity struct {
	name  string
	at    time.Time
	gen   uint64
	timer *time.Timer
}

type activityBook struct {
	mu    sync.Mutex
	quiet time.Duration
	items map[string]*activity
}

func (b *activityBook) touch(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	item := b.item(key)
	if item.name == "" {
		item.name = ActivityIdle
	}
	item.at = time.Now().UTC()
}

func (b *activityBook) input(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	item := b.item(key)
	item.gen++
	gen := item.gen
	item.name = ActivityWorking
	item.at = time.Now().UTC()
	if item.timer != nil {
		item.timer.Stop()
	}
	item.timer = time.AfterFunc(b.quiet, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if current := b.items[key]; current == item && current.gen == gen {
			current.name = ActivityIdle
			current.at = time.Now().UTC()
		}
	})
}

func (b *activityBook) idle(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	item := b.item(key)
	item.gen++
	if item.timer != nil {
		item.timer.Stop()
	}
	item.name = ActivityIdle
	item.at = time.Now().UTC()
}

func (b *activityBook) current(key string) (string, time.Time, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	item := b.items[key]
	if item == nil || item.name == "" {
		return "", time.Time{}, false
	}
	return item.name, item.at, true
}

func (b *activityBook) item(key string) *activity {
	item := b.items[key]
	if item == nil {
		item = &activity{}
		b.items[key] = item
	}
	return item
}

func (b *activityBook) stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, item := range b.items {
		item.gen++
		if item.timer != nil {
			item.timer.Stop()
		}
	}
}
