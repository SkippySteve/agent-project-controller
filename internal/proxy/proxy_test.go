package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	api "git.cluster.aimingupward.com/steve/agent-project-controller/api/v1alpha1"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestInspectedTLSRequests(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "" {
			t.Error("proxy credential leaked")
		}
		w.Write([]byte("allowed"))
	}))
	defer upstream.Close()
	cert, key, e := GenerateCA()
	if e != nil {
		t.Fatal(e)
	}
	config := Config{Rules: []api.HTTPSRule{{Host: "git.example.com", Methods: []string{"GET"}, Paths: []string{"/repo.git"}}}}
	p, e := New(config, cert, key)
	if e != nil {
		t.Fatal(e)
	}
	upstreamURL, _ := url.Parse(upstream.URL)
	roots := x509.NewCertPool()
	roots.AddCert(upstream.Certificate())
	p.Transport.TLSClientConfig = &tls.Config{RootCAs: roots}
	p.Transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, upstreamURL.Host)
	}
	// httptest cert is for example.com; preserve the requested authority at the proxy but verify the upstream fixture identity.
	p.Transport.TLSClientConfig.ServerName = "example.com"
	server := httptest.NewServer(p)
	defer server.Close()
	serverURL, _ := url.Parse(server.URL)
	ca := x509.NewCertPool()
	ca.AppendCertsFromPEM(cert)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(serverURL), TLSClientConfig: &tls.Config{RootCAs: ca}}}
	for _, test := range []struct {
		method, path string
		status       int
	}{{"GET", "/repo.git/info/refs", 200}, {"POST", "/repo.git/git-receive-pack", 403}, {"GET", "/repo.git-other", 403}, {"GET", "/repo.git/%2e%2e/admin", 403}, {"GET", "/api/v1/admin/users", 403}} {
		req, _ := http.NewRequest(test.method, "https://git.example.com"+test.path, nil)
		res, e := client.Do(req)
		if e != nil {
			t.Fatalf("%s: %v", test.path, e)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if res.StatusCode != test.status {
			t.Errorf("%s %s got %d want %d", test.method, test.path, res.StatusCode, test.status)
		}
	}
	res, e := client.Get("https://unknown.example.com/")
	if e == nil {
		res.Body.Close()
		t.Fatal("unexpected unlisted destination access")
	}
}
func TestInspectedHTTP2(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("allowed"))
	}))
	defer upstream.Close()
	cert, key, e := GenerateCA()
	if e != nil {
		t.Fatal(e)
	}
	config := Config{Rules: []api.HTTPSRule{{Host: "git.example.com", Methods: []string{"GET"}, Paths: []string{"/repo.git"}}}}
	p, e := New(config, cert, key)
	if e != nil {
		t.Fatal(e)
	}
	upstreamURL, _ := url.Parse(upstream.URL)
	roots := x509.NewCertPool()
	roots.AddCert(upstream.Certificate())
	p.Transport.TLSClientConfig = &tls.Config{RootCAs: roots, ServerName: "example.com"}
	p.Transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, upstreamURL.Host)
	}
	server := httptest.NewServer(p)
	defer server.Close()
	serverURL, _ := url.Parse(server.URL)
	ca := x509.NewCertPool()
	ca.AppendCertsFromPEM(cert)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(serverURL), ForceAttemptHTTP2: true, TLSClientConfig: &tls.Config{RootCAs: ca, NextProtos: []string{"h2"}}}}
	res, e := client.Get("https://git.example.com/repo.git/info/refs")
	if e != nil {
		t.Fatal(e)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || string(body) != "allowed" || res.TLS == nil || res.TLS.NegotiatedProtocol != "h2" {
		t.Fatalf("status %d protocol %v body %q", res.StatusCode, res.TLS, body)
	}
	denied, e := client.Get("https://git.example.com/admin")
	if e != nil {
		t.Fatal(e)
	}
	denied.Body.Close()
	if denied.StatusCode != 403 || denied.TLS == nil || denied.TLS.NegotiatedProtocol != "h2" {
		t.Fatalf("denied status %d protocol %v", denied.StatusCode, denied.TLS)
	}
}
func TestDestinationSafety(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "100.64.0.1", "::1", "::ffff:127.0.0.1", "fc00::1", "2001:db8::1"} {
		if PublicIP(net.ParseIP(s)) {
			t.Error("prohibited address accepted", s)
		}
	}
	if !PublicIP(net.ParseIP("1.1.1.1")) {
		t.Error("public IP rejected")
	}
}
func TestGitHubRulesStayOnOneRepository(t *testing.T) {
	rules, err := GitHubRules("owner/repo")
	if err != nil || len(rules) != 2 {
		t.Fatal(err)
	}
	config := Config{Rules: rules}
	if err = Validate(config); err != nil {
		t.Fatal(err)
	}
	if !Allowed(config, "github.com", "GET", "/owner/repo.git/info/refs") || !Allowed(config, "github.com", "POST", "/owner/repo.git/git-receive-pack") {
		t.Fatal("git transport denied")
	}
	if !Allowed(config, "api.github.com", "POST", "/repos/owner/repo/pulls") || !Allowed(config, "api.github.com", "GET", "/repos/owner/repo") {
		t.Fatal("repository API denied")
	}
	for _, attempt := range []struct{ host, method, path string }{
		{"github.com", "PUT", "/owner/repo.git/info/refs"},
		{"github.com", "GET", "/other/repo.git/info/refs"},
		{"api.github.com", "GET", "/repos/other/repo"},
		{"api.github.com", "GET", "/repos/owner/repository"},
		{"api.github.com", "DELETE", "/user"},
		{"api.github.com", "GET", "/repos/owner/repo.git"},
	} {
		if Allowed(config, attempt.host, attempt.method, attempt.path) {
			t.Error(attempt)
		}
	}
	if _, err = GitHubRules("owner/../repo"); err == nil {
		t.Fatal("path escape accepted")
	}
}

