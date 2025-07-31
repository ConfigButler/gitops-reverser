package webhook

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ConfigButler/gitops-reverser/internal/eventqueue"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
)

func TestEventHandler_Handle_MatchingRule(t *testing.T) {
	// Setup
	scheme := runtime.NewScheme()
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	
	ruleStore := rulestore.NewStore()
	eventQueue := eventqueue.NewQueue()
	
	// Add a rule that matches Pods
	rule := configv1alpha1.WatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-rule",
			Namespace: "default",
		},
		Spec: configv1alpha1.WatchRuleSpec{
			GitRepoConfigRef: "test-repo-config",
			Rules: []configv1alpha1.ResourceRule{
				{
					Resources: []string{"Pod"},
				},
			},
		},
	}
	ruleStore.AddOrUpdate(rule)
	
	handler := &EventHandler{
		Client:     client,
		RuleStore:  ruleStore,
		EventQueue: eventQueue,
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
			Object: runtime.RawExtension{
				Raw: podBytes,
			},
			UserInfo: authenticationv1.UserInfo{
				Username: "test-user",
			},
		},
	}
	
	// Execute
	ctx := context.Background()
	response := handler.Handle(ctx, req)
	
	// Verify
	assert.True(t, response.Allowed)
	assert.Equal(t, "request is allowed", response.Result.Message)
	assert.Equal(t, 1, eventQueue.Size())
	
	// Verify the enqueued event
	events := eventQueue.DequeueAll()
	require.Equal(t, 1, len(events))
	
	event := events[0]
	assert.Equal(t, "test-pod", event.Object.GetName())
	assert.Equal(t, "default", event.Object.GetNamespace())
	assert.Equal(t, "Pod", event.Object.GetKind())
	assert.Equal(t, "test-repo-config", event.GitRepoConfigRef)
	assert.Equal(t, "test-uid", string(event.Request.UID))
	assert.Equal(t, "test-user", event.Request.UserInfo.Username)
}

func TestEventHandler_Handle_NoMatchingRule(t *testing.T) {
	// Setup
	scheme := runtime.NewScheme()
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	
	ruleStore := rulestore.NewStore()
	eventQueue := eventqueue.NewQueue()
	
	// Add a rule that matches Services (not Pods)
	rule := configv1alpha1.WatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "service-rule",
			Namespace: "default",
		},
		Spec: configv1alpha1.WatchRuleSpec{
			GitRepoConfigRef: "test-repo-config",
			Rules: []configv1alpha1.ResourceRule{
				{
					Resources: []string{"Service"},
				},
			},
		},
	}
	ruleStore.AddOrUpdate(rule)
	
	handler := &EventHandler{
		Client:     client,
		RuleStore:  ruleStore,
		EventQueue: eventQueue,
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
			Object: runtime.RawExtension{
				Raw: podBytes,
			},
			UserInfo: authenticationv1.UserInfo{
				Username: "test-user",
			},
		},
	}
	
	// Execute
	ctx := context.Background()
	response := handler.Handle(ctx, req)
	
	// Verify
	assert.True(t, response.Allowed)
	assert.Equal(t, "request is allowed", response.Result.Message)
	assert.Equal(t, 0, eventQueue.Size()) // No events should be enqueued
}

