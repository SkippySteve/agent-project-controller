package controller

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	api "git.cluster.aimingupward.com/steve/agent-project-controller/api/v1alpha1"
	"git.cluster.aimingupward.com/steve/agent-project-controller/internal/network"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	storagev1 "k8s.io/api/storage/v1"
	ext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestProvisioningWithAPIDefaults(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("set KUBEBUILDER_ASSETS for local API server qualification")
	}
	unknown := true
	crd := func(group, kind, plural string) *ext.CustomResourceDefinition {
		return &ext.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: plural + "." + group}, Spec: ext.CustomResourceDefinitionSpec{Group: group, Scope: ext.NamespaceScoped, Names: ext.CustomResourceDefinitionNames{Kind: kind, Plural: plural}, Versions: []ext.CustomResourceDefinitionVersion{{Name: "v1beta1", Served: true, Storage: true, Schema: &ext.CustomResourceValidation{OpenAPIV3Schema: &ext.JSONSchemaProps{Type: "object", XPreserveUnknownFields: &unknown}}}}}}
	}
	cilium := crd("cilium.io", "CiliumNetworkPolicy", "ciliumnetworkpolicies")
	cilium.Spec.Versions[0].Name = "v2"
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd"), filepath.Join("testdata", "agent-sandbox-v1.0.4")}, CRDs: []*ext.CustomResourceDefinition{cilium}}
	cfg, e := env.Start()
	if e != nil {
		t.Fatal(e)
	}
	defer func() {
		if e := env.Stop(); e != nil {
			t.Error(e)
		}
	}()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = api.AddToScheme(scheme)
	c, e := client.New(cfg, client.Options{Scheme: scheme})
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if e = c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "control"}}); e != nil {
		t.Fatal(e)
	}
	w := &api.ProjectWorkspace{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: "control"}, Spec: api.WorkspaceSpec{Repository: "steve/project", DesiredState: "Running"}}
	if e = c.Create(ctx, w); e != nil {
		t.Fatal(e)
	}
	r := &Reconciler{Client: c, Host: &host{}, Network: provider{}, Config: Config{Namespace: "control", Owner: "steve", ForgejoOrigin: "https://git.example.com", Image: "example.com/worker@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RuntimeClass: "gvisor", StorageClass: "delete", StorageSize: "1Gi"}}
	placement, err := ParseScheduling(`{"nodeSelector":{"pool":"agents"},"tolerations":[{"key":"dedicated","operator":"Exists","effect":"NoSchedule"}],"affinity":{"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"kubernetes.io/arch","operator":"In","values":["amd64"]}]}]}}}}`)
	if err != nil {
		t.Fatal(err)
	}
	r.Config.WorkerScheduling = placement
	r.Config.ProxyScheduling = Scheduling{NodeSelector: map[string]string{"pool": "proxies"}}
	policy := corev1.PersistentVolumeReclaimDelete
	if e = c.Create(ctx, &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "delete"}, Provisioner: "fixture", ReclaimPolicy: &policy}); e != nil {
		t.Fatal(e)
	}
	if e = r.ensure(ctx, w); e != nil {
		t.Fatal("first provisioning", e)
	}
	// The API server defaults native Deployments, Services, PVCs and ServiceAccounts.
	if e = r.ensure(ctx, w); e != nil {
		t.Fatal("provisioning retry with API defaults", e)
	}
	// The real upstream CRD must preserve fields that establish our isolation and readiness.
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(sandboxGVK)
	if e = c.Get(ctx, types.NamespacedName{Namespace: namespace(w), Name: "project"}, u); e != nil {
		t.Fatal(e)
	}
	pod, found, e := unstructured.NestedMap(u.Object, "spec", "podTemplate", "spec")
	if e != nil || !found {
		t.Fatal("upstream schema pruned pod template")
	}
	if pod["dnsPolicy"] != "None" || pod["runtimeClassName"] != "gvisor" || pod["automountServiceAccountToken"] != false {
		t.Fatal("upstream schema did not preserve isolation and DNS settings")
	}
	if !contains(pod["nodeSelector"], map[string]any{"pool": "agents"}) || pod["affinity"] == nil || pod["tolerations"] == nil {
		t.Fatal("upstream schema pruned workspace placement")
	}
	containers, _, _ := unstructured.NestedSlice(pod, "containers")
	worker := containers[0].(map[string]any)
	if !contains(worker["resources"], map[string]any{"limits": map[string]any{"cpu": "4", "memory": "4Gi"}}) {
		t.Fatal("worker sizing missing from upstream resource")
	}
	if _, ok := worker["readinessProbe"]; !ok {
		t.Fatal("upstream schema removed project readiness")
	}
	var deployment appsv1.Deployment
	if e = c.Get(ctx, types.NamespacedName{Namespace: namespace(w), Name: "proxy"}, &deployment); e != nil {
		t.Fatal(e)
	}
	if deployment.Spec.Template.Spec.DNSPolicy != corev1.DNSNone || deployment.Spec.Template.Spec.Containers[0].Resources.Limits.Memory().String() != "512Mi" {
		t.Fatal("proxy settings lost to API defaulting")
	}
	if deployment.Spec.Template.Spec.NodeSelector["pool"] != "proxies" {
		t.Fatal("proxy placement lost to API defaulting")
	}
	w.Spec.DesiredState = "Paused"
	if e = c.Update(ctx, w); e != nil {
		t.Fatal(e)
	}
	if e = r.ensure(ctx, w); e != nil {
		t.Fatal("pause with upstream schema", e)
	}
	if e = c.Get(ctx, types.NamespacedName{Namespace: namespace(w), Name: "project"}, u); e != nil {
		t.Fatal(e)
	}
	if mode, _, _ := unstructured.NestedString(u.Object, "spec", "operatingMode"); mode != "Suspended" {
		t.Fatal("pause did not select upstream Suspended mode")
	}
	if e = r.status(ctx, w, "Preparing", false, "PreparationPending", "Waiting for clone"); e != nil {
		t.Fatal(e)
	}
	var secret corev1.Secret
	if e = c.Get(ctx, types.NamespacedName{Namespace: namespace(w), Name: "git-identity"}, &secret); e != nil {
		t.Fatal(e)
	}
	if _, ok := secret.Data["password"]; ok {
		t.Fatal("administrative identity password mounted in worker")
	}
	var trust corev1.ConfigMap
	if e = c.Get(ctx, types.NamespacedName{Namespace: namespace(w), Name: "trust"}, &trust); e != nil {
		t.Fatal(e)
	}
	if len(trust.Data) != 1 {
		t.Fatal("private inspection key exposed to worker")
	}
}

