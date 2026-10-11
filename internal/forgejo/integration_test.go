package forgejo

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	api "git.cluster.aimingupward.com/steve/agent-project-controller/api/v1alpha1"
	inspection "git.cluster.aimingupward.com/steve/agent-project-controller/internal/proxy"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

func TestRealForgejo(t *testing.T) {
	origin := os.Getenv("APC_TEST_FORGEJO_ORIGIN")
	if origin == "" {
		t.Skip("run scripts/qualify-forgejo.py with a verified Forgejo binary")
	}
	b, e := os.ReadFile(os.Getenv("APC_TEST_FORGEJO_TOKEN_FILE"))
	if e != nil {
		t.Fatal(e)
	}
	target, e := url.Parse(origin)
	if e != nil {
		t.Fatal(e)
	}
	server := httptest.NewTLSServer(httputil.NewSingleHostReverseProxy(target))
	defer server.Close()
	c, e := New(server.URL, "operator", string(b))
	if e != nil {
		t.Fatal(e)
	}
	c.HTTP = server.Client()
	ctx := context.Background()
	suffix := make([]byte, 8)
	_, _ = rand.Read(suffix)
	name := "fixture-" + hex.EncodeToString(suffix)
	repo := "operator/" + name
	if e = c.call(ctx, "POST", "/user/repos", map[string]any{"name": name, "private": true, "auto_init": true, "default_branch": "main", "readme": "Default"}, nil, nil); e != nil {
		t.Fatal(e)
	}
	cred := Credentials{Username: "apc-" + hex.EncodeToString(suffix), Password: hex.EncodeToString(suffix) + "-fixture-password-long-enough"}
	token, e := c.Grant(ctx, repo, cred)
	if e != nil {
		t.Fatal("grant", e)
	}
	cred.Token = token
	repeat, e := c.Grant(ctx, repo, cred)
	if e != nil || repeat != token {
		t.Fatal("grant retry did not reuse credential", e)
	}
	// Simulate a crash after issuing a token but before persisting its secret value.
	orphan := token
	cred.Token = ""
	token, e = c.Grant(ctx, repo, cred)
	if e != nil || token == orphan {
		t.Fatal("orphan token recovery failed", e)
	}
	cred.Token = token
	req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/api/v1/repos/"+repo, nil)
	req.Header.Set("Authorization", "token "+orphan)
	res, e := server.Client().Do(req)
	if e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	if res.StatusCode == 200 {
		t.Fatal("orphan token remains usable")
	}
	// Use real Git through the inspecting CONNECT proxy, with an independent upstream TLS connection.
	ca, key, e := inspection.GenerateCA()
	if e != nil {
		t.Fatal(e)
	}
	virtualOrigin := "https://git.fixture.example"
	inspector, e := inspection.New(inspection.Config{Rules: []api.HTTPSRule{{Host: "git.fixture.example", Methods: []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE"}, Paths: []string{"/" + repo + ".git", "/api/v1/repos/" + repo, "/api/v1/repos/operator/" + name + "-other"}}}}, ca, key)
	if e != nil {
		t.Fatal(e)
	}
	upstreamURL, _ := url.Parse(server.URL)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	inspector.Transport.TLSClientConfig = &tls.Config{RootCAs: roots, ServerName: "example.com", MinVersion: tls.VersionTLS12}
	inspector.Transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, upstreamURL.Host)
	}
	proxyServer := httptest.NewServer(inspector)
	defer proxyServer.Close()
	proxyURL, _ := url.Parse(proxyServer.URL)
	clientRoots := x509.NewCertPool()
	clientRoots.AppendCertsFromPEM(ca)
	agentHTTP := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: clientRoots, MinVersion: tls.VersionTLS12}}}
	dir := t.TempDir()
	caFile := filepath.Join(dir, "ca.pem")
	_ = os.WriteFile(caFile, ca, 0600)
	env := append(os.Environ(), "GIT_CONFIG_COUNT=4", "GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0=Authorization: Basic "+base64.StdEncoding.EncodeToString([]byte(cred.Username+":"+token)), "GIT_CONFIG_KEY_1=user.name", "GIT_CONFIG_VALUE_1=Fixture agent", "GIT_CONFIG_KEY_2=user.email", "GIT_CONFIG_VALUE_2=fixture@agentwork.invalid", "GIT_CONFIG_KEY_3=http.proxy", "GIT_CONFIG_VALUE_3="+proxyServer.URL, "GIT_SSL_CAINFO="+caFile, "GIT_TERMINAL_PROMPT=0")
	work := filepath.Join(dir, "repo")
	git := func(allowed bool, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = env
		_, err := cmd.CombinedOutput()
		if (err == nil) != allowed {
			t.Fatalf("Git operation %s: expected success=%t", args[0], allowed)
		}
	}
	git(true, "clone", virtualOrigin+"/"+repo+".git", work)
	git(true, "-C", work, "switch", "-c", "agent/qualification")
	_ = os.WriteFile(filepath.Join(work, "qualification.txt"), []byte("fixture change\n"), 0600)
	git(true, "-C", work, "add", "qualification.txt")
	git(true, "-C", work, "commit", "-m", "Fixture change")
	git(true, "-C", work, "push", "origin", "HEAD:refs/heads/agent/qualification")
	git(false, "-C", work, "push", "origin", "HEAD:refs/heads/main")
	git(false, "-C", work, "push", "--force", "origin", "HEAD:refs/heads/main")
	git(false, "-C", work, "push", "origin", ":refs/heads/main")
	agentCall := func(method, path string, body any, out any) error {
		saved := c.AdminToken
		oldOrigin, oldHTTP := c.Origin, c.HTTP
		c.Origin = virtualOrigin
		c.HTTP = agentHTTP
		c.AdminToken = token
		defer func() { c.AdminToken = saved; c.Origin = oldOrigin; c.HTTP = oldHTTP }()
		return c.call(ctx, method, path, body, out, nil)
	}
	var pr struct {
		Number int64 `json:"number"`
	}
	if e = agentCall("POST", "/repos/"+repo+"/pulls", map[string]any{"head": "agent/qualification", "base": "main", "title": "Fixture pull request"}, &pr); e != nil {
		t.Fatal("agent PR creation failed", e)
	}
	if e = agentCall("POST", "/repos/"+repo+"/pulls/"+fmtInt(pr.Number)+"/merge", map[string]any{"Do": "merge"}, nil); e == nil {
		t.Fatal("agent merged main")
	}
	// Repository scope denies a second private repository even when accidentally given another collaborator grant.
	second := name + "-other"
	if e = c.call(ctx, "POST", "/user/repos", map[string]any{"name": second, "private": true, "auto_init": true}, nil, nil); e != nil {
		t.Fatal(e)
	}
	if e = c.call(ctx, "PUT", "/repos/operator/"+second+"/collaborators/"+cred.Username, map[string]string{"permission": "write"}, nil, nil); e != nil {
		t.Fatal(e)
	}
	if e = agentCall("GET", "/repos/operator/"+second, nil, &map[string]any{}); e == nil {
		t.Fatal("token escaped repository scope")
	}
	if e = c.Revoke(ctx, repo, cred); e != nil {
		t.Fatal("revoke", e)
	}
	git(false, "-C", work, "ls-remote", "origin")
	if e = agentCall("GET", "/repos/"+repo, nil, &map[string]any{}); e == nil {
		t.Fatal("revoked token still reads private source")
	}
	if e = c.Revoke(ctx, repo, cred); e != nil {
		t.Fatal("revocation retry failed", e)
	}
	if _, e = c.Grant(ctx, repo, cred); e == nil {
		t.Fatal("disabled project identity reopened")
	}
	t.Log("Qualified HTTPS clone, branch push, protected-main rejection, PR creation, merge denial, repository token scope and repeatable revocation")
}
func fmtInt(i int64) string { return strconv.FormatInt(i, 10) }
