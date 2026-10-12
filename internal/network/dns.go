package network

import (
	"context"
	"errors"
	"net/netip"
	"sort"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// DNSResolver validates the configured CoreDNS service and ready pod endpoints.
type DNSResolver struct {
	ClusterDomain, DNSNamespace, DNSService string
	DirectDNS                               bool
}

func (c DNSResolver) defaults() DNSResolver {
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
func (c DNSResolver) DNS(ctx context.Context, k client.Client, namespace string) (*corev1.PodDNSConfig, error) {
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
	nameservers := []string{address.String()}
	if c.DirectDNS {
		var slices discoveryv1.EndpointSliceList
		if e := k.List(ctx, &slices, client.InNamespace(c.DNSNamespace), client.MatchingLabels{discoveryv1.LabelServiceName: c.DNSService}); e != nil {
			return nil, errors.New("CoreDNS endpoints unavailable")
		}
		addresses := map[string]bool{}
		for _, slice := range slices.Items {
			tcp, udp := false, false
			for _, p := range slice.Ports {
				if p.Port != nil && *p.Port == 53 && p.Protocol != nil {
					tcp = tcp || *p.Protocol == corev1.ProtocolTCP
					udp = udp || *p.Protocol == corev1.ProtocolUDP
				}
			}
			if !tcp || !udp {
				continue
			}
			for _, endpoint := range slice.Endpoints {
				if endpoint.Conditions.Ready == nil || !*endpoint.Conditions.Ready || endpoint.TargetRef == nil || endpoint.TargetRef.Kind != "Pod" || endpoint.TargetRef.Namespace != c.DNSNamespace {
					continue
				}
				pod := &corev1.Pod{}
				if e := k.Get(ctx, client.ObjectKey{Namespace: c.DNSNamespace, Name: endpoint.TargetRef.Name}, pod); e != nil || pod.Labels["k8s-app"] != "kube-dns" {
					continue
				}
				for _, raw := range endpoint.Addresses {
					ip, e := netip.ParseAddr(raw)
					if e != nil || !ip.IsGlobalUnicast() {
						continue
					}
					for _, actual := range pod.Status.PodIPs {
						if actual.IP == raw {
							addresses[raw] = true
						}
					}
				}
			}
		}
		nameservers = nil
		for ip := range addresses {
			nameservers = append(nameservers, ip)
		}
		sort.Strings(nameservers)
		if len(nameservers) == 0 {
			return nil, errors.New("no ready inspected CoreDNS pod endpoints")
		}
		if len(nameservers) > 3 {
			nameservers = nameservers[:3]
		}
	}
	ndots := "1"
	return &corev1.PodDNSConfig{Nameservers: nameservers, Searches: []string{namespace + ".svc." + c.ClusterDomain, "svc." + c.ClusterDomain, c.ClusterDomain}, Options: []corev1.PodDNSConfigOption{{Name: "ndots", Value: &ndots}}}, nil
}
