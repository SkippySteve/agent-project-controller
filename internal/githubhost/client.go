// Package githubhost grants one GitHub repository at a time through a GitHub App.
// It implements forgejo.Host so the controller can select Forgejo or GitHub,
// never both in one process. GitHub.com is the only supported API.
package githubhost

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"git.cluster.aimingupward.com/steve/agent-project-controller/internal/forgejo"
)

const (
	apiBase          = "https://api.github.com"
	apiVersion       = "2022-11-28"
	userAgent        = "agent-project-controller"
	tokenReuseMargin = 20 * time.Minute
	jwtLifetime      = 8 * time.Minute
	jwtSkew          = 60 * time.Second
)

var workspacePermissions = map[string]string{"contents": "write", "metadata": "read", "pull_requests": "write"}
var controllerPermissions = map[string]string{"administration": "read", "metadata": "read"}

type tokenRecord struct {
	value  string
	expiry time.Time
}

// Client talks to GitHub.com with a GitHub App private key. Workspace tokens
// and the controller token stay in this process. GitHub cannot list
// installation tokens, so a value that is minted and then lost in a crash
// remains valid until GitHub expires it.
type Client struct {
	Owner          string
	AppID          int64
	InstallationID int64
	HTTP           *http.Client
	api            string
	key            *rsa.PrivateKey
	now            func() time.Time
	mu             sync.Mutex
	slug           string
	controller     tokenRecord
	byRepo         map[string]tokenRecord
}

func New(owner string, appID, installationID int64, privateKey []byte) (*Client, error) {
	if forgejo.ValidateRepository(owner+"/project", owner) != nil {
		return nil, errors.New("GitHub owner is invalid")
	}
	if appID <= 0 || installationID <= 0 {
		return nil, errors.New("GitHub App id and installation id must be positive")
	}
	key, e := parseKey(privateKey)
	if e != nil {
		return nil, e
	}
	return &Client{
		Owner:          owner,
		AppID:          appID,
		InstallationID: installationID,
		key:            key,
		api:            apiBase,
		byRepo:         map[string]tokenRecord{},
		HTTP:           &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}, nil
}

func parseKey(raw []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("GitHub App private key must be a PEM RSA key of at least 2048 bits")
	}
	var key *rsa.PrivateKey
	if parsed, e := x509.ParsePKCS1PrivateKey(block.Bytes); e == nil {
		key = parsed
	} else if parsed, e := x509.ParsePKCS8PrivateKey(block.Bytes); e == nil {
		rsaKey, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("GitHub App private key must be a PEM RSA key of at least 2048 bits")
		}
		key = rsaKey
	} else {
		return nil, errors.New("GitHub App private key must be a PEM RSA key of at least 2048 bits")
	}
	if key.N.BitLen() < 2048 {
		return nil, errors.New("GitHub App private key must be a PEM RSA key of at least 2048 bits")
	}
	return key, nil
}

func (c *Client) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

type responseError struct{ Status int }

func (e responseError) Error() string { return fmt.Sprintf("GitHub returned HTTP %d", e.Status) }
func statusIs(e error, status int) bool {
	var r responseError
	return errors.As(e, &r) && r.Status == status
}

