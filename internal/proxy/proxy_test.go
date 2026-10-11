package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	api "git.cluster.aimingupward.com/steve/agent-project-controller/api/v1alpha1"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
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
