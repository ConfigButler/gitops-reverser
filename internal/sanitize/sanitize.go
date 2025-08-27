package sanitize

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// serverGeneratedFields are metadata fields that should be removed during sanitization

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

	// Clean up metadata by removing server-generated fields
	if metadata, found, err := unstructured.NestedMap(obj.Object, "metadata"); found && err == nil {
		cleanMetadata := make(map[string]interface{})

		// Copy only the fields we want to preserve
		preservedFields := []string{"name", "namespace", "labels", "annotations"}
		for _, field := range preservedFields {
			if value, exists := metadata[field]; exists && value != nil {
				cleanMetadata[field] = value
			}
		}

		// Set the cleaned metadata
		if len(cleanMetadata) > 0 {
			if err := unstructured.SetNestedMap(sanitized.Object, cleanMetadata, "metadata"); err != nil {
				// This should not happen with a freshly created map
				return nil
			}
		}
	}

	// Preserve the spec field
	if spec, found, err := unstructured.NestedFieldCopy(obj.Object, "spec"); found && err == nil {
		setNestedField(sanitized, spec, "spec")
	}

	// Preserve the data field (for ConfigMaps, Secrets)
	if data, found, err := unstructured.NestedFieldCopy(obj.Object, "data"); found && err == nil {
		setNestedField(sanitized, data, "data")
	}

	// Preserve the binaryData field (for ConfigMaps, Secrets)
	if binaryData, found, err := unstructured.NestedFieldCopy(obj.Object, "binaryData"); found && err == nil {
		setNestedField(sanitized, binaryData, "binaryData")
	}

	// Preserve other common fields that represent desired state
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

	for _, field := range preservedTopLevelFields {
		if value, found, err := unstructured.NestedFieldCopy(obj.Object, field); found && err == nil {
			setNestedField(sanitized, value, field)
		}
	}

	return sanitized
}

// setNestedField is a helper function to wrap unstructured.SetNestedField and handle errors.
func setNestedField(obj *unstructured.Unstructured, value interface{}, fields ...string) {
	_ = unstructured.SetNestedField(obj.Object, value, fields...)
}
