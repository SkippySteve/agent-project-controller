package network

import (
	"strings"
	"testing"
)

func clearNetworkEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"NETWORK_PROVIDER",
		"NETWORK_POLICY_ENFORCEMENT_ACKNOWLEDGED",
		"NETWORK_POLICY_CLUSTER_CIDRS_JSON",
		"PROXY_GATEWAY_JSON",
		"CLUSTER_DOMAIN",
		"DNS_SERVICE_NAMESPACE",
		"DNS_SERVICE_NAME",
		"DNS_DIRECT_ENDPOINTS",
	} {
		t.Setenv(key, "")
	}
}

func TestFromEnvironmentDefaultsToCilium(t *testing.T) {
	clearNetworkEnv(t)
	provider, err := FromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := provider.(Cilium); !ok {
		t.Fatalf("default provider = %T", provider)
	}
	if provider.Capabilities().ServiceProxy || !provider.Capabilities().BypassPrevention {
		t.Fatal("default capabilities changed")
	}
}

func TestFromEnvironmentRejectsProviderConfiguration(t *testing.T) {
	gateway := `{"namespace":"edge","selector":{"app":"gateway"},"port":8443}`
	for _, test := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{name: "unknown provider", env: map[string]string{"NETWORK_PROVIDER": "calico"}, want: "unsupported network provider"},
		{name: "missing acknowledgement", env: map[string]string{"NETWORK_PROVIDER": "network-policy", "NETWORK_POLICY_CLUSTER_CIDRS_JSON": `["10.0.0.0/8"]`}, want: "explicit acknowledgement"},
		{name: "acknowledgement not exactly true", env: map[string]string{"NETWORK_PROVIDER": "network-policy", "NETWORK_POLICY_ENFORCEMENT_ACKNOWLEDGED": "TRUE", "NETWORK_POLICY_CLUSTER_CIDRS_JSON": `["10.0.0.0/8"]`}, want: "explicit acknowledgement"},
		{name: "missing cidrs", env: map[string]string{"NETWORK_PROVIDER": "network-policy", "NETWORK_POLICY_ENFORCEMENT_ACKNOWLEDGED": "true"}, want: "invalid NetworkPolicy cluster CIDRs"},
		{name: "malformed cidrs", env: map[string]string{"NETWORK_PROVIDER": "network-policy", "NETWORK_POLICY_ENFORCEMENT_ACKNOWLEDGED": "true", "NETWORK_POLICY_CLUSTER_CIDRS_JSON": `{"cidr":"10.0.0.0/8"}`}, want: "invalid NetworkPolicy cluster CIDRs"},
		{name: "trailing cidr json", env: map[string]string{"NETWORK_PROVIDER": "network-policy", "NETWORK_POLICY_ENFORCEMENT_ACKNOWLEDGED": "true", "NETWORK_POLICY_CLUSTER_CIDRS_JSON": `["10.0.0.0/8"]{}`}, want: "invalid NetworkPolicy cluster CIDRs"},
		{name: "default route", env: map[string]string{"NETWORK_PROVIDER": "network-policy", "NETWORK_POLICY_ENFORCEMENT_ACKNOWLEDGED": "true", "NETWORK_POLICY_CLUSTER_CIDRS_JSON": `["0.0.0.0/0"]`}, want: "invalid NetworkPolicy cluster CIDR"},
		{name: "unmasked cidr", env: map[string]string{"NETWORK_PROVIDER": "network-policy", "NETWORK_POLICY_ENFORCEMENT_ACKNOWLEDGED": "true", "NETWORK_POLICY_CLUSTER_CIDRS_JSON": `["10.0.0.1/8"]`}, want: "invalid NetworkPolicy cluster CIDR"},
		{name: "ipv4 mapped cidr", env: map[string]string{"NETWORK_PROVIDER": "network-policy", "NETWORK_POLICY_ENFORCEMENT_ACKNOWLEDGED": "true", "NETWORK_POLICY_CLUSTER_CIDRS_JSON": `["::ffff:10.0.0.0/104"]`}, want: "invalid NetworkPolicy cluster CIDR"},
		{name: "empty cidr list", env: map[string]string{"NETWORK_PROVIDER": "network-policy", "NETWORK_POLICY_ENFORCEMENT_ACKNOWLEDGED": "true", "NETWORK_POLICY_CLUSTER_CIDRS_JSON": `[]`}, want: "explicit cluster"},
		{name: "malformed gateway", env: map[string]string{"PROXY_GATEWAY_JSON": "{"}, want: "invalid proxy gateway configuration"},
		{name: "trailing gateway json", env: map[string]string{"PROXY_GATEWAY_JSON": gateway + "{}"}, want: "invalid proxy gateway configuration"},
		{name: "unknown gateway field", env: map[string]string{"PROXY_GATEWAY_JSON": `{"namespace":"edge","selector":{"app":"gateway"},"port":8443,"extra":true}`}, want: "invalid proxy gateway configuration"},
		{name: "gateway without sni acknowledgement", env: map[string]string{"NETWORK_PROVIDER": "network-policy", "NETWORK_POLICY_ENFORCEMENT_ACKNOWLEDGED": "true", "NETWORK_POLICY_CLUSTER_CIDRS_JSON": `["10.0.0.0/8"]`, "PROXY_GATEWAY_JSON": gateway}, want: "cannot enforce gateway SNI"},
	} {
		t.Run(test.name, func(t *testing.T) {
			clearNetworkEnv(t)
			for key, value := range test.env {
				t.Setenv(key, value)
			}
			_, err := FromEnvironment()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestFromEnvironmentAcceptsExplicitNetworkPolicy(t *testing.T) {
	clearNetworkEnv(t)
	t.Setenv("NETWORK_PROVIDER", "network-policy")
	t.Setenv("NETWORK_POLICY_ENFORCEMENT_ACKNOWLEDGED", "true")
	t.Setenv("NETWORK_POLICY_CLUSTER_CIDRS_JSON", `["10.8.0.0/14","2600:1::/32"]`)
	t.Setenv("PROXY_GATEWAY_JSON", `{"namespace":"edge","selector":{"app":"gateway"},"port":8443,"disableSNIFilter":true}`)
	t.Setenv("CLUSTER_DOMAIN", "cluster.example")
	provider, err := FromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	policy, ok := provider.(NetworkPolicy)
	if !ok {
		t.Fatalf("provider = %T", provider)
	}
	if !policy.EnforcementAcknowledged || policy.Resolver.ClusterDomain != "cluster.example" || policy.Gateway == nil || !policy.Gateway.DisableSNIFilter {
		t.Fatal("provider did not keep explicit settings")
	}
	if !policy.Capabilities().ServiceProxy || !policy.Capabilities().BypassPrevention {
		t.Fatal("native provider capabilities")
	}
}
