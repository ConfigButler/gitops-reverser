// SPDX-License-Identifier: Apache-2.0

package sanitize

import (
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// PartialObjectMeta defines the subset of ObjectMeta fields we preserve
// in GitOps storage. This explicitly documents our sanitization policy.
type PartialObjectMeta struct {
	Name        string            `json:"name,omitempty"        yaml:"name,omitempty"`
	Namespace   string            `json:"namespace,omitempty"   yaml:"namespace,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"      yaml:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty" yaml:"annotations,omitempty"`
}

// FromUnstructured extracts PartialObjectMeta from an unstructured object.
func (p *PartialObjectMeta) FromUnstructured(obj *unstructured.Unstructured) {
	p.Name = obj.GetName()
	p.Namespace = obj.GetNamespace()
	p.Labels = cleanLabels(obj.GetLabels())
	p.Annotations = cleanAnnotations(obj.GetAnnotations())
}

// cleanLabels removes operational labels that should not be persisted.
func cleanLabels(labels map[string]string) map[string]string {
	if labels == nil {
		return nil
	}

	cleaned := make(map[string]string)
	for k, v := range labels {
		if isOperationalLabel(k) {
			continue
		}
		cleaned[k] = v
	}

	if len(cleaned) == 0 {
		return nil
	}
	return cleaned
}

// cleanAnnotations removes operational annotations.
// Adapted from Kyverno's approach for cleaning system-managed annotations.
func cleanAnnotations(annotations map[string]string) map[string]string {
	if annotations == nil {
		return nil
	}

	cleaned := make(map[string]string)
	for k, v := range annotations {
		if isOperationalAnnotation(k) {
			continue
		}
		cleaned[k] = v
	}

	if len(cleaned) == 0 {
		return nil
	}
	return cleaned
}

func isOperationalLabel(key string) bool {
	return strings.HasPrefix(key, "kustomize.toolkit.fluxcd.io/") ||
		strings.HasPrefix(key, "kro.run/") ||
		strings.HasPrefix(key, "applyset.kubernetes.io/")
}

func isOperationalAnnotation(key string) bool {
	return strings.HasPrefix(key, "kubectl.kubernetes.io/") ||
		strings.HasPrefix(key, "control-plane.alpha.kubernetes.io/") ||
		strings.HasPrefix(key, "deployment.kubernetes.io/") ||
		strings.HasPrefix(key, "autoscaling.alpha.kubernetes.io/") ||
		strings.HasPrefix(key, "kustomize.toolkit.fluxcd.io/") ||
		strings.HasPrefix(key, "applyset.kubernetes.io/")
}
