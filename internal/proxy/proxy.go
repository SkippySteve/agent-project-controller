package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	api "git.cluster.aimingupward.com/steve/agent-project-controller/api/v1alpha1"
)

type Config struct {
	Rules        []api.HTTPSRule `json:"rules"`
	PrivateHosts []string        `json:"privateHosts,omitempty"`
}

var dnsHost = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

func Validate(c Config) error {
	if len(c.Rules) == 0 || len(c.Rules) > 64 {
		return errors.New("between one and 64 HTTPS rules required")
	}
	for _, r := range c.Rules {
		if len(r.Host) > 253 || !dnsHost.MatchString(r.Host) || net.ParseIP(r.Host) != nil {
			return errors.New("HTTPS rules require exact lowercase DNS hosts")
		}
		if len(r.Methods) == 0 || len(r.Paths) == 0 {
			return errors.New("HTTPS rule requires methods and literal path prefixes")
		}
		for _, m := range r.Methods {
			switch m {
			case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS":
			default:
				return errors.New("unsupported HTTPS method")
			}
		}
		for _, p := range r.Paths {
			if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "%?*\\#") || strings.Contains(p, "..") || strings.Contains(p, "//") {
				return errors.New("invalid HTTPS path prefix")
			}
		}
	}
	return nil
}
func Allowed(c Config, host, method, path string) bool {
	if strings.ContainsAny(path, "\\\x00") || strings.Contains(path, "//") {
		return false
	}
	for _, seg := range strings.Split(path, "/") {
		if seg == ".." || seg == "." {
			return false
		}
	}
	for _, r := range c.Rules {
		if r.Host != host {
			continue
		}
		ok := false
		for _, m := range r.Methods {
			if m == method {
				ok = true
			}
		}
		if !ok {
			continue
		}
		for _, p := range r.Paths {
			if path == p || p == "/" || strings.HasPrefix(path, strings.TrimSuffix(p, "/")+"/") {
				return true
			}
		}
	}
	return false
}
func GenerateCA() ([]byte, []byte, error) {
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		return nil, nil, e
	}
	serial, e := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if e != nil {
		return nil, nil, e
	}
	cert := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "Agent project inspection CA"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(365 * 24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	der, e := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if e != nil {
		return nil, nil, e
	}
	raw, e := x509.MarshalPKCS8PrivateKey(key)
	if e != nil {
		return nil, nil, e
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: raw}), nil
}

type Server struct {
	Config    Config
	CA        *x509.Certificate
	Key       any
	Transport *http.Transport
	slots     chan struct{}
	mu        sync.Mutex
	certs     map[string]tls.Certificate
}

func New(c Config, cert, key []byte) (*Server, error) {
	if e := Validate(c); e != nil {
		return nil, e
	}
	pair, e := tls.X509KeyPair(cert, key)
	if e != nil {
		return nil, e
	}
	ca, e := x509.ParseCertificate(pair.Certificate[0])
	if e != nil {
		return nil, e
	}
	if !ca.IsCA || time.Now().After(ca.NotAfter) {
		return nil, errors.New("valid inspection CA required")
	}
	s := &Server{Config: c, CA: ca, Key: pair.PrivateKey, slots: make(chan struct{}, 8), certs: map[string]tls.Certificate{}}
	s.Transport = &http.Transport{Proxy: nil, DialContext: s.dial, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 30 * time.Second, IdleConnTimeout: 30 * time.Second, MaxIdleConns: 32, ForceAttemptHTTP2: false}
	return s, nil
}

var deniedPrefixes = []netip.Prefix{netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("2001:db8::/32")}

