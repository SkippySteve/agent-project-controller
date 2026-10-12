package network

import (
	"context"
	"errors"
	"net/netip"
	"sort"

	api "git.cluster.aimingupward.com/steve/agent-project-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// NetworkPolicy is an experimental L3/L4 provider. Workers have no DNS access;
// the controller pins their proxy hostname to a verified ClusterIP. HTTPS host,
// method and path authorization remain inside the trusted inspecting proxy.
type NetworkPolicy struct {
	Resolver                DNSResolver
	Gateway                 *Gateway
	ClusterCIDRs            []string
	EnforcementAcknowledged bool
}

var nonPublicCIDRs = []string{
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
	"172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.168.0.0/16",
	"198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
	"::/96", "::ffff:0:0/96", "100::/64", "2001:db8::/32", "fc00::/7", "fe80::/10", "ff00::/8",
}

func (p NetworkPolicy) Validate() error {
	if !p.EnforcementAcknowledged {
		return errors.New("NetworkPolicy requires explicit acknowledgement of independently tested CNI enforcement")
	}
	if len(p.ClusterCIDRs) == 0 {
		return errors.New("NetworkPolicy requires explicit cluster pod, service and node CIDRs")
	}
	for _, raw := range p.ClusterCIDRs {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil || prefix.Bits() == 0 || prefix != prefix.Masked() || prefix.Addr().Is4In6() {
			return errors.New("invalid NetworkPolicy cluster CIDR")
		}
	}
	if err := (Cilium{Gateway: p.Gateway}).Validate(); err != nil {
		return err
	}
	if p.Gateway != nil && !p.Gateway.DisableSNIFilter {
		return errors.New("NetworkPolicy cannot enforce gateway SNI; explicitly set disableSNIFilter=true to rely on HTTPS inspection")
	}
	return nil
}
func (p NetworkPolicy) Capabilities() Capabilities {
	return Capabilities{BypassPrevention: true, ServiceProxy: true}
}
func (p NetworkPolicy) DNS(ctx context.Context, c client.Client, ns string) (*corev1.PodDNSConfig, error) {
	return p.Resolver.DNS(ctx, c, ns)
}
func (p NetworkPolicy) excluded() []netip.Prefix {
	prefixes := []netip.Prefix{}
	for _, raw := range append(append([]string{}, nonPublicCIDRs...), p.ClusterCIDRs...) {
		prefixes = append(prefixes, netip.MustParsePrefix(raw))
	}
	return prefixes
}
func prefixInside(parent, child netip.Prefix) bool {
	if parent.Addr().BitLen() != child.Addr().BitLen() || parent.Addr().Is4In6() || child.Addr().Is4In6() {
		return false
	}
	child = child.Masked()
	if child.Bits() <= parent.Bits() || !parent.Contains(child.Addr()) {
		return false
	}
	return parent.Contains(lastPrefixAddr(child))
}
func lastPrefixAddr(prefix netip.Prefix) netip.Addr {
	raw := prefix.Addr().AsSlice()
	hostBits := len(raw)*8 - prefix.Bits()
	for i := len(raw) - 1; hostBits > 0; i-- {
		bits := hostBits
		if bits > 8 {
			bits = 8
		}
		raw[i] |= 0xff >> (8 - bits)
		hostBits -= bits
	}
	addr, ok := netip.AddrFromSlice(raw)
	if !ok {
		panic("fixed prefix width")
	}
	return addr
}
func (p NetworkPolicy) covers(prefix netip.Prefix) bool {
	for _, excluded := range p.excluded() {
		if excluded.Addr().Is4() == prefix.Addr().Is4() && excluded.Bits() <= prefix.Bits() && excluded.Contains(prefix.Addr()) {
			return true
		}
	}
	return false
}
func (p NetworkPolicy) Check(ctx context.Context, c client.Client) error {
	if err := p.Validate(); err != nil {
		return err
	}
	dns, err := p.DNS(ctx, c, "qualification")
	if err != nil {
		return err
	}
	// Include allocated DNS and API service addresses as well as all known
	// node IPs and pod CIDRs. A supplied CIDR list is still the operator's
	// declaration of the complete cluster service/pod/node networks.
	resolver := p.Resolver.defaults()
	for _, key := range []client.ObjectKey{{Namespace: resolver.DNSNamespace, Name: resolver.DNSService}, {Namespace: "default", Name: "kubernetes"}} {
		var service corev1.Service
		if err := c.Get(ctx, key, &service); err != nil {
			return errors.New("NetworkPolicy cannot verify cluster service exclusions")
		}
		for _, raw := range append(append([]string{}, service.Spec.ClusterIPs...), service.Spec.ClusterIP) {
			if raw == "" {
				continue
			}
			ip, err := netip.ParseAddr(raw)
			if err != nil || !p.covers(netip.PrefixFrom(ip, ip.BitLen())) {
				return errors.New("cluster service address is not excluded from proxy internet access")
			}
		}
	}
	for _, raw := range dns.Nameservers {
		ip, err := netip.ParseAddr(raw)
		if err != nil || !p.covers(netip.PrefixFrom(ip, ip.BitLen())) {
			return errors.New("DNS endpoint is not covered by cluster exclusions")
		}
	}
	var nodes corev1.NodeList
	if err := c.List(ctx, &nodes); err != nil || len(nodes.Items) == 0 {
		return errors.New("NetworkPolicy cannot verify node and pod network exclusions")
	}
	for _, node := range nodes.Items {
		for _, address := range node.Status.Addresses {
			if address.Type != corev1.NodeInternalIP && address.Type != corev1.NodeExternalIP {
				continue
			}
			ip, err := netip.ParseAddr(address.Address)
			if err != nil || !p.covers(netip.PrefixFrom(ip, ip.BitLen())) {
				return errors.New("node address is not excluded from proxy internet access")
			}
		}
		for _, raw := range append(append([]string{}, node.Spec.PodCIDRs...), node.Spec.PodCIDR) {
			if raw == "" {
				continue
			}
			prefix, err := netip.ParsePrefix(raw)
			if err != nil || !p.covers(prefix) {
				return errors.New("node pod CIDR is not excluded from proxy internet access")
			}
		}
	}
	return nil // API objects and acknowledgement do not prove dataplane enforcement.
}
func (p NetworkPolicy) Objects(ns, uid string, _ []string) []*unstructured.Unstructured {
	selector := func(role string) metav1.LabelSelector {
		return metav1.LabelSelector{MatchLabels: map[string]string{api.OwnerLabel: uid, "projects.agentwork.dev/role": role}}
	}
	peer := func(namespace string, labels map[string]string) networkingv1.NetworkPolicyPeer {
		return networkingv1.NetworkPolicyPeer{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": namespace}}, PodSelector: &metav1.LabelSelector{MatchLabels: labels}}
	}
	port := func(value int32, protocol corev1.Protocol) networkingv1.NetworkPolicyPort {
		n := intstr.FromInt32(value)
		return networkingv1.NetworkPolicyPort{Protocol: &protocol, Port: &n}
	}
	workerPeer := peer(ns, selector("worker").MatchLabels)
	proxyPeer := peer(ns, selector("proxy").MatchLabels)
	worker := networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: ns, Labels: map[string]string{api.OwnerLabel: uid}}, Spec: networkingv1.NetworkPolicySpec{PodSelector: selector("worker"), PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress}, Egress: []networkingv1.NetworkPolicyEgressRule{{To: []networkingv1.NetworkPolicyPeer{proxyPeer}, Ports: []networkingv1.NetworkPolicyPort{port(3128, corev1.ProtocolTCP)}}}}}
	resolver := p.Resolver.defaults()
	proxy := networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "proxy", Namespace: ns, Labels: map[string]string{api.OwnerLabel: uid}}, Spec: networkingv1.NetworkPolicySpec{PodSelector: selector("proxy"), PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress}, Ingress: []networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{workerPeer}, Ports: []networkingv1.NetworkPolicyPort{port(3128, corev1.ProtocolTCP)}}}, Egress: []networkingv1.NetworkPolicyEgressRule{{To: []networkingv1.NetworkPolicyPeer{peer(resolver.DNSNamespace, map[string]string{"k8s-app": "kube-dns"})}, Ports: []networkingv1.NetworkPolicyPort{port(53, corev1.ProtocolTCP), port(53, corev1.ProtocolUDP)}}}}}
	// IPv6 uses 2000::/3 instead of ::/0. Kubernetes rejects ::ffff:0:0/96 as an
	// exception of ::/0, and that mapped range would otherwise reach IPv4
	// addresses around the IPv4 exceptions. Unique-local and link-local
	// prefixes sit outside 2000::/3, so they are not expressible exceptions.
	for _, cidr := range []string{"0.0.0.0/0", "2000::/3"} {
		parent := netip.MustParsePrefix(cidr)
		excluded := map[string]bool{}
		for _, prefix := range p.excluded() {
			if prefixInside(parent, prefix) {
				excluded[prefix.String()] = true
			}
		}
		exceptions := []string{}
		for prefix := range excluded {
			exceptions = append(exceptions, prefix)
		}
		sort.Strings(exceptions)
		proxy.Spec.Egress = append(proxy.Spec.Egress, networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: cidr, Except: exceptions}}}, Ports: []networkingv1.NetworkPolicyPort{port(443, corev1.ProtocolTCP)}})
	}
	if p.Gateway != nil {
		proxy.Spec.Egress = append(proxy.Spec.Egress, networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{peer(p.Gateway.Namespace, p.Gateway.Selector)}, Ports: []networkingv1.NetworkPolicyPort{port(int32(p.Gateway.Port), corev1.ProtocolTCP)}})
	}
	result := []*unstructured.Unstructured{}
	for _, policy := range []networkingv1.NetworkPolicy{worker, proxy} {
		policy.TypeMeta = metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"}
		obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&policy)
		if err != nil {
			panic(err)
		} // fixed typed policy shape, no user JSON conversion
		result = append(result, &unstructured.Unstructured{Object: obj})
	}
	return result
}
