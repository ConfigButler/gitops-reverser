//go:build legacy_crd
// +build legacy_crd

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

package webhook

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/correlation"
	"github.com/ConfigButler/gitops-reverser/internal/metrics"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
)

func TestEventHandler_Handle_MatchingRule(t *testing.T) {
	// Setup
	ctx := context.Background()
	_, err := metrics.InitOTLPExporter(ctx)
	require.NoError(t, err)

	scheme := runtime.NewScheme()
	client := fake.NewClientBuilder().WithScheme(scheme).Build()

	ruleStore := rulestore.NewStore()
	correlationStore := correlation.NewStore(60*time.Second, 1000)

	// Add a rule that matches Pods
	rule := configv1alpha1.WatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-rule",
			Namespace: "default",
		},
		Spec: configv1alpha1.WatchRuleSpec{
			GitRepoConfigRef: configv1alpha1.NamespacedName{Name: "test-repo-config"},
			Rules: []configv1alpha1.ResourceRule{
				{
					Resources: []string{"pods"},
				},
			},
		},
	}
	ruleStore.AddOrUpdate(rule)

	handler := &EventHandler{
		Client:           client,
		RuleStore:        ruleStore,
		CorrelationStore: correlationStore,
	}

	// Create decoder
	decoder := admission.NewDecoder(scheme)
	handler.Decoder = &decoder

	// Create admission request for a Pod
	pod := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata": map[string]interface{}{
				"name":      "test-pod",
				"namespace": "default",
			},
			"spec": map[string]interface{}{
				"containers": []interface{}{
					map[string]interface{}{
						"name":  "test-container",
						"image": "nginx:latest",
					},
				},
			},
		},
	}

	podBytes, err := pod.MarshalJSON()
	require.NoError(t, err)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "test-uid",
			Operation: admissionv1.Create,
			Resource: metav1.GroupVersionResource{
				Group:    "",
				Version:  "v1",
				Resource: "pods",
			},
			Object: runtime.RawExtension{
				Raw: podBytes,
			},
			UserInfo: authenticationv1.UserInfo{
				Username: "test-user",
			},
		},
	}

	// Execute
	response := handler.Handle(ctx, req)

	// Verify webhook allows the request
	assert.True(t, response.Allowed)
	assert.Equal(t, "request is allowed", response.Result.Message)

	// Verify correlation entry was stored (webhook's sole responsibility)
	assert.Equal(t, 1, correlationStore.Size(), "correlation entry should be stored")

	// NOTE: Webhook does NOT enqueue events - that is the watch path's responsibility
}

func TestEventHandler_Handle_NoMatchingRule(t *testing.T) {
	// Setup
	ctx := context.Background()
	_, err := metrics.InitOTLPExporter(ctx)
	require.NoError(t, err)

	scheme := runtime.NewScheme()
	client := fake.NewClientBuilder().WithScheme(scheme).Build()

	ruleStore := rulestore.NewStore()
	correlationStore := correlation.NewStore(60*time.Second, 1000)

	// Add a rule that matches Services (not Pods)
	rule := configv1alpha1.WatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "service-rule",
			Namespace: "default",
		},
		Spec: configv1alpha1.WatchRuleSpec{
			GitRepoConfigRef: configv1alpha1.NamespacedName{Name: "test-repo-config"},
			Rules: []configv1alpha1.ResourceRule{
				{
					Resources: []string{"services"},
				},
			},
		},
	}
	ruleStore.AddOrUpdate(rule)

	handler := &EventHandler{
		Client:           client,
		RuleStore:        ruleStore,
		CorrelationStore: correlationStore,
	}

	// Create decoder
	decoder := admission.NewDecoder(scheme)
	handler.Decoder = &decoder

	// Create admission request for a Pod (which doesn't match the Service rule)
	pod := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata": map[string]interface{}{
				"name":      "test-pod",
				"namespace": "default",
			},
		},
	}

	podBytes, err := pod.MarshalJSON()
	require.NoError(t, err)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "test-uid",
			Operation: admissionv1.Create,
			Resource: metav1.GroupVersionResource{
				Group:    "",
				Version:  "v1",
				Resource: "pods",
			},
			Object: runtime.RawExtension{
				Raw: podBytes,
			},
			UserInfo: authenticationv1.UserInfo{
				Username: "test-user",
			},
		},
	}

	// Execute
	response := handler.Handle(ctx, req)

	// Verify
	assert.True(t, response.Allowed)
	assert.Equal(t, "request is allowed", response.Result.Message)
	assert.Equal(t, 0, correlationStore.Size(), "no correlation entry for non-matching rule")
}

