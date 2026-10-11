package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func git(ctx context.Context, args ...string) error {
	c := exec.CommandContext(ctx, "git", args...)
	c.Stdout = io.Discard
	c.Stderr = io.Discard
	if e := c.Run(); e != nil {
		return errors.New("git operation failed; verify repository credentials and HTTPS policy")
	}
	return nil
}
func Credential() error {
	// Git asks for credentials with useHttpPath enabled. Never answer for another repository.
	raw, e := io.ReadAll(io.LimitReader(os.Stdin, 16<<10))
	if e != nil {
		return e
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok {
			values[k] = v
		}
	}
	if values["protocol"] != "https" || values["host"] != os.Getenv("GIT_HOST") || values["path"] != os.Getenv("GIT_REPOSITORY")+".git" {
		return nil
	}
	username, e := os.ReadFile("/identity/username")
	if e != nil {
		return e
	}
	token, e := os.ReadFile("/identity/token")
	if e != nil {
		return e
	}
	_, e = fmt.Fprintf(os.Stdout, "username=%s\npassword=%s\n\n", strings.TrimSpace(string(username)), strings.TrimSpace(string(token)))
	return e
}
func Run(ctx context.Context) error {
	home := "/workspace/home"
	if e := os.MkdirAll(home, 0700); e != nil {
		return e
	}
	roots, e := os.ReadFile("/etc/ssl/certs/ca-certificates.crt")
	if e != nil {
		return e
	}
	ca, e := os.ReadFile("/trust/ca.crt")
	if e != nil {
		return e
	}
	if e = os.WriteFile(filepath.Join(home, "ca-bundle.crt"), append(roots, ca...), 0600); e != nil {
		return e
	}
	if e = git(ctx, "config", "--global", "credential.helper", "!/usr/local/bin/agent-project-controller credential"); e != nil {
		return e
	}
	if e = git(ctx, "config", "--global", "credential.useHttpPath", "true"); e != nil {
		return e
	}
	if e = git(ctx, "config", "--global", "http.sslCAInfo", filepath.Join(home, "ca-bundle.crt")); e != nil {
		return e
	}
	target := "/workspace/repo"
	origin := os.Getenv("GIT_ORIGIN") + "/" + os.Getenv("GIT_REPOSITORY") + ".git"
	if _, e = os.Stat(filepath.Join(target, ".git")); os.IsNotExist(e) {
		// Clone to a staging directory, then rename. Interrupted clones are never treated as prepared projects.
		stage := "/workspace/.clone"
		if e = os.RemoveAll(stage); e != nil {
			return e
		}
		if e = git(ctx, "clone", "--", origin, stage); e != nil {
			return e
		}
		if branch := os.Getenv("GIT_WORK_BRANCH"); branch != "" {
			if e = git(ctx, "-C", stage, "switch", "-c", branch); e != nil {
				return e
			}
		}
		if e = os.Rename(stage, target); e != nil {
			return e
		}
	} else if e != nil {
		return e
	} else {
		if e = git(ctx, "-C", target, "remote", "set-url", "origin", origin); e != nil {
			return e
		}
		if e = git(ctx, "-C", target, "fetch", "origin"); e != nil {
			return e
		}
	}
	return os.WriteFile("/workspace/.ready", []byte("prepared\n"), 0600)
}
