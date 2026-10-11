package network

import (
	"context"
	"reflect"
	"testing"

	api "git.cluster.aimingupward.com/steve/agent-project-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func dnsService() *corev1.Service {
	return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "kube-dns", Namespace: "kube-system"}, Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, ClusterIP: "10.96.0.10", Selector: map[string]string{"k8s-app": "kube-dns"}, Ports: []corev1.ServicePort{{Port: 53, Protocol: corev1.ProtocolUDP}, {Port: 53, Protocol: corev1.ProtocolTCP}}}}
}
func TestExplicitCoreDNS(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	service := dnsService()
	service.Name = "project-dns"
	service.Namespace = "dns-system"
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(service).Build()
	p := Cilium{ClusterDomain: "cluster.example", DNSNamespace: "dns-system", DNSService: "project-dns"}
	config, e := p.DNS(context.Background(), c, "project")
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(config.Nameservers, []string{"10.96.0.10"}) || !reflect.DeepEqual(config.Searches, []string{"project.svc.cluster.example", "svc.cluster.example", "cluster.example"}) || config.Options[0].Value == nil || *config.Options[0].Value != "1" {
		t.Fatal("DNS did not select the configured resolver and search domain")
	}
	objects := p.Objects("project", "uid", []string{"git.example.com"})
	for _, obj := range objects {
		egress, _, _ := unstructured.NestedSlice(obj.Object, "spec", "egress")
		dns := egress[0].(map[string]any)
		selector := dns["toEndpoints"].([]any)[0].(map[string]any)["matchLabels"].(map[string]any)
		if selector["k8s:io.kubernetes.pod.namespace"] != "dns-system" || selector["k8s:k8s-app"] != "kube-dns" {
			t.Fatal("DNS resolver and network policy disagree")
		}
		ports := dns["toPorts"].([]any)[0].(map[string]any)
		for _, raw := range ports["rules"].(map[string]any)["dns"].([]any) {
			rule := raw.(map[string]any)
			if _, ok := rule["matchPattern"]; ok {
				t.Fatal("wildcard DNS enabled")
			}
		}
	}
	worker := objects[0]
	selector, _, _ := unstructured.NestedStringMap(worker.Object, "spec", "endpointSelector", "matchLabels")
	if selector[api.OwnerLabel] != "uid" {
		t.Fatal("policy not scoped to workspace")
	}
	egress, _, _ := unstructured.NestedSlice(worker.Object, "spec", "egress")
	if len(egress) != 2 {
		t.Fatal("worker has additional outbound access")
	}
	proxySelector := egress[1].(map[string]any)["toEndpoints"].([]any)[0].(map[string]any)["matchLabels"].(map[string]any)
	if proxySelector[api.OwnerLabel] != "uid" || proxySelector["k8s:io.kubernetes.pod.namespace"] != "project" {
		t.Fatal("worker can reach another project proxy")
	}
}
func TestDNSRejectsUnsupportedResolvers(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*corev1.Service)
	}{
		{"headless", func(s *corev1.Service) { s.Spec.ClusterIP = "None" }},
		{"node-local", func(s *corev1.Service) { s.Spec.ClusterIP = "169.254.25.10" }},
		{"loopback", func(s *corev1.Service) { s.Spec.ClusterIP = "127.0.0.1" }},
		{"external-name", func(s *corev1.Service) { s.Spec.Type = corev1.ServiceTypeExternalName }},
		{"wrong-selector", func(s *corev1.Service) { s.Spec.Selector = map[string]string{"app": "node-local"} }},
		{"missing-tcp", func(s *corev1.Service) { s.Spec.Ports = s.Spec.Ports[:1] }},
	} {
		t.Run(test.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			_ = corev1.AddToScheme(scheme)
			service := dnsService()
			test.mutate(service)
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(service).Build()
			if _, e := (Cilium{}).DNS(context.Background(), c, "project"); e == nil {
				t.Fatal("unsupported DNS resolver accepted")
			}
		})
	}
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	if _, e := (Cilium{}).DNS(context.Background(), c, "project"); e == nil {
		t.Fatal("missing resolver accepted")
	}
	if _, e := (Cilium{ClusterDomain: "bad/domain"}).DNS(context.Background(), c, "project"); e == nil {
		t.Fatal("invalid cluster domain accepted")
	}
}
