package web

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

type OIDCConfig struct {
	Issuer, ClientID, ClientSecret, Origin, GroupsClaim string
	Scopes, AllowedGroups, AllowedSubjects              []string
}
type transaction struct {
	Nonce, Verifier string
	Expires         time.Time
}
type session struct {
	Subject string
	Expires time.Time
}
type OIDC struct {
	config       OIDCConfig
	oauth        oauth2.Config
	verifier     *oidc.IDTokenVerifier
	httpClient   *http.Client
	mu           sync.Mutex
	transactions map[string]transaction
	sessions     map[string]session
}

func NewOIDC(ctx context.Context, c OIDCConfig) (*OIDC, error) {
	for _, origin := range []string{c.Issuer, c.Origin} {
		u, e := url.Parse(origin)
		if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, errors.New("OIDC issuer and application origin must use HTTPS")
		}
	}
	if c.ClientID == "" || c.ClientSecret == "" || len(c.AllowedGroups)+len(c.AllowedSubjects) == 0 {
		return nil, errors.New("OIDC client credentials and explicit operator groups or subjects required")
	}
	client := &http.Client{Timeout: 10 * time.Second}
	return newOIDC(ctx, c, client)
}
func newOIDC(ctx context.Context, c OIDCConfig, client *http.Client) (*OIDC, error) {
	ctx = oidc.ClientContext(ctx, client)
	provider, e := oidc.NewProvider(ctx, c.Issuer)
	if e != nil {
		return nil, errors.New("OIDC discovery failed")
	}
	scopes := []string{oidc.ScopeOpenID}
	for _, s := range c.Scopes {
		if s != "" && s != oidc.ScopeOpenID {
			scopes = append(scopes, s)
		}
	}
	if c.GroupsClaim == "" {
		c.GroupsClaim = "groups"
	}
	return &OIDC{config: c, oauth: oauth2.Config{ClientID: c.ClientID, ClientSecret: c.ClientSecret, Endpoint: provider.Endpoint(), RedirectURL: c.Origin + "/auth/callback", Scopes: scopes}, verifier: provider.VerifierContext(ctx, &oidc.Config{ClientID: c.ClientID}), httpClient: client, transactions: map[string]transaction{}, sessions: map[string]session{}}, nil
}
func randomID() (string, error) {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		return "", e
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
func (o *OIDC) prune() {
	now := time.Now()
	for k, v := range o.transactions {
		if now.After(v.Expires) {
			delete(o.transactions, k)
		}
	}
	for k, v := range o.sessions {
		if now.After(v.Expires) {
			delete(o.sessions, k)
		}
	}
}
func (o *OIDC) Login(w http.ResponseWriter, r *http.Request) {
	state, e := randomID()
	if e != nil {
		http.Error(w, "login unavailable", 503)
		return
	}
	nonce, e := randomID()
	if e != nil {
		http.Error(w, "login unavailable", 503)
		return
	}
	txn := transaction{Nonce: nonce, Verifier: oauth2.GenerateVerifier(), Expires: time.Now().Add(5 * time.Minute)}
	o.mu.Lock()
	o.prune()
	if len(o.transactions) >= 1024 {
		o.mu.Unlock()
		http.Error(w, "login busy", 503)
		return
	}
	o.transactions[state] = txn
	o.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "__Host-apc-login", Value: state, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 300})
	http.Redirect(w, r, o.oauth.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(txn.Verifier)), http.StatusFound)
}
func (o *OIDC) Callback(w http.ResponseWriter, r *http.Request) {
	cookie, e := r.Cookie("__Host-apc-login")
	state := r.URL.Query().Get("state")
	if e != nil || state == "" || !equal(cookie.Value, state) {
		http.Error(w, "invalid login transaction", 400)
		return
	}
	o.mu.Lock()
	txn, ok := o.transactions[state]
	delete(o.transactions, state)
	o.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "__Host-apc-login", Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: -1})
	if !ok || time.Now().After(txn.Expires) {
		http.Error(w, "login transaction expired or already used", 400)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	ctx = oidc.ClientContext(ctx, o.httpClient)
	token, e := o.oauth.Exchange(ctx, r.URL.Query().Get("code"), oauth2.VerifierOption(txn.Verifier))
	if e != nil {
		http.Error(w, "login exchange failed", 401)
		return
	}
	raw, ok := token.Extra("id_token").(string)
	if !ok {
		http.Error(w, "identity token required", 401)
		return
	}
	id, e := o.verifier.Verify(ctx, raw)
	if e != nil || id.Nonce != txn.Nonce || id.Subject == "" {
		http.Error(w, "identity verification failed", 401)
		return
	}
	var claims map[string]any
	if e = id.Claims(&claims); e != nil || !o.allowed(id.Subject, claims) {
		http.Error(w, "operator access is not granted to this identity", 403)
		return
	}
	sid, e := randomID()
	if e != nil {
		http.Error(w, "login unavailable", 503)
		return
	}
	expires := time.Now().Add(15 * time.Minute)
	if id.Expiry.Before(expires) {
		expires = id.Expiry
	}
	o.mu.Lock()
	o.prune()
	if len(o.sessions) >= 4096 {
		o.mu.Unlock()
		http.Error(w, "login busy", 503)
		return
	}
	o.sessions[sid] = session{Subject: o.config.Issuer + "#" + id.Subject, Expires: expires}
	o.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "__Host-apc-session", Value: sid, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: int(time.Until(expires).Seconds())})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
func (o *OIDC) allowed(subject string, claims map[string]any) bool {
	for _, s := range o.config.AllowedSubjects {
		if subject == s {
			return true
		}
	}
	groups, ok := claims[o.config.GroupsClaim].([]any)
	if !ok {
		return false
	}
	for _, g := range groups {
		value, ok := g.(string)
		if !ok {
			continue
		}
		for _, allowed := range o.config.AllowedGroups {
			if value == allowed {
				return true
			}
		}
	}
	return false
}
func (o *OIDC) Subject(r *http.Request) (string, bool) {
	cookie, e := r.Cookie("__Host-apc-session")
	if e != nil {
		return "", false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	sess, ok := o.sessions[cookie.Value]
	if !ok {
		return "", false
	}
	if time.Now().After(sess.Expires) {
		delete(o.sessions, cookie.Value)
		return "", false
	}
	return sess.Subject, true
}
func (o *OIDC) Logout(w http.ResponseWriter, r *http.Request) {
	if c, e := r.Cookie("__Host-apc-session"); e == nil {
		o.mu.Lock()
		delete(o.sessions, c.Value)
		o.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "__Host-apc-session", Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: -1})
	w.WriteHeader(http.StatusNoContent)
}
func list(raw string) []string {
	result := []string{}
	for _, s := range strings.Split(raw, ",") {
		s = strings.TrimSpace(s)
		if s != "" {
			result = append(result, s)
		}
	}
	return result
}
func CSV(raw string) []string { return list(raw) }