func TestNetworkPolicyAPIDefaults(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("set KUBEBUILDER_ASSETS for local API server qualification")
	}
	unknown := true
	crd := func(group, kind, plural string) *ext.CustomResourceDefinition {
		return &ext.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: plural + "." + group}, Spec: ext.CustomResourceDefinitionSpec{Group: group, Scope: ext.NamespaceScoped, Names: ext.CustomResourceDefinitionNames{Kind: kind, Plural: plural}, Versions: []ext.CustomResourceDefinitionVersion{{Name: "v1beta1", Served: true, Storage: true, Schema: &ext.CustomResourceValidation{OpenAPIV3Schema: &ext.JSONSchemaProps{Type: "object", XPreserveUnknownFields: &unknown}}}}}}
	}
	cilium := crd("cilium.io", "CiliumNetworkPolicy", "ciliumnetworkpolicies")
	cilium.Spec.Versions[0].Name = "v2"
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd"), filepath.Join("testdata", "agent-sandbox-v1.0.4")}, CRDs: []*ext.CustomResourceDefinition{cilium}}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := env.Stop(); err != nil {
			t.Error(err)
		}
	}()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = api.AddToScheme(scheme)
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	if err = c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "control"}}); err != nil {
		t.Fatal(err)
	}
	policy := corev1.PersistentVolumeReclaimDelete
	if err = c.Create(ctx, &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "delete"}, Provisioner: "fixture", ReclaimPolicy: &policy}); err != nil {
		t.Fatal(err)
	}
	newWorkspace := func(name string) *api.ProjectWorkspace {
		t.Helper()
		w := &api.ProjectWorkspace{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "control"}, Spec: api.WorkspaceSpec{Repository: "steve/project", DesiredState: "Running"}}
		if err := c.Create(ctx, w); err != nil {
			t.Fatal(err)
		}
		return w
	}
	reconciler := func(provider network.Provider) *Reconciler {
		return &Reconciler{Client: c, Host: &host{}, Network: provider, Config: Config{Namespace: "control", Owner: "steve", ForgejoOrigin: "https://git.example.com", Image: "example.com/worker@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RuntimeClass: "gvisor", StorageClass: "delete", StorageSize: "1Gi"}}
	}
	ciliumWorkspace := newWorkspace("cilium-project")
	ciliumReconciler := reconciler(provider{})
	if err = ciliumReconciler.ensure(ctx, ciliumWorkspace); err != nil {
		t.Fatal(err)
	}
	native := reconciler(serviceProxy{})
	native.Client = c
	if err = native.ensure(ctx, ciliumWorkspace); err == nil || !strings.Contains(err.Error(), "proxy service drift") {
		t.Fatal(err)
	}
	var migrated corev1.Service
	if err = c.Get(ctx, types.NamespacedName{Namespace: namespace(ciliumWorkspace), Name: "proxy"}, &migrated); err != nil {
		t.Fatal(err)
	}
	if migrated.Spec.ClusterIP != corev1.ClusterIPNone {
		t.Fatal("API migration rewrote the headless proxy")
	}

	w := newWorkspace("native-project")
	r := reconciler(serviceProxy{})
	if err = r.ensure(ctx, w); err != nil {
		t.Fatal("native provisioning", err)
	}
	if err = r.ensure(ctx, w); err != nil {
		t.Fatal("native retry", err)
	}
	var service corev1.Service
	if err = c.Get(ctx, types.NamespacedName{Namespace: namespace(w), Name: "proxy"}, &service); err != nil {
		t.Fatal(err)
	}
	address, err := netip.ParseAddr(service.Spec.ClusterIP)
	if err != nil || !address.IsGlobalUnicast() || service.Spec.ClusterIP == corev1.ClusterIPNone {
		t.Fatal("API server did not allocate a proxy ClusterIP", service.Spec.ClusterIP)
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(sandboxGVK)
	if err = c.Get(ctx, types.NamespacedName{Namespace: namespace(w), Name: "project"}, u); err != nil {
		t.Fatal(err)
	}
	aliases, found, err := unstructured.NestedSlice(u.Object, "spec", "podTemplate", "spec", "hostAliases")
	if err != nil || !found || len(aliases) != 1 || aliases[0].(map[string]any)["ip"] != address.String() {
		t.Fatalf("upstream schema host alias = %#v (%v)", aliases, err)
	}
	var worker networkingv1.NetworkPolicy
	if err = c.Get(ctx, types.NamespacedName{Namespace: namespace(w), Name: "worker"}, &worker); err != nil {
		t.Fatal(err)
	}
	if len(worker.Spec.Ingress) != 0 || len(worker.Spec.Egress) != 1 {
		t.Fatal("API defaulting widened worker policy")
	}
	var pvc corev1.PersistentVolumeClaim
	if err = c.Get(ctx, types.NamespacedName{Namespace: namespace(w), Name: "workspace"}, &pvc); err != nil {
		t.Fatal(err)
	}
	extra := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "wide", Namespace: namespace(w)}, Spec: networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{}, PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}, Egress: []networkingv1.NetworkPolicyEgressRule{{}}}}
	if err = c.Create(ctx, extra); err != nil {
		t.Fatal(err)
	}
	if err = r.ensure(ctx, w); err == nil || !strings.Contains(err.Error(), "additional network policy") {
		t.Fatal(err)
	}
	if err = c.Delete(ctx, extra); err != nil {
		t.Fatal(err)
	}
	if err = r.ensure(ctx, w); err != nil {
		t.Fatal("retry after removing extra policy", err)
	}
	var again corev1.PersistentVolumeClaim
	if err = c.Get(ctx, types.NamespacedName{Namespace: namespace(w), Name: "workspace"}, &again); err != nil || again.UID != pvc.UID {
		t.Fatal("policy retry replaced the volume", err)
	}
	w.Spec.DesiredState = "Removed"
	w.Generation = 2
	if err = c.Update(ctx, w); err != nil {
		t.Fatal(err)
	}
	if err = r.remove(ctx, w); err == nil || !strings.Contains(err.Error(), "waiting for namespace") {
		t.Fatal(err)
	}
	var removing corev1.Namespace
	if err = c.Get(ctx, types.NamespacedName{Name: namespace(w)}, &removing); err != nil {
		t.Fatal(err)
	}
	if removing.DeletionTimestamp.IsZero() || r.Host.(*host).revokeCalls == 0 {
		t.Fatal("native removal did not revoke access and mark the namespace for deletion")
	}
}