func TestEventHandler_Handle_MultipleMatchingRules(t *testing.T) {
	// Setup
	scheme := runtime.NewScheme()
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	
	ruleStore := rulestore.NewStore()
	eventQueue := eventqueue.NewQueue()
	
	// Add multiple rules that match Pods
	rule1 := configv1alpha1.WatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-rule-1",
			Namespace: "default",
		},
		Spec: configv1alpha1.WatchRuleSpec{
			GitRepoConfigRef: "repo-config-1",
			Rules: []configv1alpha1.ResourceRule{
				{
					Resources: []string{"Pod"},
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
			GitRepoConfigRef: "repo-config-2",
			Rules: []configv1alpha1.ResourceRule{
				{
					Resources: []string{"Pod", "Service"},
				},
			},
		},
	}
	
	ruleStore.AddOrUpdate(rule1)
	ruleStore.AddOrUpdate(rule2)
	
	handler := &EventHandler{
		Client:     client,
		RuleStore:  ruleStore,
		EventQueue: eventQueue,
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
			Object: runtime.RawExtension{
				Raw: podBytes,
			},
			UserInfo: authenticationv1.UserInfo{
				Username: "test-user",
			},
		},
	}
	
	// Execute
	ctx := context.Background()
	response := handler.Handle(ctx, req)
	
	// Verify
	assert.True(t, response.Allowed)
	assert.Equal(t, 2, eventQueue.Size()) // Two events should be enqueued
	
	// Verify the enqueued events
	events := eventQueue.DequeueAll()
	require.Equal(t, 2, len(events))
	
	// Verify both events reference different repo configs
	repoConfigs := make([]string, len(events))
	for i, event := range events {
		assert.Equal(t, "test-pod", event.Object.GetName())
		assert.Equal(t, "Pod", event.Object.GetKind())
		repoConfigs[i] = event.GitRepoConfigRef
	}
	
	assert.Contains(t, repoConfigs, "repo-config-1")
	assert.Contains(t, repoConfigs, "repo-config-2")
}

func TestEventHandler_Handle_ExcludedByLabels(t *testing.T) {
	// Setup
	scheme := runtime.NewScheme()
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	
	ruleStore := rulestore.NewStore()
	eventQueue := eventqueue.NewQueue()
	
	// Add a rule that excludes resources with ignore label
	rule := configv1alpha1.WatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-rule",
			Namespace: "default",
		},
		Spec: configv1alpha1.WatchRuleSpec{
			GitRepoConfigRef: "test-repo-config",
			ExcludeLabels: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{
						Key:      "configbutler.ai/ignore",
						Operator: metav1.LabelSelectorOpExists,
					},
				},
			},
			Rules: []configv1alpha1.ResourceRule{
				{
					Resources: []string{"Pod"},
				},
			},
		},
	}
	ruleStore.AddOrUpdate(rule)
	
	handler := &EventHandler{
		Client:     client,
		RuleStore:  ruleStore,
		EventQueue: eventQueue,
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
			Object: runtime.RawExtension{
				Raw: podBytes,
			},
			UserInfo: authenticationv1.UserInfo{
				Username: "test-user",
			},
		},
	}
	
	// Execute
	ctx := context.Background()
	response := handler.Handle(ctx, req)
	
	// Verify
	assert.True(t, response.Allowed)
	assert.Equal(t, 0, eventQueue.Size()) // No events should be enqueued due to exclusion
}

