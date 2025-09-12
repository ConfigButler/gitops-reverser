/*
Package sanitize removes server-generated fields from Kubernetes objects.
It preserves only the desired state for Git commit operations.
*/
package sanitize

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// serverGeneratedFields are metadata fields that should be removed during sanitization

// Sanitize removes server-side fields from a Kubernetes object,
// leaving only the desired state.
func Sanitize(obj *unstructured.Unstructured) *unstructured.Unstructured {
	sanitized := &unstructured.Unstructured{Object: make(map[string]interface{})}

	setCoreIdentityFields(sanitized, obj)
	setCleanMetadata(sanitized, obj)
	preserveFields(sanitized, obj, []string{"spec", "data", "binaryData"})
	preserveTopLevelFields(sanitized, obj)

	return sanitized
}

// setCoreIdentityFields preserves the core identity fields of the object.
func setCoreIdentityFields(sanitized, obj *unstructured.Unstructured) {
	sanitized.SetAPIVersion(obj.GetAPIVersion())
	sanitized.SetKind(obj.GetKind())
	sanitized.SetName(obj.GetName())
	sanitized.SetNamespace(obj.GetNamespace())
	sanitized.SetLabels(obj.GetLabels())
	sanitized.SetAnnotations(obj.GetAnnotations())
}

// setCleanMetadata cleans up metadata by removing server-generated fields.
func setCleanMetadata(sanitized, obj *unstructured.Unstructured) {
	metadata, found, err := unstructured.NestedMap(obj.Object, "metadata")
	if !found || err != nil {
		return
	}

	cleanMetadata := make(map[string]interface{})
	preservedFields := []string{"name", "namespace", "labels", "annotations"}

	for _, field := range preservedFields {
		if value, exists := metadata[field]; exists && value != nil {
			cleanMetadata[field] = value
		}
	}

	if len(cleanMetadata) > 0 {
		_ = unstructured.SetNestedMap(sanitized.Object, cleanMetadata, "metadata")
	}
}

// preserveFields preserves specified fields from the original object.
func preserveFields(sanitized, obj *unstructured.Unstructured, fields []string) {
	for _, field := range fields {
		if value, found, err := unstructured.NestedFieldCopy(obj.Object, field); found && err == nil {
			setNestedField(sanitized, value, field)
		}
	}
}

// preserveTopLevelFields preserves other common fields that represent desired state.
func preserveTopLevelFields(sanitized, obj *unstructured.Unstructured) {
	preservedTopLevelFields := []string{
		"rules",          // for RBAC resources
		"subjects",       // for RoleBindings
		"roleRef",        // for RoleBindings
		"webhooks",       // for ValidatingWebhookConfiguration
		"template",       // for various template-based resources
		"involvedObject", // for Events
		"reason",         // for Events
		"message",        // for Events
		"type",           // for Events
		"eventTime",      // for Events
	}

	preserveFields(sanitized, obj, preservedTopLevelFields)
}

// setNestedField is a helper function to wrap unstructured.SetNestedField and handle errors.
func setNestedField(obj *unstructured.Unstructured, value interface{}, fields ...string) {
	_ = unstructured.SetNestedField(obj.Object, value, fields...)
}
