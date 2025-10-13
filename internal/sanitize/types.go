/*
Copyright 2025 ConfigButler

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

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
