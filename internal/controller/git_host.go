package controller

import (
	"errors"

	api "git.cluster.aimingupward.com/steve/agent-project-controller/api/v1alpha1"
	"git.cluster.aimingupward.com/steve/agent-project-controller/internal/proxy"
)

// AccessRules returns the inspecting-proxy allow for the selected git host.
// An empty host keeps the Forgejo profile used by existing installs.
// GitHub returns no private hosts: github.com and api.github.com must resolve
// to public addresses.
func AccessRules(host, origin, repo string) ([]api.HTTPSRule, []string, error) {
	switch host {
	case "", "forgejo":
		rule, err := proxy.ForgejoRule(origin, repo)
		if err != nil {
			return nil, nil, err
		}
		return []api.HTTPSRule{rule}, []string{rule.Host}, nil
	case "github":
		rules, err := proxy.GitHubRules(repo)
		if err != nil {
			return nil, nil, err
		}
		return rules, nil, nil
	default:
		return nil, nil, errors.New("git host must be forgejo or github")
	}
}

func cloneOrigin(host, configured string) string {
	if host == "github" {
		return "https://github.com"
	}
	return configured
}
