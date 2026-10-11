package controller

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	api "git.cluster.aimingupward.com/steve/agent-project-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	ext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	sandbox := crd("agents.x-k8s.io", "Sandbox", "sandboxes")
	cilium := crd("cilium.io", "CiliumNetworkPolicy", "ciliumnetworkpolicies")
	cilium.Spec.Versions[0].Name = "v2"
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd")}, CRDs: []*ext.CustomResourceDefinition{sandbox, cilium}}
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
