package forgejo

import (
	"strings"
	"testing"
)

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
