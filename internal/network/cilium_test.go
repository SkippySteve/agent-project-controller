package network

import (
	"context"
	"reflect"
	"testing"

	api "git.cluster.aimingupward.com/steve/agent-project-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
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

func TestGatewayIsProxyOnlyAndRestrictsServerNames(t *testing.T) {
	p := Cilium{Gateway: &Gateway{Namespace: "gateway-system", Selector: map[string]string{"app.kubernetes.io/name": "gateway"}, Port: 8443}}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	objects := p.Objects("project", "uid", []string{"git.example.com"})
	worker, _, _ := unstructured.NestedSlice(objects[0].Object, "spec", "egress")
	proxy, _, _ := unstructured.NestedSlice(objects[1].Object, "spec", "egress")
	if len(worker) != 2 || len(proxy) != 3 {
		t.Fatal("gateway permissions were added to workers or omitted from proxy")
	}
	route := proxy[2].(map[string]any)
	labels := route["toEndpoints"].([]any)[0].(map[string]any)["matchLabels"].(map[string]any)
	if labels["k8s:io.kubernetes.pod.namespace"] != "gateway-system" || labels["k8s:app.kubernetes.io/name"] != "gateway" {
		t.Fatal("gateway selector widened")
	}
	ports := route["toPorts"].([]any)[0].(map[string]any)
	if !reflect.DeepEqual(ports["serverNames"], []any{"git.example.com"}) || ports["ports"].([]any)[0].(map[string]any)["port"] != "8443" {
		t.Fatal("gateway host or port widened")
	}
	for _, bad := range []*Gateway{
		{Namespace: "", Selector: map[string]string{"app": "gateway"}, Port: 443},
		{Namespace: "gateway-system", Port: 443},
		{Namespace: "gateway-system", Selector: map[string]string{"app": ""}, Port: 443},
		{Namespace: "gateway-system", Selector: map[string]string{"io.kubernetes.pod.namespace": "other"}, Port: 443},
		{Namespace: "gateway-system", Selector: map[string]string{"app": "gateway"}, Port: 0},
	} {
		if (Cilium{Gateway: bad}).Validate() == nil {
			t.Fatal("incomplete or conflicting gateway accepted")
		}
	}
}

func TestDirectDNSUsesOnlyReadyCoreDNSPodEndpoints(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = discoveryv1.AddToScheme(scheme)
	ready := true
	tcp := corev1.ProtocolTCP
	udp := corev1.ProtocolUDP
	port := int32(53)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "dns", Namespace: "kube-system", Labels: map[string]string{"k8s-app": "kube-dns"}}, Status: corev1.PodStatus{PodIPs: []corev1.PodIP{{IP: "10.1.2.3"}}}}
	slice := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: "dns", Namespace: "kube-system", Labels: map[string]string{discoveryv1.LabelServiceName: "kube-dns"}}, AddressType: discoveryv1.AddressTypeIPv4, Ports: []discoveryv1.EndpointPort{{Port: &port, Protocol: &tcp}, {Port: &port, Protocol: &udp}}, Endpoints: []discoveryv1.Endpoint{{Addresses: []string{"10.1.2.3", "10.9.9.9"}, Conditions: discoveryv1.EndpointConditions{Ready: &ready}, TargetRef: &corev1.ObjectReference{Kind: "Pod", Namespace: "kube-system", Name: "dns"}}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dnsService(), pod, slice).Build()
	p := Cilium{DirectDNS: true}
	config, err := p.DNS(context.Background(), c, "project")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(config.Nameservers, []string{"10.1.2.3"}) {
		t.Fatal("DNS trusted an unverified endpoint address")
	}
	ready = false
	if err = c.Update(context.Background(), slice); err != nil {
		t.Fatal(err)
	}
	if _, err = p.DNS(context.Background(), c, "project"); err == nil {
		t.Fatal("unready DNS accepted")
	}
}

func TestGatewayCanAvoidCiliumSNIInterceptionWithoutWorkerPermissions(t *testing.T) {
	p := Cilium{Gateway: &Gateway{Namespace: "gateway-system", Selector: map[string]string{"app": "gateway"}, Port: 8443, DisableSNIFilter: true}}
	objects := p.Objects("project", "uid", []string{"git.example.com"})
	worker, _, _ := unstructured.NestedSlice(objects[0].Object, "spec", "egress")
	proxy, _, _ := unstructured.NestedSlice(objects[1].Object, "spec", "egress")
	if len(worker) != 2 || len(proxy) != 3 {
		t.Fatal("gateway option changed worker permissions")
	}
	ports := proxy[2].(map[string]any)["toPorts"].([]any)[0].(map[string]any)
	if _, exists := ports["serverNames"]; exists {
		t.Fatal("SNI interception remained enabled")
	}
	if len(proxy[2].(map[string]any)["toEndpoints"].([]any)) != 1 {
		t.Fatal("endpoint restriction removed")
	}
}
