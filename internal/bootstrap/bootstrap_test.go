package bootstrap

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitOutput(t *testing.T, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	out, e := c.CombinedOutput()
	if e != nil {
		t.Fatalf("fixture Git failed: %v: %s", e, out)
	}
	return strings.TrimSpace(string(out))
}

func repositoryFixture(t *testing.T) (workspace, roots, trust, origin, seed string) {
	t.Helper()
	if _, e := exec.LookPath("git"); e != nil {
		t.Fatal("Git is required for bootstrap tests")
	}
	dir := t.TempDir()
	workspace, roots, trust, origin, seed = filepath.Join(dir, "workspace"), filepath.Join(dir, "roots.crt"), filepath.Join(dir, "trust.crt"), filepath.Join(dir, "remote.git"), filepath.Join(dir, "seed")
	if e := os.MkdirAll(filepath.Join(workspace, "home"), 0700); e != nil {
		t.Fatal(e)
	}
	t.Setenv("HOME", filepath.Join(workspace, "home"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(workspace, "home", ".gitconfig"))
	t.Setenv("GIT_CONFIG_COUNT", "0")
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
	for path, contents := range map[string]string{roots: "fixture root\n", trust: "fixture trust\n"} {
		if e := os.WriteFile(path, []byte(contents), 0600); e != nil {
			t.Fatal(e)
		}
	}
	gitOutput(t, "init", "--bare", "--initial-branch=main", origin)
	gitOutput(t, "init", "--initial-branch=main", seed)
	gitOutput(t, "-C", seed, "config", "user.name", "Fixture")
	gitOutput(t, "-C", seed, "config", "user.email", "fixture@example.invalid")
	if e := os.WriteFile(filepath.Join(seed, "README"), []byte("initial\n"), 0600); e != nil {
		t.Fatal(e)
	}
	gitOutput(t, "-C", seed, "add", "README")
	gitOutput(t, "-C", seed, "commit", "-m", "Initial fixture")
	gitOutput(t, "-C", seed, "remote", "add", "origin", origin)
	gitOutput(t, "-C", seed, "push", "origin", "main")
	return
}

func TestCloneAndResumePreserveLocalWork(t *testing.T) {
	workspace, roots, trust, origin, seed := repositoryFixture(t)
	ctx := context.Background()
	if e := run(ctx, workspace, roots, trust, origin, "agent/fixture"); e != nil {
		t.Fatal(e)
	}
	repo := filepath.Join(workspace, "repo")
	head := gitOutput(t, "-C", repo, "rev-parse", "HEAD")
	if got := gitOutput(t, "-C", repo, "branch", "--show-current"); got != "agent/fixture" {
		t.Fatal("clone did not create work branch")
	}
	if e := os.WriteFile(filepath.Join(repo, "README"), []byte("local edits\n"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(repo, "untracked"), []byte("local file\n"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(seed, "upstream"), []byte("remote change\n"), 0600); e != nil {
		t.Fatal(e)
	}
	gitOutput(t, "-C", seed, "add", "upstream")
	gitOutput(t, "-C", seed, "commit", "-m", "Remote update")
	gitOutput(t, "-C", seed, "push", "origin", "main")
	upstream := gitOutput(t, "-C", seed, "rev-parse", "HEAD")
	if e := run(ctx, workspace, roots, trust, origin, "agent/fixture"); e != nil {
		t.Fatal(e)
	}
	if gitOutput(t, "-C", repo, "rev-parse", "HEAD") != head || gitOutput(t, "-C", repo, "branch", "--show-current") != "agent/fixture" {
		t.Fatal("resume changed local branch or HEAD")
	}
	if gitOutput(t, "-C", repo, "rev-parse", "origin/main") != upstream {
		t.Fatal("resume did not fetch upstream changes")
	}
	for file, want := range map[string]string{"README": "local edits\n", "untracked": "local file\n"} {
		b, e := os.ReadFile(filepath.Join(repo, file))
		if e != nil || string(b) != want {
			t.Fatal("resume lost local work")
		}
	}
	if e := run(ctx, workspace, roots, trust, origin+".missing", "agent/fixture"); e == nil {
		t.Fatal("missing remote accepted")
	}
	if _, e := os.Stat(filepath.Join(workspace, ".ready")); !os.IsNotExist(e) {
		t.Fatal("failed resume retained stale readiness marker")
	}
	if e := run(ctx, workspace, roots, trust, origin, "agent/fixture"); e != nil {
		t.Fatal("resume did not recover", e)
	}
}

func TestInterruptedCloneRetriesAtomically(t *testing.T) {
	workspace, roots, trust, origin, _ := repositoryFixture(t)
	ctx := context.Background()
	if e := run(ctx, workspace, roots, trust, origin, "../invalid"); e == nil {
		t.Fatal("invalid branch accepted")
	}
	for _, name := range []string{"repo", ".ready"} {
		if _, e := os.Stat(filepath.Join(workspace, name)); !os.IsNotExist(e) {
			t.Fatal("failed preparation exposed ready project")
		}
	}
	stage := filepath.Join(workspace, ".clone")
	if e := os.WriteFile(filepath.Join(stage, "partial"), []byte("leftover"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := run(ctx, workspace, roots, trust, origin, "agent/retry"); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(filepath.Join(workspace, "repo", "partial")); !os.IsNotExist(e) {
		t.Fatal("retry adopted interrupted clone")
	}
	if _, e := os.Stat(filepath.Join(workspace, ".ready")); e != nil {
		t.Fatal("retry not ready", e)
	}
}

func TestCredentialScopeAndMalformedRequests(t *testing.T) {
	dir := t.TempDir()
	for file, value := range map[string]string{"username": "fixture", "token": "fixture-token"} {
		if e := os.WriteFile(filepath.Join(dir, file), []byte(value), 0600); e != nil {
			t.Fatal(e)
		}
	}
	valid := "protocol=https\nhost=git.example.com\npath=owner/repo.git\n\n"
	var out bytes.Buffer
	if e := credential(strings.NewReader(valid), &out, dir, "git.example.com", "owner/repo"); e != nil {
		t.Fatal(e)
	}
	if out.String() != "username=fixture\npassword=fixture-token\n\n" {
		t.Fatal("valid request not answered")
	}
	for _, input := range []string{strings.Replace(valid, "https", "http", 1), strings.Replace(valid, "git.example.com", "other.example.com", 1), strings.Replace(valid, "owner/repo.git", "owner/other.git", 1), strings.Replace(valid, "owner/repo.git", "owner/repo.git/extra", 1)} {
		out.Reset()
		if e := credential(strings.NewReader(input), &out, filepath.Join(dir, "missing"), "git.example.com", "owner/repo"); e != nil || out.Len() != 0 {
			t.Fatal("out-of-scope request accessed credentials")
		}
	}
	for _, input := range []string{"protocol=https\nprotocol=http\n\n", "protocol=https\r\n\n", strings.Repeat("x", (16<<10)+1)} {
		out.Reset()
		if e := credential(strings.NewReader(input), &out, dir, "git.example.com", "owner/repo"); e == nil || out.Len() != 0 {
			t.Fatal("malformed request accepted")
		}
	}
	if e := os.WriteFile(filepath.Join(dir, "token"), []byte("injected\nfield=value"), 0600); e != nil {
		t.Fatal(e)
	}
	out.Reset()
	if e := credential(strings.NewReader(valid), &out, dir, "git.example.com", "owner/repo"); e == nil || out.Len() != 0 {
		t.Fatal("multiline credential accepted")
	}
}
