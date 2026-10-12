package forgejo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExistingMainProtectionExplainsMissingApprovalWithoutChangingRule(t *testing.T) {
	mutations := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			mutations++
			t.Error("existing protections must not be rewritten")
		}
		if strings.HasSuffix(r.URL.Path, "/branch_protections") {
			_ = json.NewEncoder(w).Encode([]protection{{RuleName: "main", ApplyToAdmins: true, EnableMergeWhitelist: true, MergeWhitelistUsernames: []string{"steve"}, RequiredApprovals: 0}})
		} else {
			_ = json.NewEncoder(w).Encode(map[string]any{"default_branch": "main"})
		}
	}))
	defer server.Close()
	c := &Client{Origin: server.URL, Owner: "steve", AdminToken: "fixture", HTTP: server.Client()}
	err := c.protect(context.Background(), "steve/repo")
	if err == nil || !strings.Contains(err.Error(), "require at least 1 approval (currently 0)") || !strings.Contains(err.Error(), "Settings > Branches") || mutations != 0 {
		t.Fatal(err, mutations)
	}
}

func TestNewRejectsGitHubOrigin(t *testing.T) {
	for _, origin := range []string{"https://github.com", "https://GitHub.com", "https://api.github.com"} {
		_, err := New(origin, "owner", "token")
		if err == nil || !strings.Contains(err.Error(), "GIT_HOST=github") {
			t.Fatal(origin, err)
		}
	}
	if _, err := New("https://github.com.evil.example", "owner", "token"); err != nil {
		t.Fatal(err)
	}
	if _, err := New("https://git.example.com", "owner", "token"); err != nil {
		t.Fatal(err)
	}
}
