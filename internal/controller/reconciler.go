package controller

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	api "git.cluster.aimingupward.com/steve/agent-project-controller/api/v1alpha1"
	"git.cluster.aimingupward.com/steve/agent-project-controller/internal/forgejo"
	"git.cluster.aimingupward.com/steve/agent-project-controller/internal/network"
	"git.cluster.aimingupward.com/steve/agent-project-controller/internal/proxy"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

var sandboxGVK = schema.GroupVersionKind{Group: "agents.x-k8s.io", Version: "v1beta1", Kind: "Sandbox"}

type Config struct {
	Namespace, ForgejoOrigin, Owner, Image, RuntimeClass, StorageClass string
	ProxyImage                                                         string
	StorageSize                                                        string
	WorkerResources, ProxyResources                                    ResourceProfile
}

func (c Config) Validate() error {
	if c.Namespace == "" || c.Owner == "" || c.RuntimeClass == "" || c.StorageClass == "" {
		return errors.New("control namespace, repository owner, runtime class and storage class required")
	}
	if q, e := resource.ParseQuantity(c.StorageSize); e != nil || q.Sign() <= 0 {
		return errors.New("invalid storage size")
	}
	if _, _, e := c.resources(); e != nil {
		return e
	}
	for _, image := range []string{c.Image, c.proxyImage()} {
		digest := strings.Split(image, "@sha256:")
		if len(digest) != 2 || digest[0] == "" || len(digest[1]) != 64 {
			return errors.New("workspace image requires a sha256 digest")
		}
		if _, e := hex.DecodeString(digest[1]); e != nil {
			return errors.New("invalid image digest")
		}
	}
	return nil
}
func (c Config) proxyImage() string {
	if c.ProxyImage != "" {
		return c.ProxyImage
	}
	return c.Image
}

type Reconciler struct {
	client.Client
	Host    forgejo.Host
	Network network.Provider
	Config  Config
}

var duration, _ = otel.Meter("agent-project-controller").Float64Histogram("workspace.reconcile.duration", metric.WithUnit("s"))
var attempts, _ = otel.Meter("agent-project-controller").Int64Counter("workspace.reconcile.attempts")

