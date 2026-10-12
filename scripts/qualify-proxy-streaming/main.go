// Run in a disposable pod with writable /tls and /policy mounts, host alias
// streaming.example.test=127.0.0.1, and permission to bind loopback port 443.
// Tests the installed proxy executable with verified TLS and no external egress.
package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sync/atomic"
	"time"
)

const host = "streaming.example.test"

func certificate() ([]byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	c := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "Disposable streaming qualification"}, DNSNames: []string{host}, IsCA: true, BasicConstraintsValid: true, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, c, c, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	raw, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: raw}), nil
}

func qualify() error {
	cert, key, err := certificate()
	if err != nil {
		return err
	}
	for path, data := range map[string][]byte{"/tls/ca.crt": cert, "/tls/ca.key": key} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			return err
		}
	}
	policy := `{"rules":[{"host":"streaming.example.test","methods":["POST"],"paths":["/events"]}],"privateHosts":["streaming.example.test"]}`
	if err := os.WriteFile("/policy/policy.json", []byte(policy), 0600); err != nil {
		return err
	}
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		return err
	}
	listener, err := tls.Listen("tcp", "127.0.0.1:443", &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}})
	if err != nil {
		return err
	}
	var hits atomic.Int64
	canceled := make(chan struct{}, 1)
	upstream := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: qualification-event\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		select {
		case canceled <- struct{}{}:
		default:
		}
	})}
	go upstream.Serve(listener)
	defer upstream.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/usr/local/bin/agent-project-controller", "proxy")
	command.Env = append(os.Environ(), "SSL_CERT_FILE=/tls/ca.crt")
	// Fixture errors and responses contain no credentials, but omit process logs.
	if err := command.Start(); err != nil {
		return err
	}
	defer func() { command.Process.Kill(); command.Wait() }()
	ready := false
	for i := 0; i < 100; i++ {
		connection, err := net.DialTimeout("tcp", "127.0.0.1:3128", 100*time.Millisecond)
		if err == nil {
			connection.Close()
			ready = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ready {
		return fmt.Errorf("installed proxy did not start")
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(cert)
	proxy, _ := url.Parse("http://127.0.0.1:3128")
	transport := &http.Transport{Proxy: http.ProxyURL(proxy), ForceAttemptHTTP2: true, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, NextProtos: []string{"h2"}}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	streamCtx, stopStream := context.WithTimeout(ctx, 5*time.Second)
	defer stopStream()
	request, _ := http.NewRequestWithContext(streamCtx, "POST", "https://"+host+"/events", nil)
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("stream headers unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 || response.ProtoMajor != 2 {
		return fmt.Errorf("stream status or HTTP/2 negotiation failed")
	}
	line, err := bufio.NewReader(response.Body).ReadString('\n')
	if err != nil || line != "data: qualification-event\n" {
		return fmt.Errorf("event unavailable before upstream completion")
	}
	response.Body.Close()
	stopStream()
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		return fmt.Errorf("upstream cancellation failed")
	}
	for _, denied := range []struct{ method, path string }{{"GET", "/events"}, {"POST", "/admin"}, {"POST", "/events-other"}, {"POST", "/events/%2e%2e/admin"}} {
		request, _ := http.NewRequestWithContext(ctx, denied.method, "https://"+host+denied.path, nil)
		response, err := client.Do(request)
		if err != nil {
			return fmt.Errorf("denial request failed")
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != 403 || response.ProtoMajor != 2 {
			return fmt.Errorf("HTTP/2 method/path denial failed")
		}
	}
	if hits.Load() != 1 {
		return fmt.Errorf("denied request reached upstream")
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"passed": true, "installed_proxy": true, "verified_tls": true, "http2": true, "event_before_upstream_completion": true, "upstream_cancellation": true, "denied_requests": 4, "upstream_requests": hits.Load(), "external_egress": false})
}

func main() {
	if err := qualify(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