func PublicIP(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	a = a.Unmap()
	if !a.IsGlobalUnicast() || a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() {
		return false
	}
	for _, p := range deniedPrefixes {
		if p.Contains(a) {
			return false
		}
	}
	return true
}
func (s *Server) dial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, e := net.SplitHostPort(address)
	if e != nil || port != "443" {
		return nil, errors.New("only TLS port 443 is allowed")
	}
	private := false
	for _, h := range s.Config.PrivateHosts {
		if h == host {
			private = true
		}
	}
	ips, e := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if e != nil || len(ips) == 0 {
		return nil, errors.New("upstream resolution failed")
	}
	if !private {
		for _, ip := range ips {
			if !PublicIP(ip) {
				return nil, errors.New("upstream resolves to a prohibited address")
			}
		}
	}
	d := net.Dialer{Timeout: 10 * time.Second}
	return d.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
}
func (s *Server) certificate(host string) (tls.Certificate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.certs[host]; ok && c.Leaf != nil && time.Now().Add(time.Minute).Before(c.Leaf.NotAfter) {
		return c, nil
	}
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		return tls.Certificate{}, e
	}
	serial, e := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if e != nil {
		return tls.Certificate{}, e
	}
	until := time.Now().Add(24 * time.Hour)
	if s.CA.NotAfter.Before(until) {
		until = s.CA.NotAfter
	}
	cert := &x509.Certificate{SerialNumber: serial, DNSNames: []string{host}, NotBefore: time.Now().Add(-time.Minute), NotAfter: until, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, e := x509.CreateCertificate(rand.Reader, cert, s.CA, &key.PublicKey, s.Key)
	if e != nil {
		return tls.Certificate{}, e
	}
	leaf, e := x509.ParseCertificate(der)
	if e != nil {
		return tls.Certificate{}, e
	}
	c := tls.Certificate{Certificate: [][]byte{der, s.CA.Raw}, PrivateKey: key, Leaf: leaf}
	s.certs[host] = c
	return c, nil
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c bufferedConn) Read(b []byte) (int, error) { return c.r.Read(b) }
func dropHopHeaders(h http.Header) {
	for _, v := range strings.Split(h.Get("Connection"), ",") {
		h.Del(strings.TrimSpace(v))
	}
	for _, k := range []string{"Connection", "Proxy-Authorization", "Proxy-Authenticate", "Keep-Alive", "TE", "Trailer", "Transfer-Encoding", "Upgrade"} {
		h.Del(k)
	}
}
func readRequest(reader *bufio.Reader) (*http.Request, error) {
	header := make([]byte, 0, 4096)
	for {
		line, e := reader.ReadSlice('\n')
		if e != nil {
			return nil, e
		}
		if len(header)+len(line) > 32<<10 {
			return nil, errors.New("request headers too large")
		}
		header = append(header, line...)
		if bytes.Equal(line, []byte("\r\n")) || bytes.Equal(line, []byte("\n")) {
			break
		}
	}
	return http.ReadRequest(bufio.NewReader(io.MultiReader(bytes.NewReader(header), reader)))
}