func TestEventHandler_Handle_InvalidJSON(t *testing.T) {
	// Setup
	scheme := runtime.NewScheme()
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	
	ruleStore := rulestore.NewStore()
	eventQueue := eventqueue.NewQueue()
	
	handler := &EventHandler{
		Client:     client,
		RuleStore:  ruleStore,
		EventQueue: eventQueue,
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
	ctx := context.Background()
	response := handler.Handle(ctx, req)
	
	// Verify
	assert.False(t, response.Allowed)
	assert.Equal(t, int32(400), response.Result.Code) // Bad Request
	assert.Equal(t, 0, eventQueue.Size()) // No events should be enqueued
}

func TestEventHandler_Handle_WildcardMatching(t *testing.T) {
	// Setup
	scheme := runtime.NewScheme()
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	
	ruleStore := rulestore.NewStore()
	eventQueue := eventqueue.NewQueue()
	
	// Add a rule with wildcard matching
	rule := configv1alpha1.WatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ingress-rule",
			Namespace: "default",
		},
		Spec: configv1alpha1.WatchRuleSpec{
			GitRepoConfigRef: "test-repo-config",
			Rules: []configv1alpha1.ResourceRule{
				{
					Resources: []string{"Ingress*"},
				},
			},
		},
	}
	ruleStore.AddOrUpdate(rule)
	
	handler := &EventHandler{
		Client:     client,
		RuleStore:  ruleStore,
		EventQueue: eventQueue,
	}
	
	// Create decoder
	decoder := admission.NewDecoder(scheme)
	handler.Decoder = &decoder
	
	// Create admission request for an IngressClass (should match Ingress*)
	ingressClass := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "networking.k8s.io/v1",
			"kind":       "IngressClass",
			"metadata": map[string]interface{}{
				"name": "test-ingress-class",
			},
		},
	}
	ingressClass.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "networking.k8s.io",
		Version: "v1",
		Kind:    "IngressClass",
	})
	
	ingressClassBytes, err := ingressClass.MarshalJSON()
	require.NoError(t, err)
	
	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "test-uid",
			Operation: admissionv1.Create,
			Object: runtime.RawExtension{
				Raw: ingressClassBytes,
			},
			UserInfo: authenticationv1.UserInfo{
				Username: "test-user",
			},
		},
	}
	
	// Execute
	ctx := context.Background()
	response := handler.Handle(ctx, req)
	
	// Verify
	assert.True(t, response.Allowed)
	assert.Equal(t, 1, eventQueue.Size())
	
	// Verify the enqueued event
	events := eventQueue.DequeueAll()
	require.Equal(t, 1, len(events))
	
	event := events[0]
	assert.Equal(t, "test-ingress-class", event.Object.GetName())
	assert.Equal(t, "IngressClass", event.Object.GetKind())
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
			scheme := runtime.NewScheme()
			client := fake.NewClientBuilder().WithScheme(scheme).Build()
			
			ruleStore := rulestore.NewStore()
			eventQueue := eventqueue.NewQueue()
			
			// Add a rule that matches Pods
			rule := configv1alpha1.WatchRule{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod-rule",
					Namespace: "default",
				},
				Spec: configv1alpha1.WatchRuleSpec{
					GitRepoConfigRef: "test-repo-config",
					Rules: []configv1alpha1.ResourceRule{
						{
							Resources: []string{"Pod"},
						},
					},
				},
			}
			ruleStore.AddOrUpdate(rule)
			
			handler := &EventHandler{
				Client:     client,
				RuleStore:  ruleStore,
				EventQueue: eventQueue,
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
					Object: runtime.RawExtension{
						Raw: podBytes,
					},
					UserInfo: authenticationv1.UserInfo{
						Username: "test-user",
					},
				},
			}
			
			// Execute
			ctx := context.Background()
			response := handler.Handle(ctx, req)
			
			// Verify
			assert.True(t, response.Allowed)
			assert.Equal(t, 1, eventQueue.Size())
			
			// Verify the operation is preserved in the event
			events := eventQueue.DequeueAll()
			require.Equal(t, 1, len(events))
			
			event := events[0]
			assert.Equal(t, operation, event.Request.Operation)
		})
	}
}

