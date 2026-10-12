package preflight

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"git.cluster.aimingupward.com/steve/agent-project-controller/internal/network"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type quietNet struct{ err error }

func (quietNet) Capabilities() network.Capabilities {
	return network.Capabilities{BypassPrevention: true}
}
func (quietNet) Objects(string, string, []string) []*unstructured.Unstructured {
	return nil
}
func (n quietNet) Check(context.Context, client.Client) error { return n.err }
func (n quietNet) DNS(context.Context, client.Client, string) (*corev1.PodDNSConfig, error) {
	if n.err != nil {
		return nil, n.err
	}
	return &corev1.PodDNSConfig{Nameservers: []string{"10.96.0.10"}}, nil
}

func preflightScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if e := storagev1.AddToScheme(s); e != nil {
		t.Fatal(e)
	}
	for _, gvk := range []schema.GroupVersionKind{
		{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"},
		{Group: "node.k8s.io", Version: "v1", Kind: "RuntimeClass"},
	} {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(gvk)
		s.AddKnownTypeWithName(gvk, obj)
	}
	return s
}

func object(gvk schema.GroupVersionKind, name string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetName(name)
	return obj
}

func TestPreflightPassesQualifiedShape(t *testing.T) {
	scheme := preflightScheme(t)
	policy := corev1.PersistentVolumeReclaimDelete
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		object(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"}, "sandboxes.agents.x-k8s.io"),
		object(schema.GroupVersionKind{Group: "node.k8s.io", Version: "v1", Kind: "RuntimeClass"}, "gvisor"),
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "local-path"}, ReclaimPolicy: &policy},
	).Build()
	results := Check(context.Background(), c, Options{RuntimeClass: "gvisor", StorageClass: "local-path", Network: quietNet{}})
	var out bytes.Buffer
	if e := Report(&out, results); e != nil {
		t.Fatal(e, out.String())
	}
	for _, name := range []string{"agent-sandbox", "cilium-dns", "runtime", "storage"} {
		if !strings.Contains(out.String(), "pass\t"+name+"\t") {
			t.Fatalf("missing pass for %s in %s", name, out.String())
		}
	}
}

func TestPreflightRejectsRetainAndMissingRuntime(t *testing.T) {
	scheme := preflightScheme(t)
	policy := corev1.PersistentVolumeReclaimRetain
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		object(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"}, "sandboxes.agents.x-k8s.io"),
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "local-path"}, ReclaimPolicy: &policy},
	).Build()
	results := Check(context.Background(), c, Options{RuntimeClass: "gvisor", StorageClass: "local-path", Network: quietNet{err: errors.New("cilium unavailable")}})
	var out bytes.Buffer
	if e := Report(&out, results); e == nil {
		t.Fatal("preflight passed")
	}
	text := out.String()
	for _, part := range []string{"fail\tcilium-dns\tcilium unavailable", "fail\truntime\tRuntimeClass gvisor unavailable", "fail\tstorage\tStorageClass local-path must exist and reclaim Delete", "pass\tagent-sandbox\t"} {
		if !strings.Contains(text, part) {
			t.Fatalf("missing %q in %s", part, text)
		}
	}
}
