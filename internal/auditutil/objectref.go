// SPDX-License-Identifier: Apache-2.0

// Package auditutil contains shared Kubernetes audit-event parsing helpers.
package auditutil

import (
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// VerbToOperation maps mutating Kubernetes audit verbs to watch operations.
func VerbToOperation(verb string) (configv1alpha3.OperationType, bool) {
	switch strings.ToLower(verb) {
	case "create":
		return configv1alpha3.OperationCreate, true
	case "update", "patch":
		return configv1alpha3.OperationUpdate, true
	case "delete", "deletecollection":
		return configv1alpha3.OperationDelete, true
	default:
		return "", false
	}
}

// SplitAPIVersion splits Kubernetes apiVersion into group and version.
func SplitAPIVersion(apiVersion string) (string, string) {
	group, version, found := strings.Cut(apiVersion, "/")
	if !found {
		return "", apiVersion
	}
	return group, version
}

// ObjectRefGroupVersion returns the API group and version described by ref.
func ObjectRefGroupVersion(ref *auditv1.ObjectReference) (string, string) {
	if ref == nil {
		return "", ""
	}

	group, version := SplitAPIVersion(ref.APIVersion)
	if group != "" {
		return group, version
	}
	return ref.APIGroup, version
}

// ObjectRefGVR returns the GVR described by ref when resource and version are usable.
func ObjectRefGVR(ref *auditv1.ObjectReference) (schema.GroupVersionResource, bool) {
	if ref == nil || ref.Resource == "" {
		return schema.GroupVersionResource{}, false
	}

	group, version := ObjectRefGroupVersion(ref)
	if version == "" {
		return schema.GroupVersionResource{}, false
	}
	return schema.GroupVersionResource{
		Group:    group,
		Version:  version,
		Resource: ref.Resource,
	}, true
}