// failureKind intentionally excludes error text: transport errors can contain URLs.
func failureKind(err error) string {
	if errors.Is(err, io.EOF) {
		return "eof"
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return "unexpected_eof"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "dns"
	}
	var network net.Error
	if errors.As(err, &network) && network.Timeout() {
		return "timeout"
	}
	if strings.HasPrefix(err.Error(), "http: ContentLength=") {
		return "body_length"
	}
	return "transport"
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "CONNECT" {
		http.Error(w, "CONNECT required", 405)
		return
	}
	host, port, e := net.SplitHostPort(r.Host)
	if e != nil || port != "443" || host != strings.ToLower(host) {
		http.Error(w, "destination denied", 403)
		return
	}
	found := false
	for _, rule := range s.Config.Rules {
		if rule.Host == host {
			found = true
		}
	}
	if !found {
		http.Error(w, "destination denied", 403)
		return
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		http.Error(w, "proxy busy", 503)
		return
	}
	cert, e := s.certificate(host)
	if e != nil {
		http.Error(w, "inspection unavailable", 503)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "inspection unavailable", 503)
		return
	}
	raw, rw, e := hj.Hijack()
	if e != nil {
		return
	}
	defer raw.Close()
	raw.SetDeadline(time.Now().Add(10 * time.Minute))
	if _, e = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); e != nil {
		return
	}
	if e = rw.Flush(); e != nil {
		return
	}
	conn := tls.Server(bufferedConn{raw, rw.Reader}, &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}})
	if e = conn.HandshakeContext(r.Context()); e != nil {
		return
	}
	reader := bufio.NewReaderSize(conn, 32<<10)
	for {
		req, e := readRequest(reader)
		if e != nil {
			writeError(conn, 431, "invalid or oversized request headers")
			return
		}
		reqHost := strings.TrimSuffix(req.Host, ":443")
		// Reject alternate authorities, escaped paths and protocol upgrades. They cannot be inspected safely by this HTTP/1 implementation.
		denied := reqHost != host || req.URL.IsAbs() || req.Method == "CONNECT" || req.Header.Get("Upgrade") != "" || strings.Contains(req.URL.EscapedPath(), "%") || !Allowed(s.Config, host, req.Method, req.URL.Path)
		if denied {
			req.Body.Close()
			writeError(conn, 403, "request denied")
			return
		}
		if req.ContentLength > 128<<20 {
			req.Body.Close()
			writeError(conn, 413, "request too large")
			return
		}
		req.Body = http.MaxBytesReader(nil, req.Body, 128<<20)
		req.RequestURI = ""
		req.URL.Scheme = "https"
		req.URL.Host = host
		req.Host = host
		dropHopHeaders(req.Header)
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		req = req.WithContext(ctx)
		res, e := s.Transport.RoundTrip(req)
		if e != nil {
			req.Body.Close()
			cancel()
			slog.Warn("proxy upstream request failed", "failure_kind", failureKind(e))
			writeError(conn, 502, "upstream unavailable")
			return
		}
		dropHopHeaders(res.Header)
		res.Close = true
		e = res.Write(conn)
		res.Body.Close()
		cancel()
		if e != nil || req.Close {
			return
		}
		return // One request per tunnel bounds connection lifetime and revocation delay.
	}
}
func writeError(w io.Writer, status int, message string) {
	res := http.Response{StatusCode: status, Status: fmt.Sprintf("%d %s", status, http.StatusText(status)), ProtoMajor: 1, ProtoMinor: 1, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(message)), ContentLength: int64(len(message)), Close: true}
	_ = res.Write(w)
}
func Run(ctx context.Context) error {
	raw, e := os.ReadFile("/policy/policy.json")
	if e != nil {
		return e
	}
	var cfg Config
	if e = json.Unmarshal(raw, &cfg); e != nil {
		return e
	}
	cert, e := os.ReadFile("/tls/ca.crt")
	if e != nil {
		return e
	}
	key, e := os.ReadFile("/tls/ca.key")
	if e != nil {
		return e
	}
	s, e := New(cfg, cert, key)
	if e != nil {
		return e
	}
	server := &http.Server{Addr: ":3128", Handler: s, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 32 << 10}
	go func() { <-ctx.Done(); _ = server.Close() }()
	e = server.ListenAndServe()
	if errors.Is(e, http.ErrServerClosed) {
		return nil
	}
	return e
}

// URL is used only for validated operator configuration, never arbitrary workload input.
func ForgejoRule(origin, repo string) (api.HTTPSRule, error) {
	u, e := url.Parse(origin)
	if e != nil || u.Scheme != "https" || u.Port() != "" && u.Port() != "443" {
		return api.HTTPSRule{}, errors.New("Forgejo requires HTTPS on port 443")
	}
	return api.HTTPSRule{Host: u.Hostname(), Methods: []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE"}, Paths: []string{"/" + repo + ".git", "/api/v1/repos/" + repo}}, nil
}