func (r *Reconciler) Setup(m ctrl.Manager) error {
	gauge, e := otel.Meter("agent-project-controller").Int64ObservableGauge("workspace.environments")
	if e != nil {
		return e
	}
	_, e = otel.Meter("agent-project-controller").RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		var list api.ProjectWorkspaceList
		if e := r.List(ctx, &list, client.InNamespace(r.Config.Namespace)); e != nil {
			return e
		}
		counts := map[string]int64{}
		for _, w := range list.Items {
			phase := w.Status.Phase
			switch phase {
			case "Ready", "Preparing", "Pausing", "Paused", "Blocked", "Removing", "Removed":
			case "":
				phase = "Requested"
			default:
				phase = "Other"
			}
			counts[phase]++
		}
		for phase, n := range counts {
			o.ObserveInt64(gauge, n, metric.WithAttributes(attribute.String("phase", phase)))
		}
		return nil
	}, gauge)
	if e != nil {
		return e
	}
	return ctrl.NewControllerManagedBy(m).For(&api.ProjectWorkspace{}).Complete(r)
}
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, err error) {
	started := time.Now()
	ctx, span := otel.Tracer("agent-project-controller").Start(ctx, "workspace.reconcile")
	defer func() {
		outcome := "success"
		if err != nil {
			outcome = "retry"
			span.SetStatus(codes.Error, "reconciliation will retry")
		}
		opts := metric.WithAttributes(attribute.String("outcome", outcome))
		duration.Record(ctx, time.Since(started).Seconds(), opts)
		attempts.Add(ctx, 1, opts)
		span.End()
	}()
	if req.Namespace != r.Config.Namespace {
		return ctrl.Result{}, nil
	}
	w := &api.ProjectWorkspace{}
	if e := r.Get(ctx, req.NamespacedName, w); e != nil {
		return ctrl.Result{}, client.IgnoreNotFound(e)
	}
	if !w.DeletionTimestamp.IsZero() || w.Spec.DesiredState == "Removed" {
		if e := r.remove(ctx, w); e != nil {
			statusErr := r.status(ctx, w, "Removing", false, "CleanupPending", e.Error())
			return ctrl.Result{RequeueAfter: 5 * time.Second}, errors.Join(e, statusErr)
		}
		if w.DeletionTimestamp.IsZero() {
			return ctrl.Result{}, r.status(ctx, w, "Removed", false, "Removed", "Access revoked and owned Kubernetes storage removed")
		}
		controllerutil.RemoveFinalizer(w, api.Finalizer)
		return ctrl.Result{}, r.Update(ctx, w)
	}
	if !controllerutil.ContainsFinalizer(w, api.Finalizer) {
		controllerutil.AddFinalizer(w, api.Finalizer)
		return ctrl.Result{Requeue: true}, r.Update(ctx, w)
	}
	if e := r.validate(ctx, w); e != nil {
		if stopErr := r.suspend(ctx, w); stopErr != nil {
			return ctrl.Result{}, stopErr
		}
		if statusErr := r.status(ctx, w, "Blocked", false, "PolicyRejected", e.Error()); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	if e := r.ensure(ctx, w); e != nil {
		stopErr := r.suspend(ctx, w)
		statusErr := r.status(ctx, w, "Preparing", false, "PreparationPending", e.Error())
		return ctrl.Result{RequeueAfter: 5 * time.Second}, errors.Join(e, stopErr, statusErr)
	}
	phase := "Preparing"
	ready := false
	reason := "PreparationPending"
	message := "Waiting for inspected clone and ready Sandbox"
	if w.Spec.DesiredState == "Paused" {
		phase = "Pausing"
		reason = "PausePending"
		message = "Waiting for project processes to terminate"
		gone, e := r.workerGone(ctx, w)
		if e != nil {
			return ctrl.Result{}, e
		}
		if gone {
			phase = "Paused"
			reason = "Paused"
			message = "Project paused; files and repository credentials retained"
		}
	}
	if w.Spec.DesiredState == "Running" {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(sandboxGVK)
		if e := r.Get(ctx, types.NamespacedName{Namespace: namespace(w), Name: "project"}, u); e != nil {
			return ctrl.Result{}, e
		}
		conditions, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
		for _, raw := range conditions {
			c, _ := raw.(map[string]any)
			if c["type"] == "Ready" && c["status"] == "True" {
				phase = "Ready"
				ready = true
				reason = "ProjectReady"
				message = "Inspected clone completed and project environment is ready"
			}
		}
	}
	return ctrl.Result{RequeueAfter: 10 * time.Second}, r.status(ctx, w, phase, ready, reason, message)
}
func namespace(w *api.ProjectWorkspace) string {
	return "apc-" + strings.ReplaceAll(string(w.UID), "-", "")
}
func identity(w *api.ProjectWorkspace) string {
	return "apc-" + strings.ReplaceAll(string(w.UID), "-", "")
}
func labels(w *api.ProjectWorkspace) map[string]string {
	return map[string]string{api.OwnerLabel: string(w.UID)}
}
func owner(w *api.ProjectWorkspace, obj client.Object) error {
	if obj.GetLabels()[api.OwnerLabel] != string(w.UID) {
		return errors.New("resource ownership mismatch; refusing adoption or deletion")
	}
	return nil
}
func (r *Reconciler) validate(ctx context.Context, w *api.ProjectWorkspace) error {
	if e := r.Config.Validate(); e != nil {
		return e
	}
	if w.UID == "" {
		return errors.New("workspace UID missing")
	}
	if e := forgejo.ValidateRepository(w.Spec.Repository, r.Config.Owner); e != nil {
		return e
	}
	if w.Spec.DesiredState != "Running" && w.Spec.DesiredState != "Paused" {
		return errors.New("unsupported desired state")
	}
	if !strings.Contains(r.Config.Image, "@sha256:") || r.Config.RuntimeClass == "" || r.Config.StorageClass == "" {
		return errors.New("digest-pinned image, isolation runtime and storage class required")
	}
	if !r.Network.Capabilities().BypassPrevention {
		return errors.New("network provider cannot prevent HTTPS inspection bypass")
	}
	if e := r.Network.Check(ctx, r.Client); e != nil {
		return e
	}
	sc := &storagev1.StorageClass{}
	if e := r.Get(ctx, types.NamespacedName{Name: r.Config.StorageClass}, sc); e != nil {
		return errors.New("storage class unavailable")
	}
	if sc.ReclaimPolicy == nil || *sc.ReclaimPolicy != corev1.PersistentVolumeReclaimDelete {
		return errors.New("storage class must reclaim Delete; retained storage cannot meet automatic removal policy")
	}
	// Runtime existence is checked; operators must qualify its actual isolation before installation.
	runtime := &unstructured.Unstructured{}
	runtime.SetGroupVersionKind(schema.GroupVersionKind{Group: "node.k8s.io", Version: "v1", Kind: "RuntimeClass"})
	if e := r.Get(ctx, types.NamespacedName{Name: r.Config.RuntimeClass}, runtime); e != nil {
		return errors.New("configured isolation runtime unavailable")
	}
	rule, e := proxy.ForgejoRule(r.Config.ForgejoOrigin, w.Spec.Repository)
	if e != nil {
		return e
	}
	return proxy.Validate(proxy.Config{Rules: append([]api.HTTPSRule{rule}, w.Spec.HTTPS...)})
}
func (r *Reconciler) audit(ctx context.Context, w *api.ProjectWorkspace, action string) error {
	name := fmt.Sprintf("audit-%s-%d-%s", strings.ReplaceAll(string(w.UID), "-", ""), w.Generation, action)
	cm := &corev1.ConfigMap{}
	key := types.NamespacedName{Namespace: r.Config.Namespace, Name: name}
	e := r.Get(ctx, key, cm)
	if e == nil {
		return owner(w, cm)
	}
	if !apierrors.IsNotFound(e) {
		return e
	}
	immutable := true
	actor := w.Annotations["projects.agentwork.dev/requested-by"]
	if actor == "" {
		actor = "kubernetes-api-client"
	}
	return r.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.Config.Namespace, Labels: labels(w)}, Immutable: &immutable, Data: map[string]string{"workspace": w.Name, "workspaceUID": string(w.UID), "generation": fmt.Sprint(w.Generation), "action": action, "actor": actor, "time": time.Now().UTC().Format(time.RFC3339)}})
}
func (r *Reconciler) status(ctx context.Context, w *api.ProjectWorkspace, phase string, ready bool, reason, message string) error {
	old := w.DeepCopyObject().(*api.ProjectWorkspace)
	if w.Status.Phase != phase {
		if e := r.audit(ctx, w, "phase-"+strings.ToLower(phase)); e != nil {
			return e
		}
		w.Status.Audit = append(w.Status.Audit, api.AuditEvent{Time: metav1.Now(), Action: phase, Generation: w.Generation})
		slog.InfoContext(ctx, "workspace phase changed", "workspace_uid", string(w.UID), "generation", w.Generation, "phase", phase, "trace_id", trace.SpanContextFromContext(ctx).TraceID().String())
		if len(w.Status.Audit) > 32 {
			w.Status.Audit = w.Status.Audit[len(w.Status.Audit)-32:]
		}
	}
	w.Status.Phase = phase
	w.Status.Namespace = namespace(w)
	w.Status.ObservedGeneration = w.Generation
	s := metav1.ConditionFalse
	if ready {
		s = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&w.Status.Conditions, metav1.Condition{Type: "Ready", Status: s, Reason: reason, Message: message, ObservedGeneration: w.Generation})
	if reflect.DeepEqual(old.Status, w.Status) {
		return nil
	}
	return r.Status().Patch(ctx, w, client.MergeFrom(old))
}
func (r *Reconciler) state(ctx context.Context, w *api.ProjectWorkspace) (*corev1.Secret, error) {
	s := &corev1.Secret{}
	key := types.NamespacedName{Namespace: r.Config.Namespace, Name: identity(w)}
	e := r.Get(ctx, key, s)
	if e == nil {
		if e = owner(w, s); e != nil {
			return nil, e
		}
		if string(s.Data["username"]) != identity(w) {
			return nil, errors.New("workspace identity ledger mismatch")
		}
		return s, nil
	}
	if !apierrors.IsNotFound(e) {
		return nil, e
	}
	secret := make([]byte, 32)
	if _, e = rand.Read(secret); e != nil {
		return nil, e
	}
	cert, pkey, e := proxy.GenerateCA()
	if e != nil {
		return nil, e
	}
	s = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: labels(w)}, Data: map[string][]byte{"username": []byte(identity(w)), "password": []byte(hex.EncodeToString(secret)), "ca.crt": cert, "ca.key": pkey}}
	// This durable record precedes any external identity or grant. Never automatically garbage collect it.
	return s, r.Create(ctx, s)
}
func credentials(s *corev1.Secret) forgejo.Credentials {
	return forgejo.Credentials{Username: string(s.Data["username"]), Password: string(s.Data["password"]), Token: string(s.Data["token"])}
}
func (r *Reconciler) ensure(ctx context.Context, w *api.ProjectWorkspace) error {
	if e := r.current(ctx, w); e != nil {
		return e
	}
	if e := r.audit(ctx, w, "grant-requested"); e != nil {
		return e
	}
	s, e := r.state(ctx, w)
	if e != nil {
		return e
	}
	beforeCA := append([]byte(nil), s.Data["ca.crt"]...)
	if e = r.rotateCA(ctx, w, s); e != nil {
		return e
	}
	token, e := r.Host.Grant(ctx, w.Spec.Repository, credentials(s))
	if e != nil {
		return e
	}
	if string(s.Data["token"]) != token {
		old := s.DeepCopy()
		s.Data["token"] = []byte(token)
		if e = r.Patch(ctx, s, client.MergeFrom(old)); e != nil {
			return e
		}
	}
	if e = r.audit(ctx, w, "access-granted"); e != nil {
		return e
	}
	if e = r.current(ctx, w); e != nil {
		return e
	}
	ns := &corev1.Namespace{}
	e = r.Get(ctx, types.NamespacedName{Name: namespace(w)}, ns)
	if apierrors.IsNotFound(e) {
		nslabels := labels(w)
		nslabels["pod-security.kubernetes.io/enforce"] = "restricted"
		nslabels["pod-security.kubernetes.io/enforce-version"] = "v1.34"
		ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace(w), Labels: nslabels}}
		if e = r.Create(ctx, ns); e != nil {
			return e
		}
	} else if e != nil {
		return e
	} else if e = owner(w, ns); e != nil {
		return e
	}
	if !ns.DeletionTimestamp.IsZero() {
		return errors.New("project namespace is terminating")
	}
	if e = r.modelAccess(ctx, w); e != nil {
		return e
	}
	if e = r.objects(ctx, w, s); e != nil {
		return e
	}
	if !bytes.Equal(beforeCA, s.Data["ca.crt"]) {
		return r.restartInspection(ctx, w)
	}
	return nil
}
func generationOf(raw []byte) (int, error) {
	if len(raw) == 0 {
		return 1, nil
	}
	n, e := strconv.Atoi(strings.TrimSpace(string(raw)))
	if e != nil || n < 1 || n > 1000 {
		return 0, errors.New("inspection CA generation is invalid")
	}
	return n, nil
}

