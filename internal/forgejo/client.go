package forgejo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,99}$`)

func ValidateRepository(repo, owner string) error {
	p := strings.Split(repo, "/")
	if len(p) != 2 || p[0] != owner || !namePattern.MatchString(p[0]) || !namePattern.MatchString(p[1]) {
		return errors.New("repository must belong to the configured owner")
	}
	return nil
}

type Credentials struct{ Username, Password, Token string }
type Repository struct {
	FullName string `json:"full_name"`
	Private  bool   `json:"private"`
}
type Host interface {
	Discover(context.Context) ([]Repository, error)
	Grant(context.Context, string, Credentials) (string, error)
	Revoke(context.Context, string, Credentials) error
}
type Client struct {
	Origin, Owner, AdminToken string
	HTTP                      *http.Client
}

func New(origin, owner, token string) (*Client, error) {
	u, e := url.Parse(origin)
	if e != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("Forgejo origin must be an HTTPS origin")
	}
	if !namePattern.MatchString(owner) || strings.TrimSpace(token) == "" {
		return nil, errors.New("Forgejo owner and administrative token are required")
	}
	return &Client{Origin: strings.TrimSuffix(origin, "/"), Owner: owner, AdminToken: token, HTTP: &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

type responseError struct{ Status int }

func (e responseError) Error() string { return fmt.Sprintf("Forgejo returned HTTP %d", e.Status) }
func statusIs(e error, s int) bool    { var r responseError; return errors.As(e, &r) && r.Status == s }
func (c *Client) call(ctx context.Context, method, path string, body, out any, cred *Credentials) error {
	var data io.Reader
	if body != nil {
		raw, e := json.Marshal(body)
		if e != nil {
			return e
		}
		data = bytes.NewReader(raw)
	}
	req, e := http.NewRequestWithContext(ctx, method, c.Origin+"/api/v1"+path, data)
	if e != nil {
		return e
	}
	req.Header.Set("Content-Type", "application/json")
	if cred == nil {
		req.Header.Set("Authorization", "token "+c.AdminToken)
	} else {
		req.SetBasicAuth(cred.Username, cred.Password)
	}
	res, e := c.HTTP.Do(req)
	if e != nil {
		return errors.New("Forgejo request failed")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
		return responseError{res.StatusCode}
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(out)
	}
	return nil
}
func (c *Client) Discover(ctx context.Context) ([]Repository, error) {
	var all []Repository
	for page := 1; page <= 1000; page++ {
		var items []Repository
		e := c.call(ctx, "GET", fmt.Sprintf("/users/%s/repos?limit=50&page=%d", c.Owner, page), nil, &items, nil)
		if e != nil {
			return nil, e
		}
		all = append(all, items...)
		if len(items) < 50 {
			return all, nil
		}
	}
	return nil, errors.New("repository catalog exceeds pagination limit")
}

type protection struct {
	RuleName                string   `json:"rule_name"`
	EnablePush              bool     `json:"enable_push"`
	EnableForcePush         bool     `json:"enable_force_push"`
	ApplyToAdmins           bool     `json:"apply_to_admins"`
	EnableMergeWhitelist    bool     `json:"enable_merge_whitelist"`
	MergeWhitelistUsernames []string `json:"merge_whitelist_usernames"`
	MergeWhitelistTeams     []string `json:"merge_whitelist_teams"`
	RequiredApprovals       int      `json:"required_approvals"`
}

func (c *Client) protect(ctx context.Context, repo string) error {
	var r struct {
		DefaultBranch string `json:"default_branch"`
		Archived      bool   `json:"archived"`
		Mirror        bool   `json:"mirror"`
	}
	if e := c.call(ctx, "GET", "/repos/"+repo, nil, &r, nil); e != nil {
		return e
	}
	if r.DefaultBranch != "main" || r.Archived || r.Mirror {
		return errors.New("repository must be writable with default branch main")
	}
	var rules []protection
	if e := c.call(ctx, "GET", "/repos/"+repo+"/branch_protections", nil, &rules, nil); e != nil {
		return e
	}
	var main *protection
	for i := range rules {
		if rules[i].RuleName == "main" {
			main = &rules[i]
		} else if strings.ContainsAny(rules[i].RuleName, "*?[]{}") && !strings.Contains(rules[i].RuleName, "/") {
			return errors.New("potentially overlapping main protection requires operator review")
		}
	}
	if main == nil {
		p := protection{RuleName: "main", ApplyToAdmins: true, EnableMergeWhitelist: true, MergeWhitelistUsernames: []string{c.Owner}, RequiredApprovals: 1}
		if e := c.call(ctx, "POST", "/repos/"+repo+"/branch_protections", p, nil, nil); e != nil {
			return e
		}
		// Read back effective server values before granting write access.
		return c.protect(ctx, repo)
	}
	if main.EnablePush || main.EnableForcePush || !main.ApplyToAdmins || !main.EnableMergeWhitelist || main.RequiredApprovals < 1 || len(main.MergeWhitelistTeams) > 0 || len(main.MergeWhitelistUsernames) != 1 || main.MergeWhitelistUsernames[0] != c.Owner {
		return errors.New("main protection requires no direct pushes and reviewed merges by the configured owner only")
	}
	return nil
}
func (c *Client) ensureUser(ctx context.Context, cred Credentials) error {
	var u struct {
		IsAdmin       bool   `json:"is_admin"`
		Login         string `json:"login"`
		Email         string `json:"email"`
		ProhibitLogin bool   `json:"prohibit_login"`
	}
	e := c.call(ctx, "GET", "/users/"+cred.Username, nil, &u, nil)
	if statusIs(e, 404) {
		return c.call(ctx, "POST", "/admin/users", map[string]any{"username": cred.Username, "password": cred.Password, "email": cred.Username + "@agentwork.invalid", "must_change_password": false, "send_notify": false, "visibility": "private"}, nil, nil)
	}
	if e != nil {
		return e
	}
	if u.IsAdmin || u.Login != cred.Username || u.Email != cred.Username+"@agentwork.invalid" || u.ProhibitLogin {
		return errors.New("workspace identity ownership check failed")
	}
	// Basic auth proves the stored password belongs to this identity.
	return c.call(ctx, "GET", "/users/"+cred.Username+"/tokens", nil, &[]token{}, &cred)
}

type token struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	SHA  string `json:"sha1"`
}

func (c *Client) deleteTokens(ctx context.Context, cred Credentials) error {
	var tokens []token
	if e := c.call(ctx, "GET", "/users/"+cred.Username+"/tokens", nil, &tokens, &cred); e != nil {
		return e
	}
	for _, t := range tokens {
		if e := c.call(ctx, "DELETE", fmt.Sprintf("/users/%s/tokens/%d", cred.Username, t.ID), nil, nil, &cred); e != nil && !statusIs(e, 404) {
			return e
		}
	}
	return nil
}
func (c *Client) Grant(ctx context.Context, repo string, cred Credentials) (string, error) {
	if e := ValidateRepository(repo, c.Owner); e != nil {
		return "", e
	}
	if e := c.protect(ctx, repo); e != nil {
		return "", e
	}
	if e := c.ensureUser(ctx, cred); e != nil {
		return "", e
	}
	if cred.Token != "" { // A stored token is reusable only if the server still accepts it.
		req, _ := http.NewRequestWithContext(ctx, "GET", c.Origin+"/api/v1/repos/"+repo, nil)
		req.Header.Set("Authorization", "token "+cred.Token)
		res, e := c.HTTP.Do(req)
		if e == nil {
			res.Body.Close()
			if res.StatusCode == 200 {
				if e = c.call(ctx, "PUT", "/repos/"+repo+"/collaborators/"+cred.Username, map[string]string{"permission": "write"}, nil, nil); e != nil {
					return "", e
				}
				return cred.Token, nil
			}
		}
	}
	// A crash after token creation cannot recover the token value. Remove orphaned tokens first.
	if e := c.deleteTokens(ctx, cred); e != nil {
		return "", e
	}
	if e := c.call(ctx, "PUT", "/repos/"+repo+"/collaborators/"+cred.Username, map[string]string{"permission": "write"}, nil, nil); e != nil {
		return "", e
	}
	var t token
	parts := strings.Split(repo, "/")
	if e := c.call(ctx, "POST", "/users/"+cred.Username+"/tokens", map[string]any{"name": "project-workspace", "scopes": []string{"write:repository"}, "repositories": []map[string]string{{"owner": parts[0], "name": parts[1]}}}, &t, &cred); e != nil {
		return "", e
	}
	if t.SHA == "" {
		return "", errors.New("Forgejo did not return token material")
	}
	return t.SHA, nil
}
func (c *Client) Revoke(ctx context.Context, repo string, cred Credentials) error {
	if e := ValidateRepository(repo, c.Owner); e != nil {
		return e
	}
	if e := c.call(ctx, "DELETE", "/repos/"+repo+"/collaborators/"+cred.Username, nil, nil, nil); e != nil && !statusIs(e, 404) {
		return e
	}
	var u struct {
		IsAdmin       bool   `json:"is_admin"`
		Email         string `json:"email"`
		ProhibitLogin bool   `json:"prohibit_login"`
	}
	e := c.call(ctx, "GET", "/users/"+cred.Username, nil, &u, nil)
	if statusIs(e, 404) {
		return nil
	}
	if e != nil {
		return e
	}
	if u.IsAdmin || u.Email != cred.Username+"@agentwork.invalid" {
		return errors.New("refusing to revoke an identity not owned by this workspace")
	}
	if !u.ProhibitLogin {
		if e = c.deleteTokens(ctx, cred); e != nil {
			return e
		}
		if e = c.call(ctx, "PATCH", "/admin/users/"+cred.Username, map[string]any{"prohibit_login": true}, nil, nil); e != nil {
			return e
		}
	}
	// Keep a disabled account as an audit tombstone. Deleting users can destroy PR attribution.
	if e = c.call(ctx, "GET", "/users/"+cred.Username, nil, &u, nil); e != nil {
		return e
	}
	if !u.ProhibitLogin {
		return errors.New("Forgejo identity revocation not confirmed")
	}
	return nil
}