func TestEventHandler_Handle_MultipleMatchingRules(t *testing.T) {
	// Setup
	ctx := context.Background()
	_, err := metrics.InitOTLPExporter(ctx)
	require.NoError(t, err)

	scheme := runtime.NewScheme()
	client := fake.NewClientBuilder().WithScheme(scheme).Build()

	ruleStore := rulestore.NewStore()
	correlationStore := correlation.NewStore(60*time.Second, 1000)

	// Add multiple rules that match Pods
	rule1 := configv1alpha1.WatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-rule-1",
			Namespace: "default",
		},
		Spec: configv1alpha1.WatchRuleSpec{
			GitRepoConfigRef: configv1alpha1.NamespacedName{Name: "repo-config-1"},
			Rules: []configv1alpha1.ResourceRule{
				{
					Resources: []string{"pods"},
				},
			},
		},
	}

	rule2 := configv1alpha1.WatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-rule-2",
			Namespace: "default",
		},
		Spec: configv1alpha1.WatchRuleSpec{
			GitRepoConfigRef: configv1alpha1.NamespacedName{Name: "repo-config-2"},
			Rules: []configv1alpha1.ResourceRule{
				{
					Resources: []string{"pods", "services"},
				},
			},
		},
	}

	ruleStore.AddOrUpdate(rule1)
	ruleStore.AddOrUpdate(rule2)

	handler := &EventHandler{
		Client:           client,
		RuleStore:        ruleStore,
		CorrelationStore: correlationStore,
	}

	// Create decoder
	decoder := admission.NewDecoder(scheme)
	handler.Decoder = &decoder

	// Create admission request for a Pod
	pod := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata": map[string]interface{}{
				"name":      "test-pod",
				"namespace": "default",
			},
		},
	}

	podBytes, err := pod.MarshalJSON()
	require.NoError(t, err)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "test-uid",
			Operation: admissionv1.Create,
			Resource: metav1.GroupVersionResource{
				Group:    "",
				Version:  "v1",
				Resource: "pods",
			},
			Object: runtime.RawExtension{
				Raw: podBytes,
			},
			UserInfo: authenticationv1.UserInfo{
				Username: "test-user",
			},
		},
	}

	// Execute
	response := handler.Handle(ctx, req)

	// Verify
	assert.True(t, response.Allowed)

	// Webhook stores ONE correlation entry per resource change
	// (not one per matching rule - that's the watch path's job to create multiple events)
	assert.Equal(t, 1, correlationStore.Size(), "single correlation entry for resource")
}

