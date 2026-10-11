package proxy

import "testing"

func TestRegistryRules(t *testing.T) {
	rules, e := RegistryRules([]string{"go", "npm", "python", "rust"})
	if e != nil {
		t.Fatal(e)
	}
	want := []string{"proxy.golang.org", "sum.golang.org", "registry.npmjs.org", "pypi.org", "files.pythonhosted.org", "index.crates.io", "static.crates.io"}
	if len(rules) != len(want) {
		t.Fatalf("got %d rules", len(rules))
	}
	for i, host := range want {
		rule := rules[i]
		if rule.Host != host || len(rule.Methods) != 2 || rule.Methods[0] != "GET" || rule.Methods[1] != "HEAD" || len(rule.Paths) != 1 || rule.Paths[0] != "/" {
			t.Fatalf("rule %d = %#v", i, rule)
		}
	}
	if _, e = RegistryRules(nil); e != nil {
		t.Fatal(e)
	}
	for _, ids := range [][]string{{"maven"}, {"go", "go"}, {"go", "npm", "python", "rust", "go"}} {
		if _, e = RegistryRules(ids); e == nil {
			t.Fatalf("accepted %v", ids)
		}
	}
}
