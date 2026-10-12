package controller

import "testing"

func TestGitAccessRules(t *testing.T) {
	rules, private, err := AccessRules("", "https://git.example.com", "owner/repo")
	if err != nil || len(rules) != 1 || rules[0].Host != "git.example.com" || len(private) != 1 || private[0] != "git.example.com" {
		t.Fatalf("forgejo rules: %+v %v %v", rules, private, err)
	}
	rules, private, err = AccessRules("github", "https://git.example.com", "owner/repo")
	if err != nil || private != nil || len(rules) != 2 || rules[0].Host != "github.com" || rules[1].Host != "api.github.com" {
		t.Fatalf("github rules: %+v %v %v", rules, private, err)
	}
	if cloneOrigin("github", "https://git.example.com") != "https://github.com" {
		t.Fatal("github clone origin")
	}
	if cloneOrigin("", "https://git.example.com") != "https://git.example.com" {
		t.Fatal("forgejo clone origin")
	}
	if _, _, err = AccessRules("gitlab", "https://git.example.com", "owner/repo"); err == nil {
		t.Fatal("unknown host accepted")
	}
	if _, _, err = AccessRules("github", "https://github.com", "owner/../repo"); err == nil {
		t.Fatal("github path escape accepted")
	}
}
