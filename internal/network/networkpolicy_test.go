package network

import (
	"context"
	"strings"
	"testing"

	api "git.cluster.aimingupward.com/steve/agent-project-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func nativePolicy() NetworkPolicy {
	return NetworkPolicy{
		EnforcementAcknowledged: true,
		ClusterCIDRs:            []string{"44.0.0.0/8", "2600:1::/32"},
		Gateway:                 &Gateway{Namespace: "edge-system", Selector: map[string]string{"app": "gateway"}, Port: 8443, DisableSNIFilter: true},
	}
}

func asNetworkPolicy(t *testing.T, obj *unstructured.Unstructured) networkingv1.NetworkPolicy {
	t.Helper()
	var policy networkingv1.NetworkPolicy
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &policy); err != nil {
		t.Fatal(err)
	}
	return policy
}

func peerSelectors(t *testing.T, peer networkingv1.NetworkPolicyPeer) (string, map[string]string) {
	t.Helper()
	if peer.IPBlock != nil || peer.NamespaceSelector == nil || peer.PodSelector == nil {
		t.Fatalf("peer is not limited to one namespace and pod selector: %#v", peer)
	}
	return peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"], peer.PodSelector.MatchLabels
}

func portKeys(t *testing.T, ports []networkingv1.NetworkPolicyPort) map[string]bool {
	t.Helper()
	keys := map[string]bool{}
	for _, port := range ports {
		if port.Port == nil || port.Protocol == nil || port.EndPort != nil {
			t.Fatalf("broad port %#v", port)
		}
		keys[port.Port.String()+"/"+string(*port.Protocol)] = true
	}
	return keys
}

func TestNetworkPolicyObjectsStayInsideTheProjectProxy(t *testing.T) {
	policy := nativePolicy()
	objects := policy.Objects("project-a", "uid-a", []string{"git.example.com"})
	if len(objects) != 2 {
		t.Fatalf("policies = %d", len(objects))
	}
	worker := asNetworkPolicy(t, objects[0])
	proxy := asNetworkPolicy(t, objects[1])
	if worker.Name != "worker" || proxy.Name != "proxy" || worker.Namespace != "project-a" || proxy.Namespace != "project-a" {
		t.Fatal("policy identity changed")
	}
	if worker.Labels[api.OwnerLabel] != "uid-a" || proxy.Labels[api.OwnerLabel] != "uid-a" {
		t.Fatal("policy is not owned by the workspace")
	}
	if len(worker.Spec.PolicyTypes) != 2 || len(worker.Spec.Ingress) != 0 || len(worker.Spec.Egress) != 1 {
		t.Fatal("worker has ingress or extra egress")
	}
	namespace, pods := peerSelectors(t, worker.Spec.Egress[0].To[0])
	if namespace != "project-a" || pods[api.OwnerLabel] != "uid-a" || pods["projects.agentwork.dev/role"] != "proxy" || len(worker.Spec.Egress[0].To) != 1 {
		t.Fatal("worker can reach another project, the gateway, or a public address")
	}
	if !portKeys(t, worker.Spec.Egress[0].Ports)["3128/TCP"] || len(worker.Spec.Egress[0].Ports) != 1 {
		t.Fatal("worker egress is not only TCP 3128")
	}
	encoded, err := objects[0].MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, forbidden := range []string{"0.0.0.0/0", "::/0", "edge-system", "git.example.com", "kube-dns", `"53"`} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("worker policy contains %s", forbidden)
		}
	}

	namespace, pods = peerSelectors(t, proxy.Spec.Ingress[0].From[0])
	if len(proxy.Spec.Ingress) != 1 || len(proxy.Spec.Ingress[0].From) != 1 || namespace != "project-a" || pods[api.OwnerLabel] != "uid-a" || pods["projects.agentwork.dev/role"] != "worker" {
		t.Fatal("proxy accepts traffic from outside its worker")
	}
	if !portKeys(t, proxy.Spec.Ingress[0].Ports)["3128/TCP"] || len(proxy.Spec.Ingress[0].Ports) != 1 {
		t.Fatal("proxy ingress is not only TCP 3128")
	}
	if len(proxy.Spec.Egress) != 4 {
		t.Fatalf("proxy egress rules = %d", len(proxy.Spec.Egress))
	}
	namespace, pods = peerSelectors(t, proxy.Spec.Egress[0].To[0])
	dnsPorts := portKeys(t, proxy.Spec.Egress[0].Ports)
	if namespace != "kube-system" || pods["k8s-app"] != "kube-dns" || len(pods) != 1 || !dnsPorts["53/TCP"] || !dnsPorts["53/UDP"] || len(dnsPorts) != 2 {
		t.Fatal("proxy DNS is not limited to CoreDNS TCP and UDP 53")
	}
	for i, cidr := range []string{"0.0.0.0/0", "2000::/3"} {
		block := proxy.Spec.Egress[i+1].To[0].IPBlock
		ports := portKeys(t, proxy.Spec.Egress[i+1].Ports)
		if block == nil || block.CIDR != cidr || len(proxy.Spec.Egress[i+1].To) != 1 || !ports["443/TCP"] || len(ports) != 1 {
			t.Fatalf("public egress %s widened: %#v", cidr, proxy.Spec.Egress[i+1])
		}
	}
	v4 := map[string]bool{}
	for _, cidr := range proxy.Spec.Egress[1].To[0].IPBlock.Except {
		v4[cidr] = true
	}
	v6 := map[string]bool{}
	for _, cidr := range proxy.Spec.Egress[2].To[0].IPBlock.Except {
		v6[cidr] = true
	}
	for _, cidr := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16", "44.0.0.0/8"} {
		if !v4[cidr] {
			t.Fatalf("IPv4 exception missing %s", cidr)
		}
	}
	for _, cidr := range []string{"2001:db8::/32", "2600:1::/32"} {
		if !v6[cidr] {
			t.Fatalf("IPv6 exception missing %s", cidr)
		}
	}
	for _, cidr := range []string{"fc00::/7", "fe80::/10", "::/96", "::ffff:0:0/96", "ff00::/8"} {
		if v6[cidr] {
			t.Fatalf("IPv6 allow listed an unroutable or unexpressible exception %s", cidr)
		}
	}
	if v4["2600:1::/32"] || v6["10.0.0.0/8"] || len(v4) != 15 || len(v6) != 2 {
		t.Fatalf("exceptions crossed families or changed size: v4=%d v6=%d", len(v4), len(v6))
	}
	namespace, pods = peerSelectors(t, proxy.Spec.Egress[3].To[0])
	if namespace != "edge-system" || pods["app"] != "gateway" || len(proxy.Spec.Egress[3].To) != 1 || !portKeys(t, proxy.Spec.Egress[3].Ports)["8443/TCP"] {
		t.Fatal("gateway peer widened")
	}
}

