package controller

import (
	"context"
	"strings"
	"testing"

	api "git.cluster.aimingupward.com/steve/agent-project-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

func sandboxImages(t *testing.T, r *Reconciler, namespace string) (string, string) {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(sandboxGVK)
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: "project"}, u); err != nil {
		t.Fatal(err)
	}
	init, _, _ := unstructured.NestedSlice(u.Object, "spec", "podTemplate", "spec", "initContainers")
	containers, _, _ := unstructured.NestedSlice(u.Object, "spec", "podTemplate", "spec", "containers")
	return init[0].(map[string]any)["image"].(string), containers[0].(map[string]any)["image"].(string)
}

func setSandboxImage(t *testing.T, r *Reconciler, namespace, image string) {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(sandboxGVK)
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: "project"}, u); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"initContainers", "containers"} {
		items, _, err := unstructured.NestedSlice(u.Object, "spec", "podTemplate", "spec", key)
		if err != nil || len(items) == 0 {
			t.Fatal(err)
		}
		items[0].(map[string]any)["image"] = image
		if err = unstructured.SetNestedSlice(u.Object, items, "spec", "podTemplate", "spec", key); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Update(context.Background(), u); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceImageRollKeepsVolumeAndRejectsMutation(t *testing.T) {
	ctx := context.Background()
	r, w, _ := fixture(t)
	if err := r.ensure(ctx, w); err != nil {
		t.Fatal(err)
	}
	var pvc corev1.PersistentVolumeClaim
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace(w), Name: "workspace"}, &pvc); err != nil {
		t.Fatal(err)
	}
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: "control", Name: identity(w)}, &secret); err != nil {
		t.Fatal(err)
	}
	claim := pvc.UID
	token := append([]byte{}, secret.Data["token"]...)
	pinned := r.Config.Image
	if clone, workspace := sandboxImages(t, r, namespace(w)); clone != pinned || workspace != pinned {
		t.Fatalf("initial images %s %s", clone, workspace)
	}

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: namespace(w), Labels: map[string]string{api.OwnerLabel: string(w.UID), "projects.agentwork.dev/role": "worker"}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "workspace", Image: pinned}}}}
	if err := r.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	setSandboxImage(t, r, namespace(w), "evil.example/worker:latest")
	if err := r.ensure(ctx, w); err == nil || !strings.Contains(err.Error(), "drift") {
		t.Fatal(err)
	}
	if clone, workspace := sandboxImages(t, r, namespace(w)); clone != "evil.example/worker:latest" || workspace != "evil.example/worker:latest" {
		t.Fatalf("running worker adopted images %s %s", clone, workspace)
	}
	if err := r.Delete(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if err := r.ensure(ctx, w); err == nil || !strings.Contains(err.Error(), "being replaced") {
		t.Fatal(err)
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(sandboxGVK)
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace(w), Name: "project"}, u); !apierrors.IsNotFound(err) {
		t.Fatal("mutated sandbox was left in place", err)
	}
	if err := r.ensure(ctx, w); err != nil {
		t.Fatal(err)
	}
	if clone, workspace := sandboxImages(t, r, namespace(w)); clone != pinned || workspace != pinned {
		t.Fatalf("recreated images %s %s", clone, workspace)
	}

	next := "example.com/worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	r.Config.Image = next
	if err := r.ensure(ctx, w); err == nil || !strings.Contains(err.Error(), "being replaced") {
		t.Fatal(err)
	}
	if err := r.ensure(ctx, w); err != nil {
		t.Fatal(err)
	}
	if clone, workspace := sandboxImages(t, r, namespace(w)); clone != next || workspace != next {
		t.Fatalf("rolled images %s %s", clone, workspace)
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace(w), Name: "workspace"}, &pvc); err != nil || pvc.UID != claim {
		t.Fatal("image roll replaced the volume", err)
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: "control", Name: identity(w)}, &secret); err != nil || string(secret.Data["token"]) != string(token) {
		t.Fatal("image roll replaced the Git identity", err)
	}

	setSandboxImage(t, r, namespace(w), next)
	containers, _, err := unstructured.NestedSlice(mustSandbox(t, r, namespace(w)).Object, "spec", "podTemplate", "spec", "containers")
	if err != nil {
		t.Fatal(err)
	}
	containers[0].(map[string]any)["command"] = []any{"evil"}
	sandbox := mustSandbox(t, r, namespace(w))
	if err = unstructured.SetNestedSlice(sandbox.Object, containers, "spec", "podTemplate", "spec", "containers"); err != nil {
		t.Fatal(err)
	}
	if err = r.Update(ctx, sandbox); err != nil {
		t.Fatal(err)
	}
	if err = r.ensure(ctx, w); err == nil || !strings.Contains(err.Error(), "drift") {
		t.Fatal(err)
	}
	if err = r.Get(ctx, types.NamespacedName{Namespace: namespace(w), Name: "project"}, sandbox); err != nil {
		t.Fatal("non-image drift deleted the sandbox", err)
	}
}

func mustSandbox(t *testing.T, r *Reconciler, namespace string) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(sandboxGVK)
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: "project"}, u); err != nil {
		t.Fatal(err)
	}
	return u
}