type jwtClaims struct {
	Issuer    string `json:"iss"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
}

func (c *Client) jwt() (string, error) {
	now := c.clock()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload, e := json.Marshal(jwtClaims{Issuer: fmt.Sprint(c.AppID), IssuedAt: now.Add(-jwtSkew).Unix(), ExpiresAt: now.Add(jwtLifetime).Unix()})
	if e != nil {
		return "", e
	}
	body := header + "." + base64.RawURLEncoding.EncodeToString(payload)
	sum := sha256.Sum256([]byte(body))
	sig, e := rsa.SignPKCS1v15(rand.Reader, c.key, crypto.SHA256, sum[:])
	if e != nil {
		return "", errors.New("GitHub App authentication failed")
	}
	return body + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func (c *Client) request(ctx context.Context, method, path string, body any, bearer string) (*http.Request, error) {
	var data io.Reader
	if body != nil {
		raw, e := json.Marshal(body)
		if e != nil {
			return nil, e
		}
		data = bytes.NewReader(raw)
	}
	req, e := http.NewRequestWithContext(ctx, method, c.api+path, data)
	if e != nil {
		return nil, e
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer == "jwt" {
		token, e := c.jwt()
		if e != nil {
			return nil, e
		}
		req.Header.Set("Authorization", "Bearer "+token)
	} else if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	return req, nil
}

func (c *Client) do(req *http.Request) (*http.Response, error) {
	res, e := c.HTTP.Do(req)
	if e != nil {
		if res != nil {
			res.Body.Close()
		}
		return nil, errors.New("GitHub request failed")
	}
	return res, nil
}

func (c *Client) call(ctx context.Context, method, path string, body, out any, bearer string) error {
	req, e := c.request(ctx, method, path, body, bearer)
	if e != nil {
		return e
	}
	res, e := c.do(req)
	if e != nil {
		return e
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
		return responseError{res.StatusCode}
	}
	if out != nil {
		if e = json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(out); e != nil {
			return errors.New("GitHub response could not be read")
		}
	}
	return nil
}

func (c *Client) status(ctx context.Context, method, path, bearer string) (int, error) {
	req, e := c.request(ctx, method, path, nil, bearer)
	if e != nil {
		return 0, e
	}
	res, e := c.do(req)
	if e != nil {
		return 0, e
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
	return res.StatusCode, nil
}

type tokenResponse struct {
	Token               string            `json:"token"`
	ExpiresAt           string            `json:"expires_at"`
	Permissions         map[string]string `json:"permissions"`
	RepositorySelection string            `json:"repository_selection"`
	Repositories        []struct {
		FullName string `json:"full_name"`
	} `json:"repositories"`
}

func samePermissions(got, want map[string]string) bool {
	if len(got) != len(want) {
		return false
	}
	for key, value := range want {
		if got[key] != value {
			return false
		}
	}
	return true
}

func acceptableToken(token string) bool {
	return token != "" && len(token) <= 4096 && !strings.ContainsAny(token, " \r\n\x00")
}

// discard revokes a token this process must not keep. A failed revocation
// stays an error so the caller does not treat the token as unused.
func (c *Client) discard(ctx context.Context, token, probe string) error {
	if token == "" {
		return nil
	}
	if e := c.revokeBearer(ctx, token); e != nil {
		return e
	}
	return c.confirmDead(ctx, token, probe)
}

func (c *Client) revokeBearer(ctx context.Context, token string) error {
	code, e := c.status(ctx, http.MethodDelete, "/installation/token", token)
	if e != nil {
		return e
	}
	switch code {
	case http.StatusNoContent, http.StatusUnauthorized, http.StatusNotFound:
		return nil
	default:
		return responseError{code}
	}
}

func (c *Client) confirmDead(ctx context.Context, token, probe string) error {
	code, e := c.status(ctx, http.MethodGet, probe, token)
	if e != nil {
		return e
	}
	if code == http.StatusUnauthorized || code == http.StatusNotFound {
		return nil
	}
	if code == http.StatusOK {
		return errors.New("GitHub credential revocation not confirmed")
	}
	return responseError{code}
}

func (c *Client) mint(ctx context.Context, repo, probe string, permissions map[string]string, oneRepo bool) (string, time.Time, error) {
	body := map[string]any{"permissions": permissions}
	if oneRepo {
		body["repositories"] = []string{strings.Split(repo, "/")[1]}
	}
	var token tokenResponse
	if e := c.call(ctx, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", c.InstallationID), body, &token, "jwt"); e != nil {
		return "", time.Time{}, e
	}
	reject := ""
	switch {
	case !acceptableToken(token.Token):
		reject = "GitHub did not return token material"
	case !samePermissions(token.Permissions, permissions):
		reject = "GitHub token permissions were not the requested set"
	case oneRepo && (token.RepositorySelection != "selected" || len(token.Repositories) != 1 || token.Repositories[0].FullName != repo):
		reject = "GitHub token was not limited to one repository"
	}
	expiry, e := time.Parse(time.RFC3339, token.ExpiresAt)
	if reject == "" && (e != nil || !expiry.After(c.clock().Add(tokenReuseMargin))) {
		reject = "GitHub token expiry is too soon"
	}
	if reject != "" {
		if e := c.discard(ctx, token.Token, probe); e != nil {
			return "", time.Time{}, e
		}
		return "", time.Time{}, errors.New(reject)
	}
	return token.Token, expiry, nil
}

func (c *Client) controllerToken(ctx context.Context) (string, error) {
	if c.controller.value != "" && c.controller.expiry.After(c.clock().Add(tokenReuseMargin)) {
		return c.controller.value, nil
	}
	if c.controller.value != "" {
		if e := c.discard(ctx, c.controller.value, "/installation/repositories"); e != nil {
			return "", e
		}
		c.controller = tokenRecord{}
	}
	token, expiry, e := c.mint(ctx, "", "/installation/repositories", controllerPermissions, false)
	if e != nil {
		return "", e
	}
	c.controller = tokenRecord{value: token, expiry: expiry}
	return token, nil
}

func (c *Client) controllerCall(ctx context.Context, method, path string, out any) error {
	token, e := c.controllerToken(ctx)
	if e != nil {
		return e
	}
	e = c.call(ctx, method, path, nil, out, token)
	if !statusIs(e, http.StatusUnauthorized) {
		return e
	}
	if rev := c.discard(ctx, token, "/installation/repositories"); rev != nil {
		return rev
	}
	c.controller = tokenRecord{}
	token, e = c.controllerToken(ctx)
	if e != nil {
		return e
	}
	return c.call(ctx, method, path, nil, out, token)
}

func (c *Client) appSlug(ctx context.Context) (string, error) {
	if c.slug != "" {
		return c.slug, nil
	}
	var app struct {
		Slug string `json:"slug"`
	}
	if e := c.call(ctx, http.MethodGet, "/app", nil, &app, "jwt"); e != nil {
		return "", e
	}
	if app.Slug == "" || strings.ContainsAny(app.Slug, " /\r\n") {
		return "", errors.New("GitHub App slug is missing")
	}
	c.slug = app.Slug
	return c.slug, nil
}

type branchProtection struct {
	EnforceAdmins struct {
		Enabled bool `json:"enabled"`
	} `json:"enforce_admins"`
	RequiredPullRequestReviews *struct {
		RequiredApprovingReviewCount int `json:"required_approving_review_count"`
	} `json:"required_pull_request_reviews"`
	Restrictions *struct {
		Apps []struct {
			Slug string `json:"slug"`
		} `json:"apps"`
	} `json:"restrictions"`
	AllowForcePushes struct {
		Enabled bool `json:"enabled"`
	} `json:"allow_force_pushes"`
	AllowDeletions struct {
		Enabled bool `json:"enabled"`
	} `json:"allow_deletions"`
}

func (c *Client) protect(ctx context.Context, repo string) error {
	var meta struct {
		FullName      string `json:"full_name"`
		DefaultBranch string `json:"default_branch"`
		Archived      bool   `json:"archived"`
		Disabled      bool   `json:"disabled"`
	}
	if e := c.controllerCall(ctx, http.MethodGet, "/repos/"+repo, &meta); e != nil {
		return e
	}
	if meta.FullName != repo || meta.DefaultBranch != "main" || meta.Archived || meta.Disabled {
		return errors.New("repository must be writable with default branch main")
	}
	var protection branchProtection
	e := c.controllerCall(ctx, http.MethodGet, "/repos/"+repo+"/branches/main/protection", &protection)
	if statusIs(e, http.StatusNotFound) {
		return errors.New("main protection requires classic branch protection; rulesets alone are not accepted")
	}
	if e != nil {
		return e
	}
	slug, e := c.appSlug(ctx)
	if e != nil {
		return e
	}
	if !protection.EnforceAdmins.Enabled || protection.RequiredPullRequestReviews == nil || protection.RequiredPullRequestReviews.RequiredApprovingReviewCount < 1 || protection.Restrictions == nil || protection.AllowForcePushes.Enabled || protection.AllowDeletions.Enabled {
		return errors.New("main protection must require review, apply to administrators, restrict pushes, and reject force-push and deletion")
	}
	for _, app := range protection.Restrictions.Apps {
		if app.Slug == slug {
			return errors.New("main protection must not allow this GitHub App to push")
		}
	}
	return nil
}

func (c *Client) reusable(ctx context.Context, repo string, rec tokenRecord) bool {
	if !acceptableToken(rec.value) || !rec.expiry.After(c.clock().Add(tokenReuseMargin)) {
		return false
	}
	code, e := c.status(ctx, http.MethodGet, "/repos/"+repo, rec.value)
	return e == nil && code == http.StatusOK
}

func (c *Client) Discover(ctx context.Context) ([]forgejo.Repository, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var all []forgejo.Repository
	for page := 1; page <= 100; page++ {
		var body struct {
			Repositories []forgejo.Repository `json:"repositories"`
		}
		if e := c.controllerCall(ctx, http.MethodGet, fmt.Sprintf("/installation/repositories?per_page=100&page=%d", page), &body); e != nil {
			return nil, e
		}
		for _, repo := range body.Repositories {
			if forgejo.ValidateRepository(repo.FullName, c.Owner) == nil {
				all = append(all, forgejo.Repository{FullName: repo.FullName, Private: repo.Private})
			}
		}
		if len(body.Repositories) < 100 {
			return all, nil
		}
	}
	return nil, errors.New("repository catalog exceeds pagination limit")
}

func (c *Client) Grant(ctx context.Context, repo string, cred forgejo.Credentials) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := forgejo.ValidateRepository(repo, c.Owner); e != nil {
		return "", e
	}
	if e := c.protect(ctx, repo); e != nil {
		return "", e
	}
	// A failed ledger write must not mint a second token on the next retry.
	// The map dies with the process. A token that was never saved cannot be
	// recovered, because GitHub has no installation-token inventory.
	if rec, ok := c.byRepo[repo]; ok && c.reusable(ctx, repo, rec) {
		if cred.Token != "" && cred.Token != rec.value {
			if e := c.discard(ctx, cred.Token, "/repos/"+repo); e != nil {
				return "", e
			}
		}
		return rec.value, nil
	}
	revoked := ""
	if rec, ok := c.byRepo[repo]; ok && rec.value != "" {
		if e := c.discard(ctx, rec.value, "/repos/"+repo); e != nil {
			return "", e
		}
		revoked = rec.value
		delete(c.byRepo, repo)
	}
	if cred.Token != "" && cred.Token != revoked {
		// No recorded expiry means this process restarted. Rotate the stored token.
		if e := c.discard(ctx, cred.Token, "/repos/"+repo); e != nil {
			return "", e
		}
	}
	token, expiry, e := c.mint(ctx, repo, "/repos/"+repo, workspacePermissions, true)
	if e != nil {
		return "", e
	}
	c.byRepo[repo] = tokenRecord{value: token, expiry: expiry}
	return token, nil
}

func (c *Client) Revoke(ctx context.Context, repo string, cred forgejo.Credentials) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := forgejo.ValidateRepository(repo, c.Owner); e != nil {
		return e
	}
	targets := []string{}
	if acceptableToken(cred.Token) {
		targets = append(targets, cred.Token)
	} else if cred.Token != "" {
		return errors.New("GitHub credential revocation not confirmed")
	}
	if rec, ok := c.byRepo[repo]; ok && rec.value != "" && rec.value != cred.Token {
		targets = append(targets, rec.value)
	}
	// An empty ledger after a crash cannot name the token GitHub still holds.
	if len(targets) == 0 {
		return nil
	}
	for _, token := range targets {
		if e := c.discard(ctx, token, "/repos/"+repo); e != nil {
			return e
		}
		if c.controller.value == token {
			c.controller = tokenRecord{}
		}
	}
	delete(c.byRepo, repo)
	return nil
}
