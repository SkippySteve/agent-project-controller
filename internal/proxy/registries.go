package proxy

import (
	"errors"

	api "git.cluster.aimingupward.com/steve/agent-project-controller/api/v1alpha1"
)

// RegistryRules returns the reviewed host allowlist for a package ecosystem.
// Paths are the whole host. This is not per-package version enforcement.
func RegistryRules(ids []string) ([]api.HTTPSRule, error) {
	catalog := map[string][]string{
		"npm":    {"registry.npmjs.org"},
		"go":     {"proxy.golang.org", "sum.golang.org"},
		"python": {"pypi.org", "files.pythonhosted.org"},
		"rust":   {"index.crates.io", "static.crates.io"},
	}
	if len(ids) > len(catalog) {
		return nil, errors.New("unknown registry")
	}
	seen := map[string]bool{}
	rules := []api.HTTPSRule{}
	for _, id := range ids {
		hosts, ok := catalog[id]
		if !ok || seen[id] {
			return nil, errors.New("unknown or repeated registry")
		}
		seen[id] = true
		for _, host := range hosts {
			rules = append(rules, api.HTTPSRule{Host: host, Methods: []string{"GET", "HEAD"}, Paths: []string{"/"}})
		}
	}
	return rules, nil
}