func TestEventHandler_Handle_ExcludedByLabels(t *testing.T) {
	// Setup
	ctx := context.Background()
	_, err := metrics.InitOTLPExporter(ctx)
	require.NoError(t, err)

	scheme := runtime.NewScheme()
	client := fake.NewClientBuilder().WithScheme(scheme).Build()

	ruleStore := rulestore.NewStore()
	correlationStore := correlation.NewStore(60*time.Second, 1000)

	// Add a rule that excludes resources with ignore label using ObjectSelector
	rule := configv1alpha1.WatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-rule",
			Namespace: "default",
		},
		Spec: configv1alpha1.WatchRuleSpec{
			GitRepoConfigRef: configv1alpha1.NamespacedName{Name: "test-repo-config"},
			ObjectSelector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{
						Key:      "configbutler.ai/ignore",
						Operator: metav1.LabelSelectorOpDoesNotExist,
					},
				},
			},
			Rules: []configv1alpha1.ResourceRule{
				{
					Resources: []string{"pods"},
				},
			},
		},
	}
	ruleStore.AddOrUpdate(rule)

	handler := &EventHandler{
		Client:           client,
		RuleStore:        ruleStore,
		CorrelationStore: correlationStore,
	}

	// Create decoder
	decoder := admission.NewDecoder(scheme)
	handler.Decoder = &decoder

	// Create admission request for a Pod with ignore label
	pod := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata": map[string]interface{}{
				"name":      "ignored-pod",
				"namespace": "default",
				"labels": map[string]interface{}{
					"configbutler.ai/ignore": "true",
				},
			},
		},
	}

	podBytes, err := pod.MarshalJSON()
	require.NoError(t, err)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "test-uid",
			Operation: admissionv1.Create,
			Resource: metav1.GroupVersionResource{
				Group:    "",
				Version:  "v1",
				Resource: "pods",
			},
			Object: runtime.RawExtension{
				Raw: podBytes,
			},
			UserInfo: authenticationv1.UserInfo{
				Username: "test-user",
			},
		},
	}

	// Execute
	response := handler.Handle(ctx, req)

	// Verify webhook allows request (webhook doesn't filter)
	assert.True(t, response.Allowed)
	// Webhook stores correlation for ALL resources (watch path filters based on rules)
	assert.Equal(t, 1, correlationStore.Size(), "correlation entry should be stored even if rule excludes it")
}

func TestEventHandler_Handle_InvalidJSON(t *testing.T) {
	// Setup
	ctx := context.Background()
	_, err := metrics.InitOTLPExporter(ctx)
	require.NoError(t, err)

	scheme := runtime.NewScheme()
	client := fake.NewClientBuilder().WithScheme(scheme).Build()

	ruleStore := rulestore.NewStore()
	correlationStore := correlation.NewStore(60*time.Second, 1000)

	handler := &EventHandler{
		Client:           client,
		RuleStore:        ruleStore,
		CorrelationStore: correlationStore,
	}

	// Create decoder
	decoder := admission.NewDecoder(scheme)
	handler.Decoder = &decoder

	// Create admission request with invalid JSON
	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "test-uid",
			Operation: admissionv1.Create,
			Object: runtime.RawExtension{
				Raw: []byte(`{"invalid": json}`), // Invalid JSON
			},
			UserInfo: authenticationv1.UserInfo{
				Username: "test-user",
			},
		},
	}

	// Execute
	response := handler.Handle(ctx, req)

	// Verify
	assert.False(t, response.Allowed)
	assert.Equal(t, int32(400), response.Result.Code) // Bad Request
	assert.Equal(t, 0, correlationStore.Size())       // No correlation for invalid request
}

func TestEventHandler_Handle_NamespacedIngressResource(t *testing.T) {
	// Setup
	ctx := context.Background()
	_, err := metrics.InitOTLPExporter(ctx)
	require.NoError(t, err)

	scheme := runtime.NewScheme()
	client := fake.NewClientBuilder().WithScheme(scheme).Build()

	ruleStore := rulestore.NewStore()
	correlationStore := correlation.NewStore(60*time.Second, 1000)

	// Add a rule matching Ingress resources (namespace-scoped)
	rule := configv1alpha1.WatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ingress-rule",
			Namespace: "default",
		},
		Spec: configv1alpha1.WatchRuleSpec{
			GitRepoConfigRef: configv1alpha1.NamespacedName{Name: "test-repo-config"},
			Rules: []configv1alpha1.ResourceRule{
				{
					Resources: []string{"ingresses"},
				},
			},
		},
	}
	ruleStore.AddOrUpdateWatchRule(rule)

	handler := &EventHandler{
		Client:           client,
		RuleStore:        ruleStore,
		CorrelationStore: correlationStore,
	}

	// Create decoder
	decoder := admission.NewDecoder(scheme)
	handler.Decoder = &decoder

	// Create admission request for an Ingress (namespace-scoped resource)
	ingress := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "networking.k8s.io/v1",
			"kind":       "Ingress",
			"metadata": map[string]interface{}{
				"name":      "test-ingress",
				"namespace": "default",
			},
			"spec": map[string]interface{}{
				"rules": []interface{}{},
			},
		},
	}
	ingress.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "networking.k8s.io",
		Version: "v1",
		Kind:    "Ingress",
	})

	ingressBytes, err := ingress.MarshalJSON()
	require.NoError(t, err)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "test-uid",
			Operation: admissionv1.Create,
			Resource: metav1.GroupVersionResource{
				Group:    "networking.k8s.io",
				Version:  "v1",
				Resource: "ingresses",
			},
			Object: runtime.RawExtension{
				Raw: ingressBytes,
			},
			UserInfo: authenticationv1.UserInfo{
				Username: "test-user",
			},
		},
	}

	// Execute
	response := handler.Handle(ctx, req)

	// Verify webhook stored correlation
	assert.True(t, response.Allowed)
	assert.Equal(t, 1, correlationStore.Size(), "correlation entry should be stored")
}