func TestEventHandler_Handle_ClusterScopedResource(t *testing.T) {
	// Setup
	scheme := runtime.NewScheme()
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	
	ruleStore := rulestore.NewStore()
	eventQueue := eventqueue.NewQueue()
	
	// Add a rule that matches Namespaces
	rule := configv1alpha1.WatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "namespace-rule",
			Namespace: "default",
		},
		Spec: configv1alpha1.WatchRuleSpec{
			GitRepoConfigRef: "cluster-repo-config",
			Rules: []configv1alpha1.ResourceRule{
				{
					Resources: []string{"Namespace"},
				},
			},
		},
	}
	ruleStore.AddOrUpdate(rule)
	
	handler := &EventHandler{
		Client:     client,
		RuleStore:  ruleStore,
		EventQueue: eventQueue,
	}
	
	// Create decoder
	decoder := admission.NewDecoder(scheme)
	handler.Decoder = &decoder
	
	// Create admission request for a Namespace
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
			Object: runtime.RawExtension{
				Raw: namespaceBytes,
			},
			UserInfo: authenticationv1.UserInfo{
				Username: "cluster-admin",
			},
		},
	}
	
	// Execute
	ctx := context.Background()
	response := handler.Handle(ctx, req)
	
	// Verify
	assert.True(t, response.Allowed)
	assert.Equal(t, 1, eventQueue.Size())
	
	// Verify the enqueued event
	events := eventQueue.DequeueAll()
	require.Equal(t, 1, len(events))
	
	event := events[0]
	assert.Equal(t, "test-namespace", event.Object.GetName())
	assert.Equal(t, "", event.Object.GetNamespace()) // Cluster-scoped resources have no namespace
	assert.Equal(t, "Namespace", event.Object.GetKind())
	assert.Equal(t, "cluster-repo-config", event.GitRepoConfigRef)
}

func TestEventHandler_InjectDecoder(t *testing.T) {
	handler := &EventHandler{}
	
	scheme := runtime.NewScheme()
	decoder, err := admission.NewDecoder(scheme)
	require.NoError(t, err)
	
	err = handler.InjectDecoder(&decoder)
	assert.NoError(t, err)
	assert.NotNil(t, handler.Decoder)
	assert.Equal(t, &decoder, handler.Decoder)
}

func TestEventHandler_Handle_SanitizationApplied(t *testing.T) {
	// Setup
	scheme := runtime.NewScheme()
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	
	ruleStore := rulestore.NewStore()
	eventQueue := eventqueue.NewQueue()
	
	// Add a rule that matches Pods
	rule := configv1alpha1.WatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-rule",
			Namespace: "default",
		},
		Spec: configv1alpha1.WatchRuleSpec{
			GitRepoConfigRef: "test-repo-config",
			Rules: []configv1alpha1.ResourceRule{
				{
					Resources: []string{"Pod"},
				},
			},
		},
	}
	ruleStore.AddOrUpdate(rule)
	
	handler := &EventHandler{
		Client:     client,
		RuleStore:  ruleStore,
		EventQueue: eventQueue,
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
			Object: runtime.RawExtension{
				Raw: podBytes,
			},
			UserInfo: authenticationv1.UserInfo{
				Username: "test-user",
			},
		},
	}
	
	// Execute
	ctx := context.Background()
	response := handler.Handle(ctx, req)
	
	// Verify
	assert.True(t, response.Allowed)
	assert.Equal(t, 1, eventQueue.Size())
	
	// Verify the enqueued event has sanitized object
	events := eventQueue.DequeueAll()
	require.Equal(t, 1, len(events))
	
	event := events[0]
	sanitizedObj := event.Object
	
	// Verify preserved fields
	assert.Equal(t, "test-pod", sanitizedObj.GetName())
	assert.Equal(t, "default", sanitizedObj.GetNamespace())
	assert.Equal(t, "Pod", sanitizedObj.GetKind())
	
	// Verify spec is preserved
	spec, found, err := unstructured.NestedMap(sanitizedObj.Object, "spec")
	assert.True(t, found)
	assert.NoError(t, err)
	assert.NotNil(t, spec)
	
	// Verify status is removed
	_, found, err = unstructured.NestedMap(sanitizedObj.Object, "status")
	assert.False(t, found)
	assert.NoError(t, err)
	
	// Verify server-generated metadata fields are removed
	metadata, found, err := unstructured.NestedMap(sanitizedObj.Object, "metadata")
	require.True(t, found)
	require.NoError(t, err)
	
	_, exists := metadata["creationTimestamp"]
	assert.False(t, exists, "creationTimestamp should be removed by sanitization")
	_, exists = metadata["uid"]
	assert.False(t, exists, "uid should be removed by sanitization")
}