func TestNetworkPolicyCheckRejectsUncoveredClusterAddresses(t *testing.T) {
	base := nativePolicy()
	pass := policyClient(t, "10.1.1.1", "", "10.244.0.0/24", "10.96.0.1")
	if err := base.Check(context.Background(), pass); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		node    string
		public  string
		podCIDR string
		api     string
		want    string
	}{
		{name: "public node", node: "10.1.1.1", public: "8.8.8.8", podCIDR: "10.244.0.0/24", api: "10.96.0.1", want: "node address is not excluded"},
		{name: "public pod cidr", node: "10.1.1.1", podCIDR: "8.8.8.0/24", api: "10.96.0.1", want: "pod CIDR is not excluded"},
		{name: "public api", node: "10.1.1.1", podCIDR: "10.244.0.0/24", api: "1.1.1.1", want: "cluster service address is not excluded"},
		{name: "default route pod cidr", node: "10.1.1.1", podCIDR: "0.0.0.0/0", api: "10.96.0.1", want: "pod CIDR is not excluded"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := policyClient(t, test.node, test.public, test.podCIDR, test.api)
			err := base.Check(context.Background(), c)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
	empty := fake.NewClientBuilder().WithScheme(policyScheme(t)).WithObjects(dnsService(), apiService("10.96.0.1")).Build()
	if err := base.Check(context.Background(), empty); err == nil || !strings.Contains(err.Error(), "cannot verify node") {
		t.Fatal(err)
	}
	missing := fake.NewClientBuilder().WithScheme(policyScheme(t)).Build()
	if err := base.Check(context.Background(), missing); err == nil {
		t.Fatal("missing DNS service accepted")
	}
}

func TestNetworkPolicyCheckRejectsUncoveredDirectDNS(t *testing.T) {
	scheme := policyScheme(t)
	if err := discoveryv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	ready := true
	tcp := corev1.ProtocolTCP
	udp := corev1.ProtocolUDP
	port := int32(53)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "dns", Namespace: "kube-system", Labels: map[string]string{"k8s-app": "kube-dns"}}, Status: corev1.PodStatus{PodIPs: []corev1.PodIP{{IP: "8.8.8.8"}}}}
	slice := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: "dns", Namespace: "kube-system", Labels: map[string]string{discoveryv1.LabelServiceName: "kube-dns"}}, AddressType: discoveryv1.AddressTypeIPv4, Ports: []discoveryv1.EndpointPort{{Port: &port, Protocol: &tcp}, {Port: &port, Protocol: &udp}}, Endpoints: []discoveryv1.Endpoint{{Addresses: []string{"8.8.8.8"}, Conditions: discoveryv1.EndpointConditions{Ready: &ready}, TargetRef: &corev1.ObjectReference{Kind: "Pod", Namespace: "kube-system", Name: "dns"}}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dnsService(), apiService("10.96.0.1"), pod, slice, policyNode("10.1.1.1", "", "10.244.0.0/24")).Build()
	policy := nativePolicy()
	policy.Resolver.DirectDNS = true
	err := policy.Check(context.Background(), c)
	if err == nil || !strings.Contains(err.Error(), "DNS endpoint is not covered") {
		t.Fatal(err)
	}
}

func policyScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func apiService(ip string) *corev1.Service {
	return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "kubernetes", Namespace: "default"}, Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, ClusterIP: ip, ClusterIPs: []string{ip}, Selector: map[string]string{"k8s-app": "kube-dns"}, Ports: []corev1.ServicePort{{Port: 53, Protocol: corev1.ProtocolUDP}, {Port: 53, Protocol: corev1.ProtocolTCP}}}}
}

func policyNode(internal, external, podCIDR string) *corev1.Node {
	addresses := []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: internal}}
	if external != "" {
		addresses = append(addresses, corev1.NodeAddress{Type: corev1.NodeExternalIP, Address: external})
	}
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}, Spec: corev1.NodeSpec{PodCIDR: podCIDR, PodCIDRs: []string{podCIDR}}, Status: corev1.NodeStatus{Addresses: addresses}}
}

func policyClient(t *testing.T, internal, external, podCIDR, api string) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(policyScheme(t)).WithObjects(dnsService(), apiService(api), policyNode(internal, external, podCIDR)).Build()
}
