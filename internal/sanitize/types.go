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
	p.Labels = obj.GetLabels()
	p.Annotations = cleanAnnotations(obj.GetAnnotations())
}

// cleanAnnotations removes operational annotations.
// Adapted from Kyverno's approach for cleaning system-managed annotations.
func cleanAnnotations(annotations map[string]string) map[string]string {
	if annotations == nil {
		return nil
	}

	cleaned := make(map[string]string)
	for k, v := range annotations {
		// Skip kubectl and system operational annotations
		if strings.HasPrefix(k, "kubectl.kubernetes.io/") {
			continue
		}
		if strings.HasPrefix(k, "control-plane.alpha.kubernetes.io/") {
			continue
		}
		if strings.HasPrefix(k, "deployment.kubernetes.io/") {
			continue
		}
		if strings.HasPrefix(k, "autoscaling.alpha.kubernetes.io/") {
			continue
		}
		cleaned[k] = v
	}

	if len(cleaned) == 0 {
		return nil
	}
	return cleaned
}
