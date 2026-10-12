package controller

import (
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// Scheduling permits placement changes without accepting arbitrary PodSpec overrides.
type Scheduling struct {
	NodeSelector map[string]string   `json:"nodeSelector,omitempty"`
	Tolerations  []corev1.Toleration `json:"tolerations,omitempty"`
	Affinity     *corev1.Affinity    `json:"affinity,omitempty"`
}

func ParseScheduling(raw string) (Scheduling, error) {
	var s Scheduling
	if raw == "" {
		return s, nil
	}
	if !strings.HasPrefix(strings.TrimSpace(raw), "{") {
		return s, errors.New("scheduling must be an object")
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&s); err != nil {
		return s, errors.New("invalid scheduling configuration")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return s, errors.New("invalid trailing scheduling configuration")
	}
	if len(s.NodeSelector) == 0 {
		s.NodeSelector = nil
	}
	if len(s.Tolerations) == 0 {
		s.Tolerations = nil
	}
	if s.Affinity != nil && reflect.DeepEqual(*s.Affinity, corev1.Affinity{}) {
		s.Affinity = nil
	}
	return s, s.Validate()
}

func (s Scheduling) Validate() error {
	for key, value := range s.NodeSelector {
		if len(validation.IsQualifiedName(key)) != 0 || len(validation.IsValidLabelValue(value)) != 0 {
			return errors.New("invalid scheduling node selector")
		}
	}
	for _, t := range s.Tolerations {
		if t.Key != "" && len(validation.IsQualifiedName(t.Key)) != 0 {
			return errors.New("invalid toleration key")
		}
		if t.Operator != "" && t.Operator != corev1.TolerationOpEqual && t.Operator != corev1.TolerationOpExists {
			return errors.New("invalid toleration operator")
		}
		if t.Operator == corev1.TolerationOpExists && t.Value != "" {
			return errors.New("Exists toleration cannot have a value")
		}
		if t.Operator != corev1.TolerationOpExists && (t.Key == "" || len(validation.IsValidLabelValue(t.Value)) != 0) {
			return errors.New("invalid Equal toleration")
		}
		if t.Effect != "" && t.Effect != corev1.TaintEffectNoSchedule && t.Effect != corev1.TaintEffectPreferNoSchedule && t.Effect != corev1.TaintEffectNoExecute {
			return errors.New("invalid toleration effect")
		}
		if t.TolerationSeconds != nil && (t.Effect != corev1.TaintEffectNoExecute || *t.TolerationSeconds < 0) {
			return errors.New("invalid toleration duration")
		}
	}
	return nil
}

func (s Scheduling) Apply(spec *corev1.PodSpec) {
	spec.NodeSelector = s.NodeSelector
	spec.Tolerations = s.Tolerations
	spec.Affinity = s.Affinity
}
