package controller

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	api "git.cluster.aimingupward.com/steve/agent-project-controller/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestPausedAccessUpdatePreservesIdentityAndRejectsRunningChange(t *testing.T) {
	ctx := context.Background()
	r, w, _ := fixture(t)
	if err := r.ensure(ctx, w); err != nil {
		t.Fatal(err)
	}
	var before corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: w.Namespace, Name: identity(w)}, &before); err != nil {
		t.Fatal(err)
	}
	w.Spec.HTTPS = []api.HTTPSRule{{Host: "api.example.com", Methods: []string{"GET"}, Paths: []string{"/v1"}}}
	w.Generation++
	if err := r.Update(ctx, w); err != nil {
		t.Fatal(err)
	}
	if err := r.ensure(ctx, w); err == nil || !strings.Contains(err.Error(), "pause the project") {
		t.Fatal("accepted running policy change", err)
	}
	w.Spec.DesiredState = "Paused"
	w.Generation++
	if err := r.Update(ctx, w); err != nil {
		t.Fatal(err)
	}
	if err := r.ensure(ctx, w); err != nil {
		t.Fatal(err)
	}
	var after corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: w.Namespace, Name: identity(w)}, &after); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"username", "password", "token", "ca.key", "ca.crt"} {
		if !reflect.DeepEqual(before.Data[field], after.Data[field]) {
			t.Fatal("identity changed", field)
		}
	}
	var applied []api.HTTPSRule
	if err := json.Unmarshal(after.Data[HTTPSLedgerKey], &applied); err != nil || !reflect.DeepEqual(applied, w.Spec.HTTPS) {
		t.Fatal("policy not saved", err)
	}
	var deployment appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace(w), Name: "proxy"}, &deployment); err != nil || *deployment.Spec.Replicas != 0 {
		t.Fatal("proxy not stopped", err)
	}
	w.Spec.DesiredState = "Running"
	w.Generation++
	if err := r.Update(ctx, w); err != nil {
		t.Fatal(err)
	}
	if err := r.ensure(ctx, w); err != nil {
		t.Fatal("resume", err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(&deployment), &deployment); err != nil || *deployment.Spec.Replicas != 1 {
		t.Fatal("proxy not resumed", err)
	}
}

func TestAccessUpdateWaitsForTerminatingProcessesAndRefusesDrift(t *testing.T) {
	ctx := context.Background()
	r, w, _ := fixture(t)
	if err := r.ensure(ctx, w); err != nil {
		t.Fatal(err)
	}
	w.Spec.DesiredState = "Paused"
	w.Generation++
	if err := r.Update(ctx, w); err != nil {
		t.Fatal(err)
	}
	if err := r.ensure(ctx, w); err != nil {
		t.Fatal(err)
	}
	w.Spec.HTTPS = []api.HTTPSRule{{Host: "example.com", Methods: []string{"GET"}, Paths: []string{"/"}}}
	w.Generation++
	if err := r.Update(ctx, w); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "terminating-proxy", Namespace: namespace(w), Labels: labels(w)}}
	if err := r.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if err := r.ensure(ctx, w); err == nil || !strings.Contains(err.Error(), "processes to stop") {
		t.Fatal("changed access with live process", err)
	}
	if err := r.Delete(ctx, pod); err != nil {
		t.Fatal(err)
	}
	var cm corev1.ConfigMap
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace(w), Name: "proxy-policy"}, &cm); err != nil {
		t.Fatal(err)
	}
	cm.Data["policy.json"] = `{"rules":[{"host":"unexpected.example","methods":["GET"],"paths":["/"]}]}`
	if err := r.Update(ctx, &cm); err != nil {
		t.Fatal(err)
	}
	if err := r.ensure(ctx, w); err == nil || !strings.Contains(err.Error(), "drift") {
		t.Fatal("adopted tampered policy", err)
	}
}

type failPolicyUpdate struct {
	client.Client
	failed bool
}

func (c *failPolicyUpdate) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if cm, ok := obj.(*corev1.ConfigMap); ok && cm.Name == "proxy-policy" && !c.failed {
		c.failed = true
		return errors.New("fixture interrupted policy update")
	}
	return c.Client.Update(ctx, obj, opts...)
}
func TestInterruptedAccessUpdateRecoversWithoutRecreatingStorage(t *testing.T) {
	ctx := context.Background()
	r, w, _ := fixture(t)
	if err := r.ensure(ctx, w); err != nil {
		t.Fatal(err)
	}
	w.Spec.DesiredState = "Paused"
	w.Generation++
	if err := r.Update(ctx, w); err != nil {
		t.Fatal(err)
	}
	if err := r.ensure(ctx, w); err != nil {
		t.Fatal(err)
	}
	w.Spec.HTTPS = []api.HTTPSRule{{Host: "example.com", Methods: []string{"GET"}, Paths: []string{"/new"}}}
	w.Generation++
	if err := r.Update(ctx, w); err != nil {
		t.Fatal(err)
	}
	r.Client = &failPolicyUpdate{Client: r.Client}
	if err := r.ensure(ctx, w); err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatal("did not interrupt", err)
	}
	restarted := *r
	if err := restarted.ensure(ctx, w); err != nil {
		t.Fatal("recovery", err)
	}
	if err := restarted.ensure(ctx, w); err != nil {
		t.Fatal("idempotent retry", err)
	}
	var pvc corev1.PersistentVolumeClaim
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace(w), Name: "workspace"}, &pvc); err != nil {
		t.Fatal("lost storage", err)
	}
}

func TestRepositoryHostsCannotBeOverridden(t *testing.T) {
	r, w, _ := fixture(t)
	for _, host := range []string{"git.example.com"} {
		_, err := r.policyConfiguration(w, []api.HTTPSRule{{Host: host, Methods: []string{"POST"}, Paths: []string{"/"}}})
		if err == nil {
			t.Fatal("repository permissions expanded", host)
		}
	}
}

func TestPreparationDiagnosticAuditIncludesMessageAndDeduplicatesRetries(t *testing.T) {
	ctx := context.Background()
	r, w, _ := fixture(t)
	message := "main branch protection needs operator review: require at least 1 approval (currently 0)"
	for i := 0; i < 2; i++ {
		if err := r.status(ctx, w, "Preparing", false, "PreparationPending", message); err != nil {
			t.Fatal(err)
		}
	}
	var cms corev1.ConfigMapList
	if err := r.List(ctx, &cms, client.InNamespace(w.Namespace)); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, cm := range cms.Items {
		if strings.HasPrefix(cm.Data["action"], "condition-") {
			count++
			if cm.Data["message"] != message || cm.Data["reason"] != "PreparationPending" {
				t.Fatal("missing diagnostic", cm.Data)
			}
		}
	}
	if count != 1 {
		t.Fatal("duplicate retry diagnostics", count)
	}
}
