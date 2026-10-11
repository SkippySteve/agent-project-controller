package network

import (
	"context"
	"errors"
	"strconv"

	api "git.cluster.aimingupward.com/steve/agent-project-controller/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
}
type Cilium struct{ ClusterDomain string }

func (Cilium) Capabilities() Capabilities { return Capabilities{BypassPrevention: true} }
func (Cilium) Check(ctx context.Context, c client.Client) error {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"})
	if e := c.Get(ctx, client.ObjectKey{Name: "ciliumnetworkpolicies.cilium.io"}, u); e != nil {
		return errors.New("CiliumNetworkPolicy CRD is required")
	}
	return nil // CRD presence is a prerequisite, not proof of dataplane enforcement. Qualification is separate.
}
func (c Cilium) Objects(ns, uid string, hosts []string) []*unstructured.Unstructured {
	domain := c.ClusterDomain
	if domain == "" {
		domain = "cluster.local"
	}
	port := func(p int) map[string]any { return map[string]any{"port": strconv.Itoa(p), "protocol": "TCP"} }
	dns := func(names []string) map[string]any {
		rules := []any{}
		for _, name := range names {
			rules = append(rules, map[string]any{"matchName": name})
		}
		return map[string]any{"toEndpoints": []any{map[string]any{"matchLabels": map[string]any{"k8s:io.kubernetes.pod.namespace": "kube-system", "k8s:k8s-app": "kube-dns"}}}, "toPorts": []any{map[string]any{"ports": []any{map[string]any{"port": "53", "protocol": "ANY"}}, "rules": map[string]any{"dns": rules}}}}
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
