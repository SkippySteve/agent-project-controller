package controller

import (
	"context"
	"errors"
	"testing"

	api "git.cluster.aimingupward.com/steve/agent-project-controller/api/v1alpha1"
	"git.cluster.aimingupward.com/steve/agent-project-controller/internal/forgejo"
	"git.cluster.aimingupward.com/steve/agent-project-controller/internal/network"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type host struct {
	grantCalls, revokeCalls int
	failRevoke              bool
}

func (h *host) Discover(context.Context) ([]forgejo.Repository, error) { return nil, nil }
func (h *host) Grant(context.Context, string, forgejo.Credentials) (string, error) {
	h.grantCalls++
	return "fixture-token", nil
}
func (h *host) Revoke(context.Context, string, forgejo.Credentials) error {
	h.revokeCalls++
	if h.failRevoke {
		return errors.New("Forgejo unavailable")
	}
	return nil
}

type provider struct{ network.Cilium }

func (provider) Check(context.Context, client.Client) error { return nil }
func (provider) DNS(context.Context, client.Client, string) (*corev1.PodDNSConfig, error) {
	return &corev1.PodDNSConfig{Nameservers: []string{"10.96.0.10"}}, nil
}
func fixture(t *testing.T) (*Reconciler, *api.ProjectWorkspace, *host) {
	t.Helper()
	s := runtime.NewScheme()
	_ = api.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	_ = storagev1.AddToScheme(s)
	_ = appsv1.AddToScheme(s)
	sandbox := &unstructured.Unstructured{}
	sandbox.SetGroupVersionKind(sandboxGVK)
	s.AddKnownTypeWithName(sandboxGVK, sandbox)
	runtimeClass := &unstructured.Unstructured{}
	runtimeClass.SetGroupVersionKind(schema.GroupVersionKind{Group: "node.k8s.io", Version: "v1", Kind: "RuntimeClass"})
	runtimeClass.SetName("gvisor")
	policy := corev1.PersistentVolumeReclaimDelete
	w := &api.ProjectWorkspace{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: "control", UID: types.UID("12345678-1234-1234-1234-123456789012"), Generation: 1, Finalizers: []string{api.Finalizer}}, Spec: api.WorkspaceSpec{Repository: "steve/project", DesiredState: "Running"}}
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(w).WithObjects(w, runtimeClass, &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "delete"}, ReclaimPolicy: &policy}).Build()
	h := &host{}
	r := &Reconciler{Client: c, Host: h, Network: provider{}, Config: Config{Namespace: "control", Owner: "steve", ForgejoOrigin: "https://git.example.com", Image: "example.com/worker@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RuntimeClass: "gvisor", StorageClass: "delete", StorageSize: "1Gi"}}
	return r, w, h
}
func TestRecoveryAndRemoval(t *testing.T) {
	ctx := context.Background()
	r, w, h := fixture(t)
	if e := r.ensure(ctx, w); e != nil {
		t.Fatal(e)
	}
	var secret corev1.Secret
	if e := r.Get(ctx, types.NamespacedName{Namespace: "control", Name: identity(w)}, &secret); e != nil {
		t.Fatal(e)
	}
	if len(secret.Data["password"]) == 0 {
		t.Fatal("recovery identity missing")
	}
	// Simulate a fresh controller after provisioning. All objects and credentials survive in the API.
	restarted := *r
	r = &restarted
	if e := r.ensure(ctx, w); e != nil {
		t.Fatal("retry failed", e)
	}
	h.failRevoke = true
	w.Spec.DesiredState = "Removed"
	w.Generation = 2
	if e := r.remove(ctx, w); e == nil {
		t.Fatal("revocation failure treated as cleanup success")
	}
	sandbox := &unstructured.Unstructured{}
	sandbox.SetGroupVersionKind(sandboxGVK)
	if e := r.Get(ctx, types.NamespacedName{Namespace: namespace(w), Name: "project"}, sandbox); e != nil {
		t.Fatal(e)
	}
	mode, _, _ := unstructured.NestedString(sandbox.Object, "spec", "operatingMode")
	if mode != "Suspended" {
		t.Fatal("workload not suspended during Forgejo outage")
	}
	h.failRevoke = false
	if e := r.remove(ctx, w); e == nil {
		t.Fatal("namespace deletion should require observation")
	}
	if e := r.remove(ctx, w); e != nil {
		t.Fatal(e)
	}
	if e := r.Get(ctx, types.NamespacedName{Namespace: "control", Name: identity(w)}, &secret); e != nil {
		t.Fatal(e)
	}
	if len(secret.Data["token"]) != 0 || len(secret.Data["password"]) != 0 || len(secret.Data["ca.key"]) != 0 {
		t.Fatal("control-plane credential material retained after cleanup")
	}
	grants := h.grantCalls
	if e := r.Update(ctx, w); e != nil {
		t.Fatal(e)
	}
	_, e := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "control", Name: w.Name}})
	if e != nil {
		t.Fatal(e)
	}
	if h.grantCalls != grants {
		t.Fatal("removed workspace regranted access")
	}
}
func TestRetainedVolumeBlocksCompletion(t *testing.T) {
	ctx := context.Background()
	r, w, _ := fixture(t)
	if e := r.ensure(ctx, w); e != nil {
		t.Fatal(e)
	}
	if e := r.Create(ctx, &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "remaining"}, Spec: corev1.PersistentVolumeSpec{ClaimRef: &corev1.ObjectReference{Namespace: namespace(w), Name: "workspace"}}}); e != nil {
		t.Fatal(e)
	}
	_ = r.remove(ctx, w)
	if e := r.remove(ctx, w); e == nil {
		t.Fatal("remaining PV treated as removed")
	}
}
func TestRefusesUnownedNamespace(t *testing.T) {
	ctx := context.Background()
	r, w, _ := fixture(t)
	if e := r.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace(w)}}); e != nil {
		t.Fatal(e)
	}
	if e := r.remove(ctx, w); e == nil {
		t.Fatal("unowned namespace deleted")
	}
	var ns corev1.Namespace
	if e := r.Get(ctx, types.NamespacedName{Name: namespace(w)}, &ns); e != nil {
		t.Fatal("unowned namespace disappeared")
	}
}
func TestRetainStorageRejectedBeforeGrant(t *testing.T) {
	ctx := context.Background()
	r, w, h := fixture(t)
	sc := &storagev1.StorageClass{}
	_ = r.Get(ctx, types.NamespacedName{Name: "delete"}, sc)
	retain := corev1.PersistentVolumeReclaimRetain
	sc.ReclaimPolicy = &retain
	_ = r.Update(ctx, sc)
	if e := r.validate(ctx, w); e == nil {
		t.Fatal("retained storage allowed")
	}
	if h.grantCalls != 0 {
		t.Fatal("access granted before validating storage")
	}
}

