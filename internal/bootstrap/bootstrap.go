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
	// Git invokes helpers for get/store/erase. Secrets are returned only for get.
	if len(os.Args) > 2 && os.Args[2] != "get" {
		return nil
	}
	return credential(os.Stdin, os.Stdout, "/identity", os.Getenv("GIT_HOST"), os.Getenv("GIT_REPOSITORY"))
}
func credential(in io.Reader, out io.Writer, directory, host, repository string) error {
	// Git asks for credentials with useHttpPath enabled. Never answer for another repository.
	raw, e := io.ReadAll(io.LimitReader(in, (16<<10)+1))
	if e != nil {
		return e
	}
	if len(raw) > 16<<10 {
		return errors.New("credential request too large")
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" {
			break
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.ContainsAny(line, "\r\x00") {
			return errors.New("invalid credential request")
		}
		// Git documents array attributes as multi-valued. They do not select credentials.
		if strings.HasSuffix(k, "[]") {
			continue
		}
		if _, exists := values[k]; exists {
			return errors.New("duplicate credential field")
		}
		values[k] = v
	}
	if host == "" || repository == "" || values["protocol"] != "https" || values["host"] != host || values["path"] != repository+".git" {
		return nil
	}
	username, e := os.ReadFile(filepath.Join(directory, "username"))
	if e != nil {
		return e
	}
	token, e := os.ReadFile(filepath.Join(directory, "token"))
	if e != nil {
		return e
	}
	user, password := strings.TrimSpace(string(username)), strings.TrimSpace(string(token))
	if user == "" || password == "" || strings.ContainsAny(user+password, "\r\n\x00") {
		return errors.New("invalid credential material")
	}
	_, e = fmt.Fprintf(out, "username=%s\npassword=%s\n\n", user, password)
	return e
}
func Run(ctx context.Context) error {
	return run(ctx, "/workspace", "/etc/ssl/certs/ca-certificates.crt", "/trust/ca.crt", os.Getenv("GIT_ORIGIN")+"/"+os.Getenv("GIT_REPOSITORY")+".git", os.Getenv("GIT_WORK_BRANCH"))
}
func run(ctx context.Context, workspace, rootsPath, trustPath, origin, branch string) error {
	home := filepath.Join(workspace, "home")
	// A previous readiness marker must not survive a failed resume fetch.
	if e := os.Remove(filepath.Join(workspace, ".ready")); e != nil && !os.IsNotExist(e) {
		return e
	}
	if e := os.MkdirAll(home, 0700); e != nil {
		return e
	}
	roots, e := os.ReadFile(rootsPath)
	if e != nil {
		return e
	}
	ca, e := os.ReadFile(trustPath)
	if e != nil {
		return e
	}
	if e = os.WriteFile(filepath.Join(home, "ca-bundle.crt"), append(roots, ca...), 0600); e != nil {
		return e
	}
	if e = git(ctx, "config", "--global", "--replace-all", "credential.helper", "!/usr/local/bin/agent-project-controller credential"); e != nil {
		return e
	}
	if e = git(ctx, "config", "--global", "credential.useHttpPath", "true"); e != nil {
		return e
	}
	if e = git(ctx, "config", "--global", "http.sslCAInfo", filepath.Join(home, "ca-bundle.crt")); e != nil {
		return e
	}
	target := filepath.Join(workspace, "repo")
	if _, e = os.Stat(filepath.Join(target, ".git")); os.IsNotExist(e) {
		// Clone to a staging directory, then rename. Interrupted clones are never treated as prepared projects.
		stage := filepath.Join(workspace, ".clone")
		if e = os.RemoveAll(stage); e != nil {
			return e
		}
		if e = git(ctx, "clone", "--", origin, stage); e != nil {
			return e
		}
		if branch != "" {
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
	return os.WriteFile(filepath.Join(workspace, ".ready"), []byte("prepared\n"), 0600)
}