func TestEventHandler_Handle_DifferentOperations(t *testing.T) {
	operations := []admissionv1.Operation{
		admissionv1.Create,
		admissionv1.Update,
		admissionv1.Delete,
	}

	for _, operation := range operations {
		t.Run(string(operation), func(t *testing.T) {
			// Setup
			ctx := context.Background()
			_, err := metrics.InitOTLPExporter(ctx)
			require.NoError(t, err)

			scheme := runtime.NewScheme()
			client := fake.NewClientBuilder().WithScheme(scheme).Build()

			ruleStore := rulestore.NewStore()
			correlationStore := correlation.NewStore(60*time.Second, 1000)

			// Add a rule that matches Pods
			rule := configv1alpha1.WatchRule{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod-rule",
					Namespace: "default",
				},
				Spec: configv1alpha1.WatchRuleSpec{
					GitRepoConfigRef: configv1alpha1.NamespacedName{Name: "test-repo-config"},
					Rules: []configv1alpha1.ResourceRule{
						{
							Resources: []string{"pods"},
						},
					},
				},
			}
			ruleStore.AddOrUpdate(rule)

			handler := &EventHandler{
				Client:           client,
				RuleStore:        ruleStore,
				CorrelationStore: correlationStore,
			}

			// Create decoder
			decoder := admission.NewDecoder(scheme)
			handler.Decoder = &decoder

			// Create admission request
			pod := &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Pod",
					"metadata": map[string]interface{}{
						"name":      "test-pod",
						"namespace": "default",
					},
				},
			}

			podBytes, err := pod.MarshalJSON()
			require.NoError(t, err)

			req := admission.Request{
				AdmissionRequest: admissionv1.AdmissionRequest{
					UID:       "test-uid",
					Operation: operation,
					Resource: metav1.GroupVersionResource{
						Group:    "",
						Version:  "v1",
						Resource: "pods",
					},
					Object: runtime.RawExtension{
						Raw: podBytes,
					},
					UserInfo: authenticationv1.UserInfo{
						Username: "test-user",
					},
				},
			}

			// Execute
			response := handler.Handle(ctx, req)

			// Verify webhook stored correlation
			assert.True(t, response.Allowed)
			assert.Equal(t, 1, correlationStore.Size(), "correlation entry should be stored")
		})
	}
}