func TestPathBoundaries(t *testing.T) {
	c := Config{Rules: []api.HTTPSRule{{Host: "api.example.com", Methods: []string{"GET"}, Paths: []string{"/v1/project"}}}}
	for _, path := range []string{"/v1/projects", "/v1/project/../admin", "/v1/project\\admin", "/v1//project"} {
		if Allowed(c, "api.example.com", "GET", path) {
			t.Error(path)
		}
	}
	if !Allowed(c, "api.example.com", "GET", "/v1/project/items") {
		t.Error("child path denied")
	}
	c.Rules[0].Host = "*.example.com"
	if Validate(c) == nil {
		t.Fatal("wildcard accepted")
	}
	if !strings.Contains(c.Rules[0].Host, "*") {
		t.Fatal("test fixture")
	}
}

func TestTransportFailureClassificationDoesNotExposeErrorText(t *testing.T) {
	if failureKind(io.EOF) != "eof" {
		t.Fatal("EOF not classified")
	}
	if failureKind(errors.New("https://private.example/path?token=private-token")) != "transport" {
		t.Fatal("unclassified errors must produce only a fixed label")
	}
	if failureKind(errors.New("http: ContentLength=12 with Body length 0")) != "body_length" {
		t.Fatal("body length mismatch not classified")
	}
}

func TestInspectedHTTP2StreamsBeforeUpstreamCompletes(t *testing.T) {
	release := make(chan struct{})
	canceled := make(chan struct{})
	defer close(release)
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			close(canceled)
		}
	}))
	defer upstream.Close()
	cert, key, err := GenerateCA()
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(Config{Rules: []api.HTTPSRule{{Host: "model.example.com", Methods: []string{"POST"}, Paths: []string{"/responses"}}}}, cert, key)
	if err != nil {
		t.Fatal(err)
	}
	upstreamURL, _ := url.Parse(upstream.URL)
	roots := x509.NewCertPool()
	roots.AddCert(upstream.Certificate())
	p.Transport.TLSClientConfig = &tls.Config{RootCAs: roots, ServerName: "example.com"}
	p.Transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, upstreamURL.Host)
	}
	server := httptest.NewServer(p)
	defer server.Close()
	serverURL, _ := url.Parse(server.URL)
	ca := x509.NewCertPool()
	ca.AppendCertsFromPEM(cert)
	transport := &http.Transport{Proxy: http.ProxyURL(serverURL), ForceAttemptHTTP2: true, TLSClientConfig: &tls.Config{RootCAs: ca, NextProtos: []string{"h2"}}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://model.example.com/responses", strings.NewReader("{}"))
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("stream headers were buffered: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 || res.ProtoMajor != 2 {
		t.Fatalf("status %d protocol %s", res.StatusCode, res.Proto)
	}
	line, err := bufio.NewReader(res.Body).ReadString('\n')
	if err != nil || line != "data: first\n" {
		t.Fatalf("stream event was buffered: %q %v", line, err)
	}
	res.Body.Close()
	cancel()
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("downstream cancellation did not stop the upstream stream")
	}
}
