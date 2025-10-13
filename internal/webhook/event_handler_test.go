package webhook

import (
	"context"
	"testing"

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
	"github.com/ConfigButler/gitops-reverser/internal/eventqueue"
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
	eventQueue := eventqueue.NewQueue()

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
	assert.Equal(t, 1, eventQueue.Size())

	// Verify the enqueued event
	events := eventQueue.DequeueAll()
	require.Len(t, events, 1)

	event := events[0]
	assert.Equal(t, "test-pod", event.Object.GetName())
	assert.Equal(t, "default", event.Object.GetNamespace())
	assert.Equal(t, "Pod", event.Object.GetKind())
	assert.Equal(t, "test-repo-config", event.GitRepoConfigRef)
	assert.Equal(t, "test-user", event.UserInfo.Username)
	assert.Equal(t, "CREATE", event.Operation)
}

func TestEventHandler_Handle_NoMatchingRule(t *testing.T) {
	// Setup
	ctx := context.Background()
	_, err := metrics.InitOTLPExporter(ctx)
	require.NoError(t, err)

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
	assert.Equal(t, 0, eventQueue.Size()) // No events should be enqueued
}

func TestEventHandler_Handle_MultipleMatchingRules(t *testing.T) {
	// Setup
	ctx := context.Background()
	_, err := metrics.InitOTLPExporter(ctx)
	require.NoError(t, err)

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
	assert.Equal(t, 2, eventQueue.Size()) // Two events should be enqueued

	// Verify the enqueued events
	events := eventQueue.DequeueAll()
	require.Len(t, events, 2)

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
	ctx := context.Background()
	_, err := metrics.InitOTLPExporter(ctx)
	require.NoError(t, err)

	scheme := runtime.NewScheme()
	client := fake.NewClientBuilder().WithScheme(scheme).Build()

	ruleStore := rulestore.NewStore()
	eventQueue := eventqueue.NewQueue()

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
	assert.Equal(t, 0, eventQueue.Size()) // No events should be enqueued due to exclusion
}

func TestEventHandler_Handle_InvalidJSON(t *testing.T) {
	// Setup
	ctx := context.Background()
	_, err := metrics.InitOTLPExporter(ctx)
	require.NoError(t, err)

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
	response := handler.Handle(ctx, req)

	// Verify
	assert.False(t, response.Allowed)
	assert.Equal(t, int32(400), response.Result.Code) // Bad Request
	assert.Equal(t, 0, eventQueue.Size())             // No events should be enqueued
}

func TestEventHandler_Handle_NamespacedIngressResource(t *testing.T) {
	// Setup
	ctx := context.Background()
	_, err := metrics.InitOTLPExporter(ctx)
	require.NoError(t, err)

	scheme := runtime.NewScheme()
	client := fake.NewClientBuilder().WithScheme(scheme).Build()

	ruleStore := rulestore.NewStore()
	eventQueue := eventqueue.NewQueue()

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
		Client:     client,
		RuleStore:  ruleStore,
		EventQueue: eventQueue,
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

	// Verify
	assert.True(t, response.Allowed)
	assert.Equal(t, 1, eventQueue.Size())

	// Verify the enqueued event
	events := eventQueue.DequeueAll()
	require.Len(t, events, 1)

	event := events[0]
	assert.Equal(t, "test-ingress", event.Object.GetName())
	assert.Equal(t, "Ingress", event.Object.GetKind())
	assert.Equal(t, "default", event.Object.GetNamespace())
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
			eventQueue := eventqueue.NewQueue()

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
			assert.Equal(t, 1, eventQueue.Size())

			// Verify the operation is preserved in the event
			events := eventQueue.DequeueAll()
			require.Len(t, events, 1)

			event := events[0]
			assert.Equal(t, string(operation), event.Operation)
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
	eventQueue := eventqueue.NewQueue()

	// Add a WatchRule for namespaces (namespace-scoped rule watching cluster-scoped resource)
	// Note: WatchRule cannot watch cluster-scoped resources per the new design,
	// so this test should verify that NO events are enqueued
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
		Client:     client,
		RuleStore:  ruleStore,
		EventQueue: eventQueue,
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

	// Verify - WatchRule CANNOT watch cluster-scoped resources
	assert.True(t, response.Allowed)
	assert.Equal(t, 0, eventQueue.Size()) // No events should be enqueued - WatchRule can't watch cluster resources
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
	eventQueue := eventqueue.NewQueue()

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
	assert.Equal(t, 1, eventQueue.Size())

	// Verify the enqueued event has sanitized object
	events := eventQueue.DequeueAll()
	require.Len(t, events, 1)

	event := events[0]
	sanitizedObj := event.Object

	// Verify preserved fields
	assert.Equal(t, "test-pod", sanitizedObj.GetName())
	assert.Equal(t, "default", sanitizedObj.GetNamespace())
	assert.Equal(t, "Pod", sanitizedObj.GetKind())

	// Verify spec is preserved
	spec, found, err := unstructured.NestedMap(sanitizedObj.Object, "spec")
	assert.True(t, found)
	require.NoError(t, err)
	assert.NotNil(t, spec)

	// Verify status is removed
	_, found, err = unstructured.NestedMap(sanitizedObj.Object, "status")
	assert.False(t, found)
	require.NoError(t, err)

	// Verify server-generated metadata fields are removed
	metadata, found, err := unstructured.NestedMap(sanitizedObj.Object, "metadata")
	require.True(t, found)
	require.NoError(t, err)

	_, exists := metadata["creationTimestamp"]
	assert.False(t, exists, "creationTimestamp should be removed by sanitization")
	_, exists = metadata["uid"]
	assert.False(t, exists, "uid should be removed by sanitization")
}