func TestRemovalRevokesDespiteLocalOwnershipDrift(t *testing.T) {
	ctx := context.Background()
	r, w, h := fixture(t)
	if e := r.ensure(ctx, w); e != nil {
		t.Fatal(e)
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(sandboxGVK)
	key := types.NamespacedName{Namespace: namespace(w), Name: "project"}
	if e := r.Get(ctx, key, u); e != nil {
		t.Fatal(e)
	}
	u.SetLabels(map[string]string{api.OwnerLabel: "someone-else"})
	if e := r.Update(ctx, u); e != nil {
		t.Fatal(e)
	}
	if e := r.remove(ctx, w); e == nil {
		t.Fatal("ownership drift allowed cleanup completion")
	}
	if h.revokeCalls != 1 {
		t.Fatal("local drift blocked external revocation")
	}
	if e := r.Get(ctx, key, u); e != nil {
		t.Fatal("unowned Sandbox deleted")
	}
}

func TestMissingLedgerAfterPriorGenerationGrantBlocksRemoval(t *testing.T) {
	ctx := context.Background()
	r, w, _ := fixture(t)
	if e := r.ensure(ctx, w); e != nil {
		t.Fatal(e)
	}
	s := &corev1.Secret{}
	if e := r.Get(ctx, types.NamespacedName{Namespace: "control", Name: identity(w)}, s); e != nil {
		t.Fatal(e)
	}
	if e := r.Delete(ctx, s); e != nil {
		t.Fatal(e)
	}
	w.Generation = 3
	if e := r.remove(ctx, w); e == nil {
		t.Fatal("missing prior-generation ledger accepted")
	}
	var ns corev1.Namespace
	if e := r.Get(ctx, types.NamespacedName{Name: namespace(w)}, &ns); e != nil {
		t.Fatal("storage deleted without verified revocation")
	}
}

func TestAddedNetworkPermissionRejected(t *testing.T) {
	ctx := context.Background()
	r, w, _ := fixture(t)
	if e := r.ensure(ctx, w); e != nil {
		t.Fatal(e)
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: "cilium.io", Version: "v2", Kind: "CiliumNetworkPolicy"})
	if e := r.Get(ctx, types.NamespacedName{Namespace: namespace(w), Name: "worker"}, u); e != nil {
		t.Fatal(e)
	}
	rules, _, _ := unstructured.NestedSlice(u.Object, "spec", "egress")
	rules[1].(map[string]any)["toEntities"] = []any{"world"}
	_ = unstructured.SetNestedSlice(u.Object, rules, "spec", "egress")
	if e := r.Update(ctx, u); e != nil {
		t.Fatal(e)
	}
	if e := r.ensure(ctx, w); e == nil {
		t.Fatal("added network permission accepted")
	}
}

