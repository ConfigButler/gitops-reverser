package sanitize

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Sanitize removes server-side fields from a Kubernetes object,
// leaving only the desired state.
func Sanitize(obj *unstructured.Unstructured) *unstructured.Unstructured {
	sanitized := &unstructured.Unstructured{Object: make(map[string]interface{})}

	// Preserve the core identity fields.
	sanitized.SetAPIVersion(obj.GetAPIVersion())
	sanitized.SetKind(obj.GetKind())
	sanitized.SetName(obj.GetName())
	sanitized.SetNamespace(obj.GetNamespace())
	sanitized.SetLabels(obj.GetLabels())
	sanitized.SetAnnotations(obj.GetAnnotations())

	// Preserve the spec and data fields.
	if spec, found, err := unstructured.NestedFieldCopy(obj.Object, "spec"); found && err == nil {
		unstructured.SetNestedField(sanitized.Object, spec, "spec")
	}
	if data, found, err := unstructured.NestedFieldCopy(obj.Object, "data"); found && err == nil {
		unstructured.SetNestedField(sanitized.Object, data, "data")
	}

	return sanitized
}
