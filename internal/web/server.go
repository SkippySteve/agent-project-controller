package web

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	api "git.cluster.aimingupward.com/steve/agent-project-controller/api/v1alpha1"
	"git.cluster.aimingupward.com/steve/agent-project-controller/internal/forgejo"
	"git.cluster.aimingupward.com/steve/agent-project-controller/internal/proxy"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

//go:embed index.html
var index []byte

//go:embed app.js
var app []byte

//go:embed style.css
var style []byte

type Server struct {
	Client                             client.Client
	Host                               forgejo.Host
	Namespace, Owner, Password, Origin string
	OIDC                               *OIDC
	AllowBasic                         bool
}
type actorKey struct{}

func equal(a, b string) bool {
	aHash := sha256.Sum256([]byte(a))
	bHash := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(aHash[:], bHash[:]) == 1
}
func (s *Server) Handler() (http.Handler, error) {
	u, e := url.Parse(s.Origin)
	if e != nil || u.Host == "" || (u.Scheme != "https" && u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1") || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || s.Password != "" && len(s.Password) < 32 || s.AllowBasic && len(s.Password) < 32 || s.OIDC == nil && !s.AllowBasic {
		return nil, errors.New("dashboard requires an explicit HTTPS origin and OIDC or explicitly enabled static authentication")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(index)
	})
	mux.HandleFunc("GET /app.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Write(app)
	})
	mux.HandleFunc("GET /style.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Write(style)
	})
	mux.HandleFunc("GET /api/v1/repositories", func(w http.ResponseWriter, r *http.Request) {
		repos, e := s.Host.Discover(r.Context())
		if e != nil {
			http.Error(w, "repository discovery unavailable", 502)
			return
		}
		writeJSON(w, repos)
	})
	mux.HandleFunc("GET /api/v1/session", func(w http.ResponseWriter, r *http.Request) {
		actor, _ := r.Context().Value(actorKey{}).(string)
		writeJSON(w, map[string]any{"actor": actor, "oidc": s.OIDC != nil})
	})
	mux.HandleFunc("GET /api/v1/workspaces", func(w http.ResponseWriter, r *http.Request) {
		list := &api.ProjectWorkspaceList{}
		if e := s.Client.List(r.Context(), list, client.InNamespace(s.Namespace)); e != nil {
			http.Error(w, "workspace catalog unavailable", 503)
			return
		}
		writeJSON(w, list.Items)
	})
	mux.HandleFunc("POST /api/v1/workspaces", s.create)
	mux.HandleFunc("PATCH /api/v1/workspaces/{name}", s.change)
	mux.HandleFunc("GET /api/v1/workspaces/{name}/audit", s.audit)
	if s.OIDC != nil {
		mux.HandleFunc("POST /auth/logout", s.OIDC.Logout)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		if s.OIDC != nil && r.Method == "GET" {
			if r.URL.Path == "/auth/login" {
				s.OIDC.Login(w, r)
				return
			}
			if r.URL.Path == "/auth/callback" {
				s.OIDC.Callback(w, r)
				return
			}
		}
		bearer := s.Password != "" && strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") && equal(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), s.Password)
		user, pass, basic := r.BasicAuth()
		basic = s.AllowBasic && s.Password != "" && basic && user == "operator" && equal(pass, s.Password)
		subject := ""
		session := false
		if s.OIDC != nil {
			subject, session = s.OIDC.Subject(r)
		}
		if !bearer && !basic && !session {
			if s.OIDC != nil && r.Method == "GET" && r.URL.Path == "/" {
				http.Redirect(w, r, "/auth/login", 302)
				return
			}
			if s.AllowBasic {
				w.Header().Set("WWW-Authenticate", `Basic realm="Project workspaces", charset="UTF-8"`)
			}
			http.Error(w, "authentication required; sign in at /auth/login", 401)
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" {
			origin := r.Header.Get("Origin")
			if origin != "" && origin != s.Origin || (basic || session) && origin != s.Origin {
				http.Error(w, "same-origin request required", 403)
				return
			}
			if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
				http.Error(w, "JSON required", 415)
				return
			}
		}
		if bearer {
			subject = "automation-token"
		} else if basic {
			subject = "static-operator"
		}
		ctx, cancel := context.WithTimeout(context.WithValue(r.Context(), actorKey{}, subject), 25*time.Second)
		defer cancel()
		mux.ServeHTTP(w, r.WithContext(ctx))
	}), nil
}
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		return e
	}
	var extra any
	if e := d.Decode(&extra); e != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}