func TestAddedPodPrivilegesRejected(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*corev1.PodSpec)
	}{
		{"host-network", func(p *corev1.PodSpec) { p.HostNetwork = true }},
		{"host-pid", func(p *corev1.PodSpec) { p.HostPID = true }},
		{"host-ipc", func(p *corev1.PodSpec) { p.HostIPC = true }},
		{"capability", func(p *corev1.PodSpec) {
			p.Containers[0].SecurityContext.Capabilities.Add = []corev1.Capability{"NET_ADMIN"}
		}},
		{"unmasked-proc", func(p *corev1.PodSpec) { v := corev1.UnmaskedProcMount; p.Containers[0].SecurityContext.ProcMount = &v }},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			r, w, _ := fixture(t)
			if e := r.ensure(ctx, w); e != nil {
				t.Fatal(e)
			}
			var deployment appsv1.Deployment
			if e := r.Get(ctx, types.NamespacedName{Namespace: namespace(w), Name: "proxy"}, &deployment); e != nil {
				t.Fatal(e)
			}
			test.change(&deployment.Spec.Template.Spec)
			if e := r.Update(ctx, &deployment); e != nil {
				t.Fatal(e)
			}
			if e := r.ensure(ctx, w); e == nil {
				t.Fatal("added proxy privilege accepted")
			}
		})
	}
}

func TestAddedSandboxHostNetworkingRejected(t *testing.T) {
	ctx := context.Background()
	r, w, _ := fixture(t)
	if e := r.ensure(ctx, w); e != nil {
		t.Fatal(e)
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(sandboxGVK)
	if e := r.Get(ctx, types.NamespacedName{Namespace: namespace(w), Name: "project"}, u); e != nil {
		t.Fatal(e)
	}
	if e := unstructured.SetNestedField(u.Object, true, "spec", "podTemplate", "spec", "hostNetwork"); e != nil {
		t.Fatal(e)
	}
	if e := r.Update(ctx, u); e != nil {
		t.Fatal(e)
	}
	if e := r.ensure(ctx, w); e == nil {
		t.Fatal("Sandbox gained host network access")
	}
}