// rotateCA advances the inspection CA only when the operator annotation increases by one.
// The private key stays in the control ledger and the proxy. Workers receive the public certificate.
func (r *Reconciler) rotateCA(ctx context.Context, w *api.ProjectWorkspace, s *corev1.Secret) error {
	have, e := generationOf(s.Data["ca-generation"])
	if e != nil {
		return e
	}
	want := have
	if raw := strings.TrimSpace(w.Annotations["projects.agentwork.dev/ca-generation"]); raw != "" {
		want, e = generationOf([]byte(raw))
		if e != nil {
			return e
		}
	}
	if want < have {
		return errors.New("inspection CA generation cannot go backwards")
	}
	if want > have+1 {
		return errors.New("inspection CA generation must increase by one")
	}
	if want == have && string(s.Data["ca-generation"]) == strconv.Itoa(have) {
		return nil
	}
	old := s.DeepCopy()
	if want > have {
		cert, key, e := proxy.GenerateCA()
		if e != nil {
			return e
		}
		s.Data["ca.crt"] = cert
		s.Data["ca.key"] = key
	}
	s.Data["ca-generation"] = []byte(strconv.Itoa(want))
	if e = r.Patch(ctx, s, client.MergeFrom(old)); e != nil {
		return e
	}
	if want > have {
		return r.audit(ctx, w, "ca-"+strconv.Itoa(want))
	}
	return nil
}
func (r *Reconciler) modelAccess(ctx context.Context, w *api.ProjectWorkspace) error {
	s := &corev1.Secret{}
	e := r.Get(ctx, types.NamespacedName{Namespace: namespace(w), Name: "model-access"}, s)
	if apierrors.IsNotFound(e) {
		return nil
	}
	if e != nil {
		return e
	}
	return RejectRefreshMaterial(s.Data)
}
func (r *Reconciler) restartInspection(ctx context.Context, w *api.ProjectWorkspace) error {
	for _, role := range []string{"worker", "proxy"} {
		var pods corev1.PodList
		if e := r.List(ctx, &pods, client.InNamespace(namespace(w)), client.MatchingLabels{api.OwnerLabel: string(w.UID), "projects.agentwork.dev/role": role}); e != nil {
			return e
		}
		for i := range pods.Items {
			if e := r.Delete(ctx, &pods.Items[i]); e != nil && !apierrors.IsNotFound(e) {
				return e
			}
		}
	}
	return nil
}
func (r *Reconciler) apply(ctx context.Context, w *api.ProjectWorkspace, obj client.Object) error {
	key := client.ObjectKeyFromObject(obj)
	existing := obj.DeepCopyObject().(client.Object)
	e := r.Get(ctx, key, existing)
	if apierrors.IsNotFound(e) {
		return r.Create(ctx, obj)
	}
	if e != nil {
		return e
	}
	if e = owner(w, existing); e != nil {
		return e
	}
	// Controller-owned immutable project configuration is written once. Reject unexpected drift instead of broad adoption.
	switch expected := obj.(type) {
	case *unstructured.Unstructured:
		current := existing.(*unstructured.Unstructured)
		want, _, _ := unstructured.NestedMap(expected.Object, "spec")
		got, _, _ := unstructured.NestedMap(current.Object, "spec")
		if expected.GetKind() == "Sandbox" {
			pod, _, _ := unstructured.NestedMap(got, "podTemplate", "spec")
			var spec corev1.PodSpec
			encoded, e := json.Marshal(pod)
			if e != nil || json.Unmarshal(encoded, &spec) != nil || !safePod(spec) {
				return errors.New("unsafe Sandbox pod configuration drift detected")
			}
			gotCopy, _ := cloneValue(got).(map[string]any)
			wantCopy, _ := cloneValue(want).(map[string]any)
			normalizeSandboxNameservers(gotCopy)
			normalizeSandboxNameservers(wantCopy)
			gotCopy["operatingMode"] = want["operatingMode"]
			if !contains(gotCopy, wantCopy) {
				return errors.New("Sandbox configuration drift detected")
			}
			gotNS, _, e := unstructured.NestedStringSlice(got, "podTemplate", "spec", "dnsConfig", "nameservers")
			if e != nil {
				return e
			}
			wantNS, foundNS, e := unstructured.NestedStringSlice(want, "podTemplate", "spec", "dnsConfig", "nameservers")
			if e != nil || !foundNS || len(wantNS) == 0 {
				return errors.New("Sandbox DNS nameservers missing")
			}
			dnsChanged := !reflect.DeepEqual(gotNS, wantNS)
			mode, _, _ := unstructured.NestedString(current.Object, "spec", "operatingMode")
			modeChanged := mode != fmt.Sprint(want["operatingMode"])
			if dnsChanged || modeChanged {
				old := current.DeepCopy()
				if modeChanged {
					_ = unstructured.SetNestedField(current.Object, want["operatingMode"], "spec", "operatingMode")
				}
				if dnsChanged {
					values := make([]any, len(wantNS))
					for i := range wantNS {
						values[i] = wantNS[i]
					}
					if e = unstructured.SetNestedSlice(current.Object, values, "spec", "podTemplate", "spec", "dnsConfig", "nameservers"); e != nil {
						return e
					}
				}
				if e = r.Patch(ctx, current, client.MergeFrom(old)); e != nil {
					return e
				}
			}
			// Restart a worker only when its resolver differs. The claim, namespace and Forgejo identity stay.
			// A retry after a crash still sees the stale pod even if the template patch already landed.
			restarted, e := r.restartStaleDNSWorkers(ctx, w, wantNS)
			if e != nil {
				return e
			}
			if !dnsChanged && !restarted {
				return nil
			}
			sum := sha256.Sum256([]byte(strings.Join(wantNS, ",")))
			return r.audit(ctx, w, "dns-"+hex.EncodeToString(sum[:6]))
		}
		if !same(got, want) {
			return errors.New("network policy drift detected")
		}
	case *corev1.Secret:
		current := existing.(*corev1.Secret)
		if !reflect.DeepEqual(current.Data, expected.Data) {
			old := current.DeepCopy()
			current.Data = expected.Data
			return r.Patch(ctx, current, client.MergeFrom(old))
		}
	case *corev1.ConfigMap:
		current := existing.(*corev1.ConfigMap)
		if current.Name == "trust" {
			want := expected.Data["ca.crt"]
			if want == "" || len(expected.Data) != 1 || len(current.Data) != 1 || current.Data["ca.crt"] == "" {
				return errors.New("project configuration drift detected")
			}
			if current.Data["ca.crt"] != want {
				old := current.DeepCopy()
				current.Data = map[string]string{"ca.crt": want}
				return r.Patch(ctx, current, client.MergeFrom(old))
			}
			return nil
		}
		if !reflect.DeepEqual(current.Data, expected.Data) {
			return errors.New("project configuration drift detected")
		}
	case *appsv1.Deployment:
		current := existing.(*appsv1.Deployment)
		if !safePod(current.Spec.Template.Spec) {
			return errors.New("proxy deployment drift detected")
		}
		gotSpec := current.Spec.DeepCopy()
		wantSpec := expected.Spec.DeepCopy()
		normalizeProxyDrift(gotSpec)
		normalizeProxyDrift(wantSpec)
		if !contains(*gotSpec, *wantSpec) || current.Spec.Template.Spec.DNSConfig == nil || expected.Spec.Template.Spec.DNSConfig == nil {
			return errors.New("proxy deployment drift detected")
		}
		wantImage, ok := proxyContainerImage(expected.Spec)
		gotImage, gotOK := proxyContainerImage(current.Spec)
		if !ok || !gotOK {
			return errors.New("proxy deployment drift detected")
		}
		dnsChanged := !reflect.DeepEqual(current.Spec.Template.Spec.DNSConfig.Nameservers, expected.Spec.Template.Spec.DNSConfig.Nameservers)
		imageChanged := gotImage != wantImage
		if dnsChanged || imageChanged {
			old := current.DeepCopy()
			if dnsChanged {
				current.Spec.Template.Spec.DNSConfig.Nameservers = append([]string{}, expected.Spec.Template.Spec.DNSConfig.Nameservers...)
			}
			if imageChanged {
				for i := range current.Spec.Template.Spec.Containers {
					if current.Spec.Template.Spec.Containers[i].Name == "proxy" {
						current.Spec.Template.Spec.Containers[i].Image = wantImage
					}
				}
			}
			if e = r.Patch(ctx, current, client.MergeFrom(old)); e != nil {
				return e
			}
		}
	case *corev1.ServiceAccount:
		actual := existing.(*corev1.ServiceAccount)
		if actual.AutomountServiceAccountToken == nil || *actual.AutomountServiceAccountToken {
			return errors.New("service account token mounting enabled unexpectedly")
		}
	case *corev1.PersistentVolumeClaim:
		actual := existing.(*corev1.PersistentVolumeClaim)
		if !contains(actual.Spec, expected.Spec) {
			return errors.New("project storage configuration drift detected")
		}
		if actual.Spec.VolumeName != "" {
			var pv corev1.PersistentVolume
			if e = r.Get(ctx, types.NamespacedName{Name: actual.Spec.VolumeName}, &pv); e != nil {
				return e
			}
			if pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimDelete {
				return errors.New("bound project volume must reclaim Delete")
			}
		}
	case *corev1.Service:
		actual := existing.(*corev1.Service)
		if actual.Spec.ClusterIP != expected.Spec.ClusterIP || !contains(actual.Spec.Selector, expected.Spec.Selector) || !contains(actual.Spec.Ports, expected.Spec.Ports) || actual.Spec.Type != corev1.ServiceTypeClusterIP && actual.Spec.Type != "" {
			return errors.New("proxy service drift detected")
		}
	}
	return nil
}
func cloneValue(v any) any {
	encoded, e := json.Marshal(v)
	if e != nil {
		return nil
	}
	var out any
	if json.Unmarshal(encoded, &out) != nil {
		return nil
	}
	return out
}
func normalizeSandboxNameservers(spec map[string]any) {
	_ = unstructured.SetNestedStringSlice(spec, []string{"normalized"}, "podTemplate", "spec", "dnsConfig", "nameservers")
}
func normalizeProxyDrift(spec *appsv1.DeploymentSpec) {
	if spec.Template.Spec.DNSConfig != nil {
		spec.Template.Spec.DNSConfig.Nameservers = []string{"normalized"}
	}
	for i := range spec.Template.Spec.Containers {
		if spec.Template.Spec.Containers[i].Name == "proxy" {
			spec.Template.Spec.Containers[i].Image = "normalized"
		}
	}
}
func proxyContainerImage(spec appsv1.DeploymentSpec) (string, bool) {
	for _, c := range spec.Template.Spec.Containers {
		if c.Name == "proxy" && c.Image != "" {
			return c.Image, true
		}
	}
	return "", false
}
func (r *Reconciler) restartStaleDNSWorkers(ctx context.Context, w *api.ProjectWorkspace, want []string) (bool, error) {
	var pods corev1.PodList
	if e := r.List(ctx, &pods, client.InNamespace(namespace(w)), client.MatchingLabels{api.OwnerLabel: string(w.UID), "projects.agentwork.dev/role": "worker"}); e != nil {
		return false, e
	}
	restarted := false
	for i := range pods.Items {
		got := []string{}
		if pods.Items[i].Spec.DNSConfig != nil {
			got = pods.Items[i].Spec.DNSConfig.Nameservers
		}
		if reflect.DeepEqual(got, want) {
			continue
		}
		if e := r.Delete(ctx, &pods.Items[i]); e != nil && !apierrors.IsNotFound(e) {
			return restarted, e
		}
		restarted = true
	}
	return restarted, nil
}
func (r *Reconciler) workerGone(ctx context.Context, w *api.ProjectWorkspace) (bool, error) {
	var pods corev1.PodList
	if e := r.List(ctx, &pods, client.InNamespace(namespace(w)), client.MatchingLabels{api.OwnerLabel: string(w.UID), "projects.agentwork.dev/role": "worker"}); e != nil {
		return false, e
	}
	return len(pods.Items) == 0, nil
}
func (r *Reconciler) current(ctx context.Context, w *api.ProjectWorkspace) error {
	now := &api.ProjectWorkspace{}
	if e := r.Get(ctx, client.ObjectKeyFromObject(w), now); e != nil {
		return e
	}
	if now.UID != w.UID || now.Generation != w.Generation || now.Spec.DesiredState != w.Spec.DesiredState || !now.DeletionTimestamp.IsZero() {
		return errors.New("workspace request changed; reconciliation will restart")
	}
	return nil
}
func (r *Reconciler) suspend(ctx context.Context, w *api.ProjectWorkspace) error {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(sandboxGVK)
	e := r.Get(ctx, types.NamespacedName{Namespace: namespace(w), Name: "project"}, u)
	if apierrors.IsNotFound(e) {
		return nil
	}
	if e != nil {
		return e
	}
	if e = owner(w, u); e != nil {
		return e
	}
	old := u.DeepCopy()
	_ = unstructured.SetNestedField(u.Object, "Suspended", "spec", "operatingMode")
	return r.Patch(ctx, u, client.MergeFrom(old))
}
func (r *Reconciler) stop(ctx context.Context, w *api.ProjectWorkspace) error {
	// Terminate local execution even if Forgejo is unavailable. Keep retrying external revocation independently.
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(sandboxGVK)
	key := types.NamespacedName{Namespace: namespace(w), Name: "project"}
	e := r.Get(ctx, key, u)
	if e == nil {
		if e = owner(w, u); e != nil {
			return e
		}
		old := u.DeepCopy()
		_ = unstructured.SetNestedField(u.Object, "Suspended", "spec", "operatingMode")
		if e = r.Patch(ctx, u, client.MergeFrom(old)); e != nil {
			return e
		}
	} else if !apierrors.IsNotFound(e) {
		return e
	}
	var pods corev1.PodList
	if e = r.List(ctx, &pods, client.InNamespace(namespace(w)), client.MatchingLabels{api.OwnerLabel: string(w.UID)}); e != nil {
		return e
	}
	for i := range pods.Items {
		if e = r.Delete(ctx, &pods.Items[i]); e != nil && !apierrors.IsNotFound(e) {
			return e
		}
	}
	return nil
}
func (r *Reconciler) remove(ctx context.Context, w *api.ProjectWorkspace) error {
	if e := r.audit(ctx, w, "remove-requested"); e != nil {
		return e
	}
	stopErr := r.stop(ctx, w)
	s := &corev1.Secret{}
	e := r.Get(ctx, types.NamespacedName{Namespace: r.Config.Namespace, Name: identity(w)}, s)
	if e == nil {
		if e = owner(w, s); e != nil {
			return e
		}
		if string(s.Data["username"]) != identity(w) {
			return errors.New("workspace identity ledger mismatch")
		}
		if string(s.Data["revoked"]) != "true" {
			if e = r.Host.Revoke(ctx, w.Spec.Repository, credentials(s)); e != nil {
				return e
			}
		}
	} else if !apierrors.IsNotFound(e) {
		return e
	} else { // Missing ledger is safe only when no identity was ever created.
		cm := &corev1.ConfigMap{}
		auditName := fmt.Sprintf("audit-%s-%d-access-granted", strings.ReplaceAll(string(w.UID), "-", ""), w.Generation)
		if e = r.Get(ctx, types.NamespacedName{Namespace: r.Config.Namespace, Name: auditName}, cm); e == nil {
			return errors.New("credential ledger missing; operator recovery required")
		}
		// Check all generations, since a pause changes generation.
		var records corev1.ConfigMapList
		if e = r.List(ctx, &records, client.InNamespace(r.Config.Namespace), client.MatchingLabels{api.OwnerLabel: string(w.UID)}); e != nil {
			return e
		}
		for _, record := range records.Items {
			if record.Data["action"] == "access-granted" || record.Data["action"] == "grant-requested" {
				return errors.New("credential ledger missing; operator recovery required")
			}
		}
	}
	if e = r.audit(ctx, w, "access-revoked"); e != nil {
		return e
	}
	if stopErr != nil {
		return stopErr
	}
	ns := &corev1.Namespace{}
	e = r.Get(ctx, types.NamespacedName{Name: namespace(w)}, ns)
	if e == nil {
		if e = owner(w, ns); e != nil {
			return e
		}
		if ns.DeletionTimestamp.IsZero() {
			if e = r.Delete(ctx, ns); e != nil {
				return e
			}
		}
		return errors.New("waiting for namespace and processes to terminate")
	}
	if !apierrors.IsNotFound(e) {
		return e
	}
	var pvs corev1.PersistentVolumeList
	if e = r.List(ctx, &pvs); e != nil {
		return e
	}
	for _, pv := range pvs.Items {
		if pv.Spec.ClaimRef != nil && pv.Spec.ClaimRef.Namespace == namespace(w) {
			return errors.New("waiting for storage provisioner to delete project volume")
		}
	}
	// State is kept until the finalizer is removed. A crash cannot lose the revocation identity.
	if e = r.audit(ctx, w, "cleanup-verified"); e != nil {
		return e
	}
	if s.Name != "" && string(s.Data["revoked"]) != "true" {
		old := s.DeepCopy()
		s.Data = map[string][]byte{"revoked": []byte("true"), "username": s.Data["username"]}
		if e = r.Patch(ctx, s, client.MergeFrom(old)); e != nil {
			return e
		}
	}
	return nil
}
func metadata(w *api.ProjectWorkspace, name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: namespace(w), Labels: labels(w)}
}
func int64p(i int64) *int64            { return &i }
func boolp(b bool) *bool               { return &b }
func int32p(i int32) *int32            { return &i }
func fromInt(i int) intstr.IntOrString { return intstr.FromInt(i) }
func contains(got, want any) bool {
	// Normalize numeric representations from typed JSON and dynamic Kubernetes clients.
	norm := func(v any) any { b, _ := json.Marshal(v); var o any; _ = json.Unmarshal(b, &o); return o }
	return subset(norm(got), norm(want))
}
func same(got, want any) bool {
	return contains(got, want) && contains(want, got)
}
func safePod(p corev1.PodSpec) bool {
	if p.HostNetwork || p.HostPID || p.HostIPC || len(p.EphemeralContainers) != 0 || p.ShareProcessNamespace != nil && *p.ShareProcessNamespace {
		return false
	}
	if p.SecurityContext != nil && len(p.SecurityContext.Sysctls) != 0 {
		return false
	}
	containers := append(append([]corev1.Container{}, p.InitContainers...), p.Containers...)
	for _, c := range containers {
		if c.SecurityContext == nil {
			return false
		}
		s := c.SecurityContext
		if s.Capabilities != nil && len(s.Capabilities.Add) != 0 || s.ProcMount != nil && *s.ProcMount != corev1.DefaultProcMount {
			return false
		}
	}
	return true
}
func subset(got, want any) bool {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return false
		}
		for k, v := range w {
			if !subset(g[k], v) {
				return false
			}
		}
		return true
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return false
		}
		for i := range w {
			if !subset(g[i], w[i]) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(got, want)
	}
}
func (r *Reconciler) objects(ctx context.Context, w *api.ProjectWorkspace, s *corev1.Secret) error {
	ns := namespace(w)
	workerResources, proxyResources, e := r.Config.resources()
	if e != nil {
		return e
	}
	dns, e := r.Network.DNS(ctx, r.Client, ns)
	if e != nil {
		return e
	}
	rule, e := proxy.ForgejoRule(r.Config.ForgejoOrigin, w.Spec.Repository)
	if e != nil {
		return e
	}
	cfg := proxy.Config{Rules: append([]api.HTTPSRule{rule}, w.Spec.HTTPS...), PrivateHosts: []string{rule.Host}}
	raw, e := json.Marshal(cfg)
	if e != nil {
		return e
	}
	hosts := []string{}
	for _, r := range cfg.Rules {
		hosts = append(hosts, r.Host)
	}
	objects := []client.Object{}
	for _, p := range r.Network.Objects(ns, string(w.UID), hosts) {
		objects = append(objects, p)
	}
	objects = append(objects, &corev1.ServiceAccount{ObjectMeta: metadata(w, "worker"), AutomountServiceAccountToken: boolp(false)},
		&corev1.Secret{ObjectMeta: metadata(w, "git-identity"), Data: map[string][]byte{"username": s.Data["username"], "token": s.Data["token"]}},
		&corev1.Secret{ObjectMeta: metadata(w, "proxy-ca"), Data: map[string][]byte{"ca.crt": s.Data["ca.crt"], "ca.key": s.Data["ca.key"]}},
		&corev1.ConfigMap{ObjectMeta: metadata(w, "trust"), Data: map[string]string{"ca.crt": string(s.Data["ca.crt"])}},
		&corev1.ConfigMap{ObjectMeta: metadata(w, "proxy-policy"), Data: map[string]string{"policy.json": string(raw)}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metadata(w, "workspace"), Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: &r.Config.StorageClass, AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(r.Config.StorageSize)}}}},
		&corev1.Service{ObjectMeta: metadata(w, "proxy"), Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, ClusterIP: corev1.ClusterIPNone, Selector: map[string]string{api.OwnerLabel: string(w.UID), "projects.agentwork.dev/role": "proxy"}, Ports: []corev1.ServicePort{{Port: 3128, TargetPort: fromInt(3128), Protocol: corev1.ProtocolTCP}}}})
	security := &corev1.SecurityContext{Privileged: boolp(false), RunAsNonRoot: boolp(true), RunAsUser: int64p(1000), RunAsGroup: int64p(1000), AllowPrivilegeEscalation: boolp(false), ReadOnlyRootFilesystem: boolp(true), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}, SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}}
	plabels := labels(w)
	plabels["projects.agentwork.dev/role"] = "proxy"
	proxyDeployment := &appsv1.Deployment{ObjectMeta: metadata(w, "proxy"), Spec: appsv1.DeploymentSpec{Replicas: int32p(1), Selector: &metav1.LabelSelector{MatchLabels: plabels}, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: plabels}, Spec: corev1.PodSpec{DNSPolicy: corev1.DNSNone, DNSConfig: dns, AutomountServiceAccountToken: boolp(false), ServiceAccountName: "worker", TerminationGracePeriodSeconds: int64p(1), SecurityContext: &corev1.PodSecurityContext{FSGroup: int64p(1000)}, Containers: []corev1.Container{{Name: "proxy", Image: r.Config.proxyImage(), Args: []string{"proxy"}, SecurityContext: security, Resources: proxyResources, Ports: []corev1.ContainerPort{{ContainerPort: 3128}}, ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: fromInt(3128)}}}, VolumeMounts: []corev1.VolumeMount{{Name: "tls", MountPath: "/tls", ReadOnly: true}, {Name: "policy", MountPath: "/policy", ReadOnly: true}}}}, Volumes: []corev1.Volume{{Name: "tls", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "proxy-ca", DefaultMode: int32p(0440)}}}, {Name: "policy", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "proxy-policy"}}}}}}}}}
	objects = append(objects, proxyDeployment)
	u, _ := url.Parse(r.Config.ForgejoOrigin)
	env := []corev1.EnvVar{{Name: "HOME", Value: "/workspace/home"}, {Name: "GIT_ORIGIN", Value: r.Config.ForgejoOrigin}, {Name: "GIT_HOST", Value: u.Host}, {Name: "GIT_REPOSITORY", Value: w.Spec.Repository}, {Name: "GIT_WORK_BRANCH", Value: "agent/" + identity(w)}, {Name: "GIT_TERMINAL_PROMPT", Value: "0"}, {Name: "HTTPS_PROXY", Value: "http://proxy:3128"}, {Name: "https_proxy", Value: "http://proxy:3128"}, {Name: "HTTP_PROXY", Value: "http://proxy:3128"}, {Name: "http_proxy", Value: "http://proxy:3128"}, {Name: "SSL_CERT_FILE", Value: "/workspace/home/ca-bundle.crt"}, {Name: "NODE_EXTRA_CA_CERTS", Value: "/trust/ca.crt"}, {Name: "REQUESTS_CA_BUNDLE", Value: "/workspace/home/ca-bundle.crt"}}
	mounts := []corev1.VolumeMount{{Name: "workspace", MountPath: "/workspace"}, {Name: "identity", MountPath: "/identity", ReadOnly: true}, {Name: "trust", MountPath: "/trust", ReadOnly: true}, {Name: "model", MountPath: "/run/model", ReadOnly: true}, {Name: "scratch", MountPath: "/tmp"}}
	workerlabels := labels(w)
	workerlabels["projects.agentwork.dev/role"] = "worker"
	spec := corev1.PodSpec{DNSPolicy: corev1.DNSNone, DNSConfig: dns, RuntimeClassName: &r.Config.RuntimeClass, AutomountServiceAccountToken: boolp(false), ServiceAccountName: "worker", TerminationGracePeriodSeconds: int64p(5), SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: boolp(true), RunAsUser: int64p(1000), RunAsGroup: int64p(1000), FSGroup: int64p(1000)}, InitContainers: []corev1.Container{{Name: "clone", Image: r.Config.Image, Args: []string{"bootstrap"}, Env: env, SecurityContext: security, Resources: workerResources, VolumeMounts: mounts}}, Containers: []corev1.Container{{Name: "workspace", Image: r.Config.Image, Command: []string{"/bin/sh", "-c", "trap 'exit 0' TERM INT; while :; do sleep 3600 & wait $!; done"}, Env: env, WorkingDir: "/workspace/repo", SecurityContext: security, Resources: workerResources, VolumeMounts: mounts, ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"test", "-f", "/workspace/.ready"}}}}}}, Volumes: []corev1.Volume{{Name: "workspace", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "workspace"}}}, {Name: "identity", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "git-identity", DefaultMode: int32p(0440)}}}, {Name: "trust", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "trust"}}}}, {Name: "model", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "model-access", Optional: boolp(true), DefaultMode: int32p(0440)}}}, {Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}}
	b, e := json.Marshal(spec)
	if e != nil {
		return e
	}
	var podspec map[string]any
	if e = json.Unmarshal(b, &podspec); e != nil {
		return e
	}
	sandbox := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "agents.x-k8s.io/v1beta1", "kind": "Sandbox", "metadata": map[string]any{"name": "project", "namespace": ns, "labels": map[string]any{api.OwnerLabel: string(w.UID)}}, "spec": map[string]any{"operatingMode": w.Spec.DesiredState, "podTemplate": map[string]any{"metadata": map[string]any{"labels": stringMap(workerlabels)}, "spec": podspec}}}}
	if w.Spec.DesiredState == "Paused" {
		_ = unstructured.SetNestedField(sandbox.Object, "Suspended", "spec", "operatingMode")
	}
	objects = append(objects, sandbox)
	for _, obj := range objects {
		if e = r.apply(ctx, w, obj); e != nil {
			return e
		}
	}
	return nil
}
func stringMap(m map[string]string) map[string]any {
	o := map[string]any{}
	for k, v := range m {
		o[k] = v
	}
	return o
}
