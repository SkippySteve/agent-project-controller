package controller

import (
	"context"
	"strings"
	"testing"

	api "git.cluster.aimingupward.com/steve/agent-project-controller/api/v1alpha1"
	"git.cluster.aimingupward.com/steve/agent-project-controller/internal/network"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type serviceProxy struct{}

func (serviceProxy) Capabilities() network.Capabilities {
	return network.Capabilities{BypassPrevention: true, ServiceProxy: true}
}
func (serviceProxy) Check(context.Context, client.Client) error { return nil }
func (serviceProxy) DNS(context.Context, client.Client, string) (*corev1.PodDNSConfig, error) {
	return &corev1.PodDNSConfig{Nameservers: []string{"10.96.0.10"}}, nil
}
func (serviceProxy) Objects(ns, uid string, hosts []string) []*unstructured.Unstructured {
	return (network.NetworkPolicy{ClusterCIDRs: []string{"10.0.0.0/8", "fd00::/8"}, EnforcementAcknowledged: true}).Objects(ns, uid, hosts)
}

func useServiceProxy(r *Reconciler) {
	r.Network = serviceProxy{}
}

func seedProxy(t *testing.T, r *Reconciler, w *api.ProjectWorkspace, ip string) {
	t.Helper()
	service := &corev1.Service{ObjectMeta: metadata(w, "proxy"), Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, ClusterIP: ip, Selector: map[string]string{api.OwnerLabel: string(w.UID), "projects.agentwork.dev/role": "proxy"}, Ports: []corev1.ServicePort{{Port: 3128, TargetPort: intstr.FromInt(3128), Protocol: corev1.ProtocolTCP}}}}
	if err := r.Create(context.Background(), service); err != nil {
		t.Fatal(err)
	}
}

func sandboxHostAliases(t *testing.T, r *Reconciler, w *api.ProjectWorkspace) []any {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(sandboxGVK)
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: namespace(w), Name: "project"}, u); err != nil {
		t.Fatal(err)
	}
	aliases, _, _ := unstructured.NestedSlice(u.Object, "spec", "podTemplate", "spec", "hostAliases")
	return aliases
}

func TestServiceProxyPinsAllocatedAddressAndPreservesVolume(t *testing.T) {
	ctx := context.Background()
	r, w, h := fixture(t)
	useServiceProxy(r)
	seedProxy(t, r, w, "10.96.0.20")
	if err := r.ensure(ctx, w); err != nil {
		t.Fatal(err)
	}
	aliases := sandboxHostAliases(t, r, w)
	if len(aliases) != 1 || aliases[0].(map[string]any)["ip"] != "10.96.0.20" {
		t.Fatalf("host alias = %#v", aliases)
	}
	var pvc corev1.PersistentVolumeClaim
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace(w), Name: "workspace"}, &pvc); err != nil {
		t.Fatal(err)
	}
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: "control", Name: identity(w)}, &secret); err != nil {
		t.Fatal(err)
	}
	token := append([]byte{}, secret.Data["token"]...)
	claim := pvc.UID
	sandbox := &unstructured.Unstructured{}
	sandbox.SetGroupVersionKind(sandboxGVK)
	if err := r.Delete(ctx, sandboxFrom(namespace(w))); err != nil {
		t.Fatal(err)
	}
	restarted := *r
	if err := restarted.ensure(ctx, w); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace(w), Name: "workspace"}, &pvc); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: "control", Name: identity(w)}, &secret); err != nil {
		t.Fatal(err)
	}
	if pvc.UID != claim || string(secret.Data["token"]) != string(token) {
		t.Fatal("restart changed the volume or Git identity")
	}
	aliases = sandboxHostAliases(t, r, w)
	if len(aliases) != 1 || aliases[0].(map[string]any)["ip"] != "10.96.0.20" {
		t.Fatal("restart dropped the proxy host alias")
	}
	if h.grantCalls < 1 {
		t.Fatal("restart lost the recorded grant")
	}
	w.Spec.DesiredState = "Removed"
	w.Generation = 2
	if err := r.remove(ctx, w); err == nil {
		t.Fatal("namespace deletion should require observation")
	}
	if err := r.remove(ctx, w); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, types.NamespacedName{Name: namespace(w)}, &corev1.Namespace{}); err == nil {
		t.Fatal("native-provider removal left the namespace")
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: "control", Name: identity(w)}, &secret); err != nil {
		t.Fatal(err)
	}
	if h.revokeCalls == 0 || len(secret.Data["token"]) != 0 || string(secret.Data["revoked"]) != "true" {
		t.Fatal("native-provider removal skipped revocation or retained credentials")
	}
}

func sandboxFrom(ns string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(sandboxGVK)
	u.SetNamespace(ns)
	u.SetName("project")
	return u
}

