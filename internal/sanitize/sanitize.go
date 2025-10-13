/*
SPDX-License-Identifier: Apache-2.0

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
	preserveFields(sanitized, obj, []string{"spec", "data", "binaryData"})
	preserveTopLevelFields(sanitized, obj)

	// Remove auto-generated metadata fields (adapted from Kyverno)
	if metadata, found, _ := unstructured.NestedMap(sanitized.Object, "metadata"); found {
		removeAutoGenMetadata(metadata)
		_ = unstructured.SetNestedMap(sanitized.Object, metadata, "metadata")
	}

	// Remove nested server-generated fields based on resource kind
	removeNestedServerFields(sanitized)

	return sanitized
}

// setCoreIdentityFields preserves the core identity fields of the object.
func setCoreIdentityFields(sanitized, obj *unstructured.Unstructured) {
	sanitized.SetAPIVersion(obj.GetAPIVersion())
	sanitized.SetKind(obj.GetKind())
	sanitized.SetName(obj.GetName())
	sanitized.SetNamespace(obj.GetNamespace())
	sanitized.SetLabels(obj.GetLabels())
	// Clean annotations using the cleanAnnotations function from types.go
	sanitized.SetAnnotations(cleanAnnotations(obj.GetAnnotations()))
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

// removeAutoGenMetadata removes server-generated metadata fields.
// Adapted from Kyverno's excludeAutoGenMetadata function.
func removeAutoGenMetadata(metadata map[string]interface{}) {
	// Fields that are always removed
	delete(metadata, "uid")
	delete(metadata, "resourceVersion")
	delete(metadata, "generation")
	delete(metadata, "creationTimestamp")
	delete(metadata, "deletionTimestamp")
	delete(metadata, "deletionGracePeriodSeconds")
	delete(metadata, "selfLink")
	delete(metadata, "managedFields")
	delete(metadata, "ownerReferences")
}

// removeNestedServerFields removes nested server-generated fields from spec
// based on resource kind.
func removeNestedServerFields(obj *unstructured.Unstructured) {
	kind := obj.GetKind()

	switch kind {
	case "Service":
		removeServiceFields(obj)
	case "Pod":
		removePodFields(obj)
	case "PersistentVolumeClaim":
		removePVCFields(obj)
	}
}

// removeServiceFields removes Service-specific server-generated fields.
func removeServiceFields(obj *unstructured.Unstructured) {
	unstructured.RemoveNestedField(obj.Object, "spec", "clusterIP")
	unstructured.RemoveNestedField(obj.Object, "spec", "clusterIPs")
	unstructured.RemoveNestedField(obj.Object, "spec", "healthCheckNodePort")
	unstructured.RemoveNestedField(obj.Object, "spec", "ipFamilies")
	unstructured.RemoveNestedField(obj.Object, "spec", "ipFamilyPolicy")
	unstructured.RemoveNestedField(obj.Object, "spec", "internalTrafficPolicy")
}

// removePodFields removes Pod-specific server-generated fields.
func removePodFields(obj *unstructured.Unstructured) {
	unstructured.RemoveNestedField(obj.Object, "spec", "nodeName")
	// serviceAccountName is often auto-injected, but we keep it if explicitly set
	// The distinction is hard to make, so we keep it for now
}

// removePVCFields removes PersistentVolumeClaim-specific server-generated fields.
func removePVCFields(obj *unstructured.Unstructured) {
	unstructured.RemoveNestedField(obj.Object, "spec", "volumeName")
	unstructured.RemoveNestedField(obj.Object, "spec", "volumeMode")
}
