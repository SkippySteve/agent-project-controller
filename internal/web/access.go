package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"

	api "git.cluster.aimingupward.com/steve/agent-project-controller/api/v1alpha1"
	"git.cluster.aimingupward.com/steve/agent-project-controller/internal/controller"
	"git.cluster.aimingupward.com/steve/agent-project-controller/internal/proxy"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var errAccessRules = errors.New("duplicate or reserved HTTPS host")

func stringUID(obj *api.ProjectWorkspace) string { return strings.ReplaceAll(string(obj.UID), "-", "") }

func (s *Server) accessRules(repository string, registries []string, https []api.HTTPSRule) ([]api.HTTPSRule, error) {
	registry, err := proxy.RegistryRules(registries)
	if err != nil {
		return nil, err
	}
	rules := append(registry, https...)
	reserved := map[string]bool{}
	var required []api.HTTPSRule
	if s.GitOrigin != "" {
		gitRules, _, err := controller.AccessRules(s.GitHost, s.GitOrigin, repository)
		if err != nil {
			return nil, err
		}
		for _, rule := range gitRules {
			reserved[rule.Host] = true
		}
		required = gitRules
	}
	seen := map[string]bool{}
	for _, rule := range rules {
		if seen[rule.Host] || reserved[rule.Host] {
			return nil, errAccessRules
		}
		seen[rule.Host] = true
	}
	if len(rules) > 63 {
		return nil, errAccessRules
	}
	if len(rules) > 0 || len(required) > 0 {
		if err := proxy.Validate(proxy.Config{Rules: append(required, rules...)}); err != nil {
			return nil, err
		}
	}
	return rules, nil
}

func (s *Server) changeAccess(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ResourceVersion string           `json:"resourceVersion"`
		HTTPS           *[]api.HTTPSRule `json:"https"`
		Registries      []string         `json:"registries,omitempty"`
	}
	if decode(w, r, &request) != nil || request.ResourceVersion == "" || request.HTTPS == nil {
		http.Error(w, "HTTPS policy and resourceVersion required", 400)
		return
	}
	obj := &api.ProjectWorkspace{}
	if err := s.Client.Get(r.Context(), types.NamespacedName{Namespace: s.Namespace, Name: r.PathValue("name")}, obj); err != nil {
		http.Error(w, "project unavailable", 404)
		return
	}
	if obj.ResourceVersion != request.ResourceVersion {
		http.Error(w, "project changed; refresh before retrying", 409)
		return
	}
	if !obj.DeletionTimestamp.IsZero() || obj.Spec.DesiredState != "Paused" || obj.Status.Phase != "Paused" || obj.Status.ObservedGeneration != obj.Generation {
		http.Error(w, "pause the project and wait for Paused before editing access", 409)
		return
	}
	rules, err := s.accessRules(obj.Spec.Repository, request.Registries, *request.HTTPS)
	if err != nil {
		http.Error(w, "invalid HTTPS policy; use unique exact hosts, supported methods and literal path prefixes, excluding repository hosts", 400)
		return
	}
	var pods corev1.PodList
	if err := s.Client.List(r.Context(), &pods, client.InNamespace(obj.Status.Namespace), client.MatchingLabels{api.OwnerLabel: string(obj.UID)}); err != nil {
		http.Error(w, "cannot verify stopped project processes", 503)
		return
	}
	if obj.Status.Namespace == "" || len(pods.Items) != 0 {
		http.Error(w, "waiting for all project processes to stop", 409)
		return
	}
	if !s.accessApplied(r, obj) {
		http.Error(w, "previous access policy has not finished reconciling", 409)
		return
	}
	obj.Spec.HTTPS = rules
	if obj.Annotations == nil {
		obj.Annotations = map[string]string{}
	}
	actor, _ := r.Context().Value(actorKey{}).(string)
	obj.Annotations["projects.agentwork.dev/requested-by"] = actor
	if err := s.Client.Update(r.Context(), obj); err != nil {
		http.Error(w, "access update failed; refresh before retrying", 409)
		return
	}
	writeJSON(w, obj)
}

func (s *Server) accessApplied(r *http.Request, obj *api.ProjectWorkspace) bool {
	ledger := &corev1.Secret{}
	if err := s.Client.Get(r.Context(), types.NamespacedName{Namespace: s.Namespace, Name: "apc-" + stringUID(obj)}, ledger); err != nil {
		return false
	}
	if ledger.Labels[api.OwnerLabel] != string(obj.UID) {
		return false
	}
	var applied []api.HTTPSRule
	if json.Unmarshal(ledger.Data[controller.HTTPSLedgerKey], &applied) != nil {
		return false
	}
	// Omitted and empty rules mean the same deny-by-default optional policy.
	return len(applied) == 0 && len(obj.Spec.HTTPS) == 0 || reflect.DeepEqual(applied, obj.Spec.HTTPS)
}