func TestServiceProxyRejectsUnusableAndDriftedAccess(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name string
		ip   string
		want string
	}{
		{name: "missing", want: "allocated unicast ClusterIP"},
		{name: "headless", ip: corev1.ClusterIPNone, want: "proxy service drift"},
		{name: "loopback", ip: "127.0.0.1", want: "allocated unicast ClusterIP"},
		{name: "link-local", ip: "169.254.1.1", want: "allocated unicast ClusterIP"},
	} {
		t.Run(test.name, func(t *testing.T) {
			r, w, _ := fixture(t)
			useServiceProxy(r)
			if test.ip != "" {
				seedProxy(t, r, w, test.ip)
			}
			err := r.ensure(ctx, w)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}

	r, w, _ := fixture(t)
	useServiceProxy(r)
	seedProxy(t, r, w, "10.96.0.20")
	if err := r.ensure(ctx, w); err != nil {
		t.Fatal(err)
	}
	var service corev1.Service
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace(w), Name: "proxy"}, &service); err != nil {
		t.Fatal(err)
	}
	service.Spec.Ports[0].Port = 80
	if err := r.Update(ctx, &service); err != nil {
		t.Fatal(err)
	}
	if err := r.ensure(ctx, w); err == nil || !strings.Contains(err.Error(), "proxy service drift") {
		t.Fatal(err)
	}

	r, w, _ = fixture(t)
	useServiceProxy(r)
	seedProxy(t, r, w, "10.96.0.21")
	if err := r.ensure(ctx, w); err != nil {
		t.Fatal(err)
	}
	var policy networkingv1.NetworkPolicy
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace(w), Name: "worker"}, &policy); err != nil {
		t.Fatal(err)
	}
	udp := corev1.ProtocolUDP
	dnsPort := intstr.FromInt32(53)
	policy.Spec.Egress = append(policy.Spec.Egress, networkingv1.NetworkPolicyEgressRule{Ports: []networkingv1.NetworkPolicyPort{{Protocol: &udp, Port: &dnsPort}}})
	if err := r.Update(ctx, &policy); err != nil {
		t.Fatal(err)
	}
	if err := r.ensure(ctx, w); err == nil || !strings.Contains(err.Error(), "network policy drift") {
		t.Fatal(err)
	}

	r, w, _ = fixture(t)
	useServiceProxy(r)
	seedProxy(t, r, w, "10.96.0.22")
	if err := r.ensure(ctx, w); err != nil {
		t.Fatal(err)
	}
	extra := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "wide", Namespace: namespace(w)}, Spec: networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{}, PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}, Egress: []networkingv1.NetworkPolicyEgressRule{{}}}}
	if err := r.Create(ctx, extra); err != nil {
		t.Fatal(err)
	}
	if err := r.ensure(ctx, w); err == nil || !strings.Contains(err.Error(), "additional network policy") {
		t.Fatal(err)
	}
}

func TestProviderMigrationStaysSuspended(t *testing.T) {
	ctx := context.Background()
	r, w, _ := fixture(t)
	if err := r.ensure(ctx, w); err != nil {
		t.Fatal(err)
	}
	if aliases := sandboxHostAliases(t, r, w); len(aliases) != 0 {
		t.Fatal("Cilium worker received a native host alias")
	}
	var before corev1.PersistentVolumeClaim
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace(w), Name: "workspace"}, &before); err != nil {
		t.Fatal(err)
	}
	useServiceProxy(r)
	err := r.ensure(ctx, w)
	if err == nil || !strings.Contains(err.Error(), "proxy service drift") {
		t.Fatal(err)
	}
	var service corev1.Service
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace(w), Name: "proxy"}, &service); err != nil {
		t.Fatal(err)
	}
	if service.Spec.ClusterIP != corev1.ClusterIPNone {
		t.Fatal("failed migration rewrote the proxy service")
	}
	cilium := &unstructured.Unstructured{}
	cilium.SetGroupVersionKind(schema.GroupVersionKind{Group: "cilium.io", Version: "v2", Kind: "CiliumNetworkPolicy"})
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace(w), Name: "worker"}, cilium); err != nil {
		t.Fatal("failed migration deleted the existing policy", err)
	}
	var after corev1.PersistentVolumeClaim
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace(w), Name: "workspace"}, &after); err != nil || after.UID != before.UID {
		t.Fatal("failed migration changed the volume", err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "control", Name: w.Name}}); err == nil {
		t.Fatal("provider migration reconciled")
	}
	current := sandboxFrom(namespace(w))
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace(w), Name: "project"}, current); err != nil {
		t.Fatal(err)
	}
	mode, _, _ := unstructured.NestedString(current.Object, "spec", "operatingMode")
	if mode != "Suspended" {
		t.Fatalf("migration left the worker %s", mode)
	}
}