var validName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Name       string          `json:"name"`
		Repository string          `json:"repository"`
		HTTPS      []api.HTTPSRule `json:"https,omitempty"`
	}
	if e := decode(w, r, &request); e != nil || !validName.MatchString(request.Name) || forgejo.ValidateRepository(request.Repository, s.Owner) != nil {
		http.Error(w, "invalid project name or repository", 400)
		return
	}
	if len(request.HTTPS) > 0 && proxy.Validate(proxy.Config{Rules: request.HTTPS}) != nil {
		http.Error(w, "invalid HTTPS policy", 400)
		return
	}
	actor, _ := r.Context().Value(actorKey{}).(string)
	obj := &api.ProjectWorkspace{TypeMeta: metav1.TypeMeta{APIVersion: api.GroupVersion.String(), Kind: "ProjectWorkspace"}, ObjectMeta: metav1.ObjectMeta{Name: request.Name, Namespace: s.Namespace, Annotations: map[string]string{"projects.agentwork.dev/requested-by": actor}}, Spec: api.WorkspaceSpec{Repository: request.Repository, DesiredState: "Running", HTTPS: request.HTTPS}}
	if e := s.Client.Create(r.Context(), obj); e != nil {
		if apierrors.IsAlreadyExists(e) {
			http.Error(w, "project name already exists", 409)
		} else {
			http.Error(w, "project request could not be saved", 503)
		}
		return
	}
	w.WriteHeader(201)
	writeJSON(w, obj)
}
func (s *Server) change(w http.ResponseWriter, r *http.Request) {
	var request struct {
		DesiredState    string `json:"desiredState"`
		ResourceVersion string `json:"resourceVersion"`
	}
	if e := decode(w, r, &request); e != nil || request.ResourceVersion == "" || (request.DesiredState != "Running" && request.DesiredState != "Paused" && request.DesiredState != "Removed") {
		http.Error(w, "state and resourceVersion required", 400)
		return
	}
	obj := &api.ProjectWorkspace{}
	if e := s.Client.Get(r.Context(), types.NamespacedName{Namespace: s.Namespace, Name: r.PathValue("name")}, obj); e != nil {
		http.Error(w, "project unavailable", 404)
		return
	}
	if obj.ResourceVersion != request.ResourceVersion {
		http.Error(w, "project changed; refresh before retrying", 409)
		return
	}
	if !obj.DeletionTimestamp.IsZero() || obj.Spec.DesiredState == "Removed" {
		http.Error(w, "removed projects cannot be reopened; create a new project", 409)
		return
	}
	actor, _ := r.Context().Value(actorKey{}).(string)
	if obj.Annotations == nil {
		obj.Annotations = map[string]string{}
	}
	obj.Annotations["projects.agentwork.dev/requested-by"] = actor
	obj.Spec.DesiredState = request.DesiredState
	if e := s.Client.Update(r.Context(), obj); e != nil {
		http.Error(w, "project update failed; refresh before retrying", 409)
		return
	}
	writeJSON(w, obj)
}
func (s *Server) audit(w http.ResponseWriter, r *http.Request) {
	obj := &api.ProjectWorkspace{}
	if e := s.Client.Get(r.Context(), types.NamespacedName{Namespace: s.Namespace, Name: r.PathValue("name")}, obj); e != nil {
		http.Error(w, "project unavailable", 404)
		return
	}
	var list corev1.ConfigMapList
	if e := s.Client.List(r.Context(), &list, client.InNamespace(s.Namespace), client.MatchingLabels{api.OwnerLabel: string(obj.UID)}); e != nil {
		http.Error(w, "audit unavailable", 503)
		return
	}
	items := []map[string]string{}
	for _, cm := range list.Items {
		if strings.HasPrefix(cm.Name, "audit-") {
			items = append(items, cm.Data)
		}
	}
	writeJSON(w, items)
}
