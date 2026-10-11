package v1alpha1

import (
	"encoding/json"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var GroupVersion = schema.GroupVersion{Group: "projects.agentwork.dev", Version: "v1alpha1"}

const Finalizer = "projects.agentwork.dev/cleanup"
const OwnerLabel = "projects.agentwork.dev/workspace-uid"

type HTTPSRule struct {
	Host    string   `json:"host"`
	Methods []string `json:"methods"`
	Paths   []string `json:"paths"` // Literal prefixes, not regular expressions.
}
type WorkspaceSpec struct {
	Repository   string      `json:"repository"`
	DesiredState string      `json:"desiredState"`
	HTTPS        []HTTPSRule `json:"https,omitempty"`
}
type AuditEvent struct {
	Time       metav1.Time `json:"time"`
	Action     string      `json:"action"`
	Generation int64       `json:"generation"`
}
type WorkspaceStatus struct {
	Phase              string             `json:"phase,omitempty"`
	Namespace          string             `json:"namespace,omitempty"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
	Audit              []AuditEvent       `json:"audit,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
type ProjectWorkspace struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              WorkspaceSpec   `json:"spec"`
	Status            WorkspaceStatus `json:"status,omitempty"`
}
type ProjectWorkspaceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ProjectWorkspace `json:"items"`
}

func (w *ProjectWorkspace) DeepCopyObject() runtime.Object {
	out := new(ProjectWorkspace)
	b, _ := json.Marshal(w)
	_ = json.Unmarshal(b, out)
	return out
}
func (w *ProjectWorkspaceList) DeepCopyObject() runtime.Object {
	out := new(ProjectWorkspaceList)
	b, _ := json.Marshal(w)
	_ = json.Unmarshal(b, out)
	return out
}
func AddToScheme(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &ProjectWorkspace{}, &ProjectWorkspaceList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
