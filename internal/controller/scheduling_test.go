package controller

import (
	"context"
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

func TestSchedulingParsing(t *testing.T) {
	for _, raw := range []string{"", `{}`, `{"nodeSelector":{},"tolerations":[],"affinity":{}}`} {
		s, err := ParseScheduling(raw)
		if err != nil || !reflect.DeepEqual(s, Scheduling{}) {
			t.Fatalf("default placement changes pod spec: %s: %+v %v", raw, s, err)
		}
	}
	for _, raw := range []string{`null`, `[]`, `{"hostNetwork":true}`, `{"nodeName":"node"}`, `{"nodeSelector":{"bad key":"value"}}`, `{"tolerations":[{"operator":"All"}]}`, `{"tolerations":[{"operator":"Exists","value":"anything"}]}`, `{"affinity":{"unknown":true}}`, `{} {}`} {
		if _, err := ParseScheduling(raw); err == nil {
			t.Fatalf("invalid placement accepted: %s", raw)
		}
	}
}

func TestProjectPlacementAndRestart(t *testing.T) {
	r, w, _ := fixture(t)
	placement, err := ParseScheduling(`{"nodeSelector":{"pool":"isolated"},"tolerations":[{"key":"dedicated","operator":"Equal","value":"agents","effect":"NoSchedule"}],"affinity":{"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"kubernetes.io/arch","operator":"In","values":["amd64"]}]}]}}}}`)
	if err != nil {
		t.Fatal(err)
	}
	r.Config.WorkerScheduling = placement
	r.Config.ProxyScheduling = Scheduling{NodeSelector: map[string]string{"pool": "proxy"}}
	ctx := context.Background()
	if err := r.ensure(ctx, w); err != nil {
		t.Fatal(err)
	}
	sandbox := &unstructured.Unstructured{}
	sandbox.SetGroupVersionKind(sandboxGVK)
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace(w), Name: "project"}, sandbox); err != nil {
		t.Fatal(err)
	}
	raw, _, _ := unstructured.NestedMap(sandbox.Object, "spec", "podTemplate", "spec")
	var pod corev1.PodSpec
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &pod); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pod.NodeSelector, placement.NodeSelector) || !reflect.DeepEqual(pod.Tolerations, placement.Tolerations) || !reflect.DeepEqual(pod.Affinity, placement.Affinity) {
		t.Fatalf("workspace placement lost: %+v", pod)
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken || pod.RuntimeClassName == nil || *pod.RuntimeClassName != "gvisor" {
		t.Fatal("placement changed isolation settings")
	}
	var deployment appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace(w), Name: "proxy"}, &deployment); err != nil {
		t.Fatal(err)
	}
	if deployment.Spec.Template.Spec.NodeSelector["pool"] != "proxy" {
		t.Fatal("proxy placement lost")
	}
	restarted := *r
	if err := restarted.ensure(ctx, w); err != nil {
		t.Fatalf("configured placement cannot recover after restart: %v", err)
	}
	restarted.Config.WorkerScheduling = Scheduling{NodeSelector: map[string]string{"pool": "different"}}
	if err := restarted.ensure(ctx, w); err == nil || !strings.Contains(err.Error(), "drift") {
		t.Fatalf("placement drift silently accepted: %v", err)
	}
}
