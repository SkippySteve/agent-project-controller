package v1alpha1

import (
	"context"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"os"
	"path/filepath"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"testing"
)

func TestCRDValidation(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("set KUBEBUILDER_ASSETS to run a local Kubernetes API server")
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd")}, ErrorIfCRDPathMissing: true}
	cfg, e := env.Start()
	if e != nil {
		t.Fatal(e)
	}
	defer func() {
		if e := env.Stop(); e != nil {
			t.Error(e)
		}
	}()
	s := runtime.NewScheme()
	_ = AddToScheme(s)
	_ = corev1.AddToScheme(s)
	c, e := client.New(cfg, client.Options{Scheme: s})
	if e != nil {
		t.Fatal(e)
	}
	ctx := context.Background()
	if e = c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "control"}}); e != nil {
		t.Fatal(e)
	}
	w := &ProjectWorkspace{ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "control"}, Spec: WorkspaceSpec{Repository: "operator/repo", DesiredState: "Running"}}
	if e = c.Create(ctx, w); e != nil {
		t.Fatal("create", e)
	}
	key := types.NamespacedName{Namespace: w.Namespace, Name: w.Name}
	w.Spec.Repository = "operator/other"
	if e = c.Update(ctx, w); !apierrors.IsInvalid(e) {
		t.Fatal("repository mutation accepted", e)
	}
	_ = c.Get(ctx, key, w)
	w.Spec.HTTPS = []HTTPSRule{{Host: "example.com", Methods: []string{"GET"}, Paths: []string{"/"}}}
	if e = c.Update(ctx, w); !apierrors.IsInvalid(e) {
		t.Fatal("HTTPS expansion accepted", e)
	}
	_ = c.Get(ctx, key, w)
	w.Spec.DesiredState = "Removed"
	if e = c.Update(ctx, w); e != nil {
		t.Fatal(e)
	}
	w.Spec.DesiredState = "Running"
	if e = c.Update(ctx, w); !apierrors.IsInvalid(e) {
		t.Fatal("removed workspace reopened", e)
	}
	_ = c.Get(ctx, key, w)
	w.Status.Phase = "Removed"
	if e = c.Status().Update(ctx, w); e != nil {
		t.Fatal("status subresource", e)
	}
}
