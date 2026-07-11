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

// isOperationalLabel reports whether a label is controller bookkeeping.
//
// Note the asymmetry with Argo CD: its `label` and `annotation+label` tracking
// methods stamp `app.kubernetes.io/instance`, which is indistinguishable from the
// standard recommended label that Helm and Kustomize set for legitimate reasons.
// It is therefore NOT stripped, and reverser-managed paths must leave Argo CD on
// its default `annotation` tracking method. See docs/bi-directional.md.
func isOperationalLabel(key string) bool {
	return strings.HasPrefix(key, "kustomize.toolkit.fluxcd.io/") ||
		strings.HasPrefix(key, "kro.run/") ||
		strings.HasPrefix(key, "applyset.kubernetes.io/")
}

// isOperationalAnnotation reports whether an annotation is controller bookkeeping.
//
// Argo CD's tracking annotations are matched EXACTLY, not by prefix. Its
// repo-server stamps `argocd.argoproj.io/tracking-id` onto every non-CRD object it
// applies (the default tracking method is `annotation`), with the value
// `<app>:<group>/<Kind>:<ns>/<name>`, plus `installation-id` when an installation
// ID is configured. That is controller state, not user intent, and committing it
// to Git is actively harmful: Argo never validates a tracking-id against the
// object it reads it from, so a manifest carrying a foreign one makes another
// Application fail to sync with "Shared resource found".
//
// This CANNOT be an `argocd.argoproj.io/` prefix strip. Sibling annotations under
// that prefix — sync-wave, sync-options, compare-options, hook — are user intent
// that belongs in Git, and stripping them would silently drop a user's sync
// ordering. Hence the exact-key match.
//
// Exercised end-to-end against a real Argo CD in
// test/e2e/argocd_bi_directional_e2e_test.go.
func isOperationalAnnotation(key string) bool {
	switch key {
	case "argocd.argoproj.io/tracking-id", "argocd.argoproj.io/installation-id":
		return true
	}

	return strings.HasPrefix(key, "kubectl.kubernetes.io/") ||
		strings.HasPrefix(key, "control-plane.alpha.kubernetes.io/") ||
		strings.HasPrefix(key, "deployment.kubernetes.io/") ||
		strings.HasPrefix(key, "autoscaling.alpha.kubernetes.io/") ||
		strings.HasPrefix(key, "kustomize.toolkit.fluxcd.io/") ||
		strings.HasPrefix(key, "applyset.kubernetes.io/")
}
