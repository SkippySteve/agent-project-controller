package controller

import (
	"errors"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// ResourceProfile is operator configuration, not an untrusted project request.
type ResourceProfile struct {
	CPURequest, MemoryRequest, CPULimit, MemoryLimit string
}

func (p ResourceProfile) requirements(defaults ResourceProfile) (corev1.ResourceRequirements, error) {
	values := []string{p.CPURequest, p.MemoryRequest, p.CPULimit, p.MemoryLimit}
	fallback := []string{defaults.CPURequest, defaults.MemoryRequest, defaults.CPULimit, defaults.MemoryLimit}
	quantities := make([]resource.Quantity, 4)
	for i, value := range values {
		if value == "" {
			value = fallback[i]
		}
		q, e := resource.ParseQuantity(value)
		if e != nil || q.Sign() <= 0 {
			return corev1.ResourceRequirements{}, errors.New("resource requests and limits must be positive quantities")
		}
		quantities[i] = q
	}
	if quantities[0].Cmp(quantities[2]) > 0 || quantities[1].Cmp(quantities[3]) > 0 {
		return corev1.ResourceRequirements{}, errors.New("resource requests must not exceed limits")
	}
	return corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: quantities[0], corev1.ResourceMemory: quantities[1]}, Limits: corev1.ResourceList{corev1.ResourceCPU: quantities[2], corev1.ResourceMemory: quantities[3]}}, nil
}

func (c Config) resources() (corev1.ResourceRequirements, corev1.ResourceRequirements, error) {
	worker, e := c.WorkerResources.requirements(ResourceProfile{"500m", "512Mi", "4", "4Gi"})
	if e != nil {
		return worker, corev1.ResourceRequirements{}, e
	}
	proxy, e := c.ProxyResources.requirements(ResourceProfile{"100m", "128Mi", "1", "512Mi"})
	return worker, proxy, e
}
