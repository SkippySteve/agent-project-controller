package network

import (
	"context"
	"errors"
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
	// ServiceProxy pins the worker proxy hostname to an allocated ClusterIP.
	ServiceProxy bool
}
type Provider interface {
	Capabilities() Capabilities
	Objects(namespace, uid string, hosts []string) []*unstructured.Unstructured
	Check(context.Context, client.Client) error
	DNS(context.Context, client.Client, string) (*corev1.PodDNSConfig, error)
}

// Gateway is an operator-configured TLS gateway reached after Kubernetes service translation.
// Its permissions apply only to the inspecting proxy, never to workers.
type Gateway struct {
	Namespace        string            `json:"namespace"`
	Selector         map[string]string `json:"selector"`
	Port             int               `json:"port"`
	DisableSNIFilter bool              `json:"disableSNIFilter,omitempty"`
}
type Cilium struct {
	ClusterDomain, DNSNamespace, DNSService string
	Gateway                                 *Gateway
	DirectDNS                               bool
}

func (c Cilium) Validate() error {
	if c.Gateway == nil {
		return nil
	}
	g := c.Gateway
	if len(validation.IsDNS1123Label(g.Namespace)) != 0 || g.Port < 1 || g.Port > 65535 || len(g.Selector) == 0 {
		return errors.New("gateway requires an explicit namespace, pod selector and TLS port")
	}
	for key, value := range g.Selector {
		if len(validation.IsQualifiedName(key)) != 0 || len(validation.IsValidLabelValue(value)) != 0 || value == "" || key == "io.kubernetes.pod.namespace" {
			return errors.New("invalid gateway pod selector")
		}
	}
	return nil
}

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

func (c Cilium) DNS(ctx context.Context, k client.Client, namespace string) (*corev1.PodDNSConfig, error) {
	return (DNSResolver{ClusterDomain: c.ClusterDomain, DNSNamespace: c.DNSNamespace, DNSService: c.DNSService, DirectDNS: c.DirectDNS}).DNS(ctx, k, namespace)
}

func (Cilium) Capabilities() Capabilities { return Capabilities{BypassPrevention: true} }
func (p Cilium) Check(ctx context.Context, c client.Client) error {
	if e := p.Validate(); e != nil {
		return e
	}
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
	if c.Gateway != nil {
		labels := map[string]any{"k8s:io.kubernetes.pod.namespace": c.Gateway.Namespace}
		for k, v := range c.Gateway.Selector {
			labels["k8s:"+k] = v
		}
		serverNames := []any{}
		for _, host := range hosts {
			serverNames = append(serverNames, host)
		}
		spec := proxy.Object["spec"].(map[string]any)
		ports := map[string]any{"ports": []any{port(c.Gateway.Port)}}
		if !c.Gateway.DisableSNIFilter {
			ports["serverNames"] = serverNames
		}
		spec["egress"] = append(spec["egress"].([]any), map[string]any{
			"toEndpoints": []any{map[string]any{"matchLabels": labels}},
			"toPorts":     []any{ports},
		})
	}
	return []*unstructured.Unstructured{worker, proxy}
}
