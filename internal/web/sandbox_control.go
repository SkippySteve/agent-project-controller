package web

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// SandboxControl mounts an existing trusted control service in the authenticated console.
// Its backend retains ownership of existing worker policies, previews and their audit history.
// The URL is operator configuration, never supplied by a browser or workspace.
type SandboxControl struct{ URL, Origin, Operator string }

func (c SandboxControl) Validate() error {
	target, err := url.Parse(c.URL)
	if err != nil || target.Host == "" || target.User != nil || target.RawQuery != "" || target.Fragment != "" || target.Path != "" || (target.Scheme != "http" && target.Scheme != "https") {
		return errors.New("sandbox control requires an explicit trusted HTTP(S) backend origin")
	}
	origin, err := url.Parse(c.Origin)
	if err != nil || origin.Host == "" || origin.Scheme != "https" || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" || c.Operator == "" || strings.ContainsAny(c.Operator, "\r\n") {
		return errors.New("sandbox control requires its HTTPS origin and delegated operator")
	}
	return nil
}

func sandboxRoute(path string, method string) bool {
	if method == "GET" || method == "HEAD" {
		switch path {
		case "/", "/status", "/history", "/export":
			return true
		}
		return strings.HasPrefix(path, "/static/") && !strings.Contains(path, "..") && !strings.Contains(path, "//")
	}
	if method != "POST" {
		return false
	}
	switch path {
	case "/egress", "/access", "/policy", "/packages", "/packages-import", "/allow-request", "/catalog", "/lifecycle", "/rollback", "/profile", "/decision":
		return true
	}
	return false
}

func (c SandboxControl) Handler(consoleOrigin string) http.Handler {
	target, _ := url.Parse(c.URL)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.ResponseHeaderTimeout = 20 * time.Second
	proxy := &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(p *httputil.ProxyRequest) {
			p.SetURL(target)
			p.Out.URL.Path = strings.TrimPrefix(p.In.URL.Path, "/sandboxes")
			p.Out.URL.RawPath = ""
			// Do not delegate the console's cookie or automation credential to the old backend.
			p.Out.Header.Del("Authorization")
			p.Out.Header.Del("Cookie")
			for _, cookie := range p.In.Cookies() {
				if cookie.Name == "__Host-sandbox-csrf" {
					p.Out.AddCookie(cookie)
				}
			}
			for key := range p.Out.Header {
				if strings.HasPrefix(strings.ToLower(key), "x-authentik-") || strings.HasPrefix(strings.ToLower(key), "x-forwarded-") {
					p.Out.Header.Del(key)
				}
			}
			p.Out.Header.Set("X-authentik-username", c.Operator)
			p.Out.Header.Del("Accept-Encoding")
			p.Out.Header.Del("Referer")
			if p.In.Method == "POST" {
				p.Out.Header.Set("Origin", c.Origin)
			}
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "Existing sandbox controls are unavailable. The request could not be confirmed; refresh before retrying. Project controls remain available.", http.StatusBadGateway)
		},
	}
	// Preserve only the CSRF cookie without forwarding backend authentication cookies.
	proxy.ModifyResponse = func(response *http.Response) error {
		cookies := response.Cookies()
		response.Header.Del("Set-Cookie")
		for _, cookie := range cookies {
			if cookie.Name == "__Host-sandbox-csrf" && cookie.Secure && cookie.Path == "/" && cookie.Domain == "" {
				response.Header.Add("Set-Cookie", cookie.String())
			}
		}
		response.Header.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		// no-referrer makes browser form navigations send Origin: null. Preserve
		// the same-origin form fence without disclosing referrers to other sites.
		response.Header.Set("Referrer-Policy", "same-origin")
		return c.rewriteResponse(response)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/sandboxes")
		if !sandboxRoute(path, r.Method) || strings.ContainsAny(r.URL.Path, "\\\x00") || r.URL.RawPath != "" && r.URL.RawPath != r.URL.Path {
			http.Error(w, "sandbox control route unavailable", 404)
			return
		}
		if r.Method == "POST" && r.Header.Get("Origin") != consoleOrigin {
			http.Error(w, "same-origin request required", 403)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 26<<20)
		proxy.ServeHTTP(w, r)
	})
}

func (c SandboxControl) mountURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if parsed.IsAbs() {
		prefix := c.Origin
		if !strings.HasPrefix(raw, prefix+"/") && raw != prefix {
			return "", errors.New("backend redirect leaves the console")
		}
		raw = strings.TrimPrefix(raw, prefix)
	}
	if strings.HasPrefix(raw, "//") {
		return "", errors.New("backend redirect leaves the console")
	}
	if !strings.HasPrefix(raw, "/") {
		raw = "/" + raw
	}
	return "/sandboxes" + raw, nil
}

func (c SandboxControl) rewriteResponse(response *http.Response) error {
	if location := response.Header.Get("Location"); location != "" {
		mounted, err := c.mountURL(location)
		if err != nil {
			return err
		}
		response.Header.Set("Location", mounted)
	}
	if response.Request.Method == "HEAD" || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/html") {
		return nil
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
	if err != nil || len(raw) > 4<<20 {
		return errors.New("sandbox control page exceeds the console bound")
	}
	root, err := html.Parse(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		for i, attribute := range node.Attr {
			switch attribute.Key {
			case "href", "src", "action", "formaction", "hx-get", "hx-post", "hx-put", "hx-patch", "hx-delete":
				if strings.HasPrefix(attribute.Val, "/") && !strings.HasPrefix(attribute.Val, "//") || strings.HasPrefix(attribute.Val, c.Origin+"/") {
					if mounted, err := c.mountURL(attribute.Val); err == nil {
						node.Attr[i].Val = mounted
					}
				}
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
		if node.Type == html.ElementNode && node.Data == "head" {
			link := &html.Node{Type: html.ElementNode, Data: "link", Attr: []html.Attribute{{Key: "rel", Val: "stylesheet"}, {Key: "href", Val: "/console.css"}}}
			node.AppendChild(link)
			node.AppendChild(&html.Node{Type: html.ElementNode, Data: "script", Attr: []html.Attribute{{Key: "src", Val: "/shell.js"}, {Key: "defer", Val: ""}}})
		}
		if node.Type == html.ElementNode && node.Data == "body" {
			shell, _ := html.ParseFragment(strings.NewReader(`<nav class="console-nav" aria-label="Console"><a href="/">Projects</a><a href="/sandboxes/" aria-current="page">Existing sandboxes</a><button id="console-logout" type="button" hidden>Sign out</button><span id="console-message" role="status"></span></nav>`), nil)
			// ParseFragment with nil returns a document wrapper. Move only its body content.
			for _, fragment := range shell {
				var find func(*html.Node)
				find = func(n *html.Node) {
					if n.Type == html.ElementNode && n.Data == "nav" {
						n.Parent.RemoveChild(n)
						node.InsertBefore(n, node.FirstChild)
						return
					}
					for child := n.FirstChild; child != nil; {
						next := child.NextSibling
						find(child)
						child = next
					}
				}
				find(fragment)
			}
		}
	}
	walk(root)
	var encoded bytes.Buffer
	if err = html.Render(&encoded, root); err != nil {
		return err
	}
	response.Body = io.NopCloser(&encoded)
	response.ContentLength = int64(encoded.Len())
	response.Header.Set("Content-Length", strconv.Itoa(encoded.Len()))
	response.Header.Del("Content-Encoding")
	return nil
}
