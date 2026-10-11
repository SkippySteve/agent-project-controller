package network

import (
	"context"
	"errors"
	"net/netip"
	"strconv"

	api "git.cluster.aimingupward.com/steve/agent-project-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Capabilities struct {
	BypassPrevention bool
	HTTPSInspection  bool
}
type Provider interface {
	Capabilities() Capabilities
	Objects(namespace, uid string, hosts []string) []*unstructured.Unstructured
	Check(context.Context, client.Client) error
	DNS(context.Context, client.Client, string) (*corev1.PodDNSConfig, error)
}
type Cilium struct{ ClusterDomain, DNSNamespace, DNSService string }

func (c Cilium) defaults() Cilium {
	if c.ClusterDomain == "" {
		c.ClusterDomain = "cluster.local"
	}
	if c.DNSNamespace == "" {
		c.DNSNamespace = "kube-system"
	}
	if c.DNSService == "" {
		c.DNSService = "kube-dns"
	}
	return c
}

// DNS deliberately avoids inheriting a node-local resolver that the endpoint policy cannot restrict.
// Service routing must still be qualified against the installed Cilium and node-local DNS configuration.
func (c Cilium) DNS(ctx context.Context, k client.Client, namespace string) (*corev1.PodDNSConfig, error) {
	c = c.defaults()
	if len(validation.IsDNS1123Subdomain(c.ClusterDomain)) != 0 || len(validation.IsDNS1123Label(c.DNSNamespace)) != 0 || len(validation.IsDNS1035Label(c.DNSService)) != 0 {
		return nil, errors.New("invalid cluster domain or CoreDNS service configuration")
	}
	service := &corev1.Service{}
	if e := k.Get(ctx, client.ObjectKey{Namespace: c.DNSNamespace, Name: c.DNSService}, service); e != nil {
		return nil, errors.New("configured CoreDNS service unavailable")
	}
	address, e := netip.ParseAddr(service.Spec.ClusterIP)
	if e != nil || !address.IsGlobalUnicast() || service.Spec.Type != corev1.ServiceTypeClusterIP || service.Spec.Selector["k8s-app"] != "kube-dns" {
		return nil, errors.New("CoreDNS requires a ClusterIP service selecting k8s-app=kube-dns pods")
	}
	tcp, udp := false, false
	for _, port := range service.Spec.Ports {
		if port.Port == 53 {
			tcp = tcp || port.Protocol == corev1.ProtocolTCP
			udp = udp || port.Protocol == corev1.ProtocolUDP
		}
	}
	if !tcp || !udp {
		return nil, errors.New("CoreDNS service requires TCP and UDP port 53")
	}
	ndots := "1"
	return &corev1.PodDNSConfig{Nameservers: []string{address.String()}, Searches: []string{namespace + ".svc." + c.ClusterDomain, "svc." + c.ClusterDomain, c.ClusterDomain}, Options: []corev1.PodDNSConfigOption{{Name: "ndots", Value: &ndots}}}, nil
}

func (Cilium) Capabilities() Capabilities { return Capabilities{BypassPrevention: true} }
func (p Cilium) Check(ctx context.Context, c client.Client) error {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"})
	if e := c.Get(ctx, client.ObjectKey{Name: "ciliumnetworkpolicies.cilium.io"}, u); e != nil {
		return errors.New("CiliumNetworkPolicy CRD is required")
	}
	if _, e := p.DNS(ctx, c, "qualification"); e != nil {
		return e
	}
	return nil // CRD presence is a prerequisite, not proof of dataplane enforcement. Qualification is separate.
}
func (c Cilium) Objects(ns, uid string, hosts []string) []*unstructured.Unstructured {
	c = c.defaults()
	domain := c.ClusterDomain
	port := func(p int) map[string]any { return map[string]any{"port": strconv.Itoa(p), "protocol": "TCP"} }
	dns := func(names []string) map[string]any {
		rules := []any{}
		for _, name := range names {
			rules = append(rules, map[string]any{"matchName": name})
		}
		return map[string]any{"toEndpoints": []any{map[string]any{"matchLabels": map[string]any{"k8s:io.kubernetes.pod.namespace": c.DNSNamespace, "k8s:k8s-app": "kube-dns"}}}, "toPorts": []any{map[string]any{"ports": []any{map[string]any{"port": "53", "protocol": "ANY"}}, "rules": map[string]any{"dns": rules}}}}
	}
	workerDNS := []string{"proxy", "proxy." + ns, "proxy." + ns + ".svc", "proxy." + ns + ".svc." + domain}
	proxyDNS := []string{}
	for _, host := range hosts {
		proxyDNS = append(proxyDNS, host, host+"."+ns+".svc."+domain, host+".svc."+domain, host+"."+domain)
	}
	base := func(name, role string, spec map[string]any) *unstructured.Unstructured {
		spec["endpointSelector"] = map[string]any{"matchLabels": map[string]any{"projects.agentwork.dev/role": role, api.OwnerLabel: uid}}
		return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "cilium.io/v2", "kind": "CiliumNetworkPolicy", "metadata": map[string]any{"name": name, "namespace": ns, "labels": map[string]any{api.OwnerLabel: uid}}, "spec": spec}}
	}
	worker := base("worker", "worker", map[string]any{"ingress": []any{}, "egress": []any{dns(workerDNS), map[string]any{"toEndpoints": []any{map[string]any{"matchLabels": map[string]any{"k8s:io.kubernetes.pod.namespace": ns, "projects.agentwork.dev/role": "proxy", api.OwnerLabel: uid}}}, "toPorts": []any{map[string]any{"ports": []any{port(3128)}}}}}})
	names := []any{}
	for _, h := range hosts {
		names = append(names, map[string]any{"matchName": h})
	}
	proxy := base("proxy", "proxy", map[string]any{"ingress": []any{map[string]any{"fromEndpoints": []any{map[string]any{"matchLabels": map[string]any{"k8s:io.kubernetes.pod.namespace": ns, "projects.agentwork.dev/role": "worker", api.OwnerLabel: uid}}}, "toPorts": []any{map[string]any{"ports": []any{port(3128)}}}}}, "egress": []any{dns(proxyDNS), map[string]any{"toFQDNs": names, "toPorts": []any{map[string]any{"ports": []any{port(443)}}}}}})
	return []*unstructured.Unstructured{worker, proxy}
}