func TestEventHandler_Handle_ClusterScopedResource(t *testing.T) {
	// Setup
	ctx := context.Background()
	_, err := metrics.InitOTLPExporter(ctx)
	require.NoError(t, err)

	scheme := runtime.NewScheme()
	client := fake.NewClientBuilder().WithScheme(scheme).Build()

	ruleStore := rulestore.NewStore()
	correlationStore := correlation.NewStore(60*time.Second, 1000)

	// Add a WatchRule for namespaces (namespace-scoped rule watching cluster-scoped resource)
	// Note: Webhook stores correlation for ALL resources regardless of rules
	rule := configv1alpha1.WatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "namespace-rule",
			Namespace: "default",
		},
		Spec: configv1alpha1.WatchRuleSpec{
			GitRepoConfigRef: configv1alpha1.NamespacedName{Name: "cluster-repo-config"},
			Rules: []configv1alpha1.ResourceRule{
				{
					Resources: []string{"namespaces"},
				},
			},
		},
	}
	ruleStore.AddOrUpdateWatchRule(rule)

	handler := &EventHandler{
		Client:           client,
		RuleStore:        ruleStore,
		CorrelationStore: correlationStore,
	}

	// Create decoder
	decoder := admission.NewDecoder(scheme)
	handler.Decoder = &decoder

	// Create admission request for a Namespace (cluster-scoped resource)
	namespace := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Namespace",
			"metadata": map[string]interface{}{
				"name": "test-namespace",
			},
		},
	}

	namespaceBytes, err := namespace.MarshalJSON()
	require.NoError(t, err)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "test-uid",
			Operation: admissionv1.Create,
			Resource: metav1.GroupVersionResource{
				Group:    "",
				Version:  "v1",
				Resource: "namespaces",
			},
			Object: runtime.RawExtension{
				Raw: namespaceBytes,
			},
			UserInfo: authenticationv1.UserInfo{
				Username: "cluster-admin",
			},
		},
	}

	// Execute
	response := handler.Handle(ctx, req)

	// Verify webhook stored correlation (webhook stores for ALL resources)
	assert.True(t, response.Allowed)
	assert.Equal(t, 1, correlationStore.Size(), "webhook stores correlation for all resources")
}

func TestEventHandler_InjectDecoder(t *testing.T) {
	handler := &EventHandler{}

	scheme := runtime.NewScheme()
	decoder := admission.NewDecoder(scheme)

	err := handler.InjectDecoder(&decoder)
	require.NoError(t, err)
	assert.NotNil(t, handler.Decoder)
	assert.Equal(t, &decoder, handler.Decoder)
}

func TestEventHandler_Handle_SanitizationApplied(t *testing.T) {
	// Setup
	ctx := context.Background()
	_, err := metrics.InitOTLPExporter(ctx)
	require.NoError(t, err)

	scheme := runtime.NewScheme()
	client := fake.NewClientBuilder().WithScheme(scheme).Build()

	ruleStore := rulestore.NewStore()
	correlationStore := correlation.NewStore(60*time.Second, 1000)

	// Add a rule that matches Pods
	rule := configv1alpha1.WatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-rule",
			Namespace: "default",
		},
		Spec: configv1alpha1.WatchRuleSpec{
			GitRepoConfigRef: configv1alpha1.NamespacedName{Name: "test-repo-config"},
			Rules: []configv1alpha1.ResourceRule{
				{
					Resources: []string{"pods"},
				},
			},
		},
	}
	ruleStore.AddOrUpdate(rule)

	handler := &EventHandler{
		Client:           client,
		RuleStore:        ruleStore,
		CorrelationStore: correlationStore,
	}

	// Create decoder
	decoder := admission.NewDecoder(scheme)
	handler.Decoder = &decoder

	// Create admission request for a Pod with status (should be sanitized)
	pod := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata": map[string]interface{}{
				"name":              "test-pod",
				"namespace":         "default",
				"creationTimestamp": "2025-01-01T00:00:00Z", // Should be removed
				"uid":               "12345",                // Should be removed
			},
			"spec": map[string]interface{}{
				"containers": []interface{}{
					map[string]interface{}{
						"name":  "test-container",
						"image": "nginx:latest",
					},
				},
			},
			"status": map[string]interface{}{ // Should be removed
				"phase": "Running",
			},
		},
	}

	podBytes, err := pod.MarshalJSON()
	require.NoError(t, err)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "test-uid",
			Operation: admissionv1.Create,
			Resource: metav1.GroupVersionResource{
				Group:    "",
				Version:  "v1",
				Resource: "pods",
			},
			Object: runtime.RawExtension{
				Raw: podBytes,
			},
			UserInfo: authenticationv1.UserInfo{
				Username: "test-user",
			},
		},
	}

	// Execute
	response := handler.Handle(ctx, req)

	// Verify webhook stored correlation (actual responsibility)
	assert.True(t, response.Allowed)
	assert.Equal(t, 1, correlationStore.Size(), "correlation entry should be stored")

	// NOTE: Sanitization testing moved to internal/sanitize package tests
	// Webhook's job is correlation storage only
}
