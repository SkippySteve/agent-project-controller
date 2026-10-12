package network

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
)

// FromEnvironment is shared by the controller and preflight so they cannot
// qualify one provider while running another.
func FromEnvironment() (Provider, error) {
	get := func(key, fallback string) string {
		if value := os.Getenv(key); value != "" {
			return value
		}
		return fallback
	}
	resolver := DNSResolver{ClusterDomain: get("CLUSTER_DOMAIN", "cluster.local"), DNSNamespace: get("DNS_SERVICE_NAMESPACE", "kube-system"), DNSService: get("DNS_SERVICE_NAME", "kube-dns"), DirectDNS: os.Getenv("DNS_DIRECT_ENDPOINTS") == "true"}
	var gateway *Gateway
	if raw := os.Getenv("PROXY_GATEWAY_JSON"); raw != "" {
		var value Gateway
		if err := decodeConfiguration(raw, &value); err != nil {
			return nil, errors.New("invalid proxy gateway configuration")
		}
		gateway = &value
	}
	switch get("NETWORK_PROVIDER", "cilium") {
	case "cilium":
		p := Cilium{ClusterDomain: resolver.ClusterDomain, DNSNamespace: resolver.DNSNamespace, DNSService: resolver.DNSService, DirectDNS: resolver.DirectDNS, Gateway: gateway}
		return p, p.Validate()
	case "network-policy":
		p := NetworkPolicy{Resolver: resolver, Gateway: gateway, EnforcementAcknowledged: os.Getenv("NETWORK_POLICY_ENFORCEMENT_ACKNOWLEDGED") == "true"}
		if err := decodeConfiguration(os.Getenv("NETWORK_POLICY_CLUSTER_CIDRS_JSON"), &p.ClusterCIDRs); err != nil {
			return nil, errors.New("invalid NetworkPolicy cluster CIDRs")
		}
		return p, p.Validate()
	default:
		return nil, errors.New("unsupported network provider")
	}
}
func decodeConfiguration(raw string, value any) error {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("trailing configuration")
	}
	return nil
}
