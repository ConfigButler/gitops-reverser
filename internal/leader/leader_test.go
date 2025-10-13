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

package leader

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

func TestPodLabeler_Start_AddLabel(t *testing.T) {
	// Setup
	scheme := runtime.NewScheme()
	err := corev1.AddToScheme(scheme)
	require.NoError(t, err)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "test-namespace",
			Labels:    map[string]string{},
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pod).
		Build()

	logger := zap.New(zap.UseDevMode(true))
	labeler := &PodLabeler{
		Client:    client,
		Log:       logger,
		PodName:   "test-pod",
		Namespace: "test-namespace",
	}

	// Create a context that will be canceled after a short time
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// Execute
	err = labeler.Start(ctx)
	require.NoError(t, err)

	// Verify the label was added
	updatedPod := &corev1.Pod{}
	err = client.Get(context.Background(), types.NamespacedName{
		Name:      "test-pod",
		Namespace: "test-namespace",
	}, updatedPod)
	require.NoError(t, err)

	// The label should have been added and then removed during shutdown
	// Since we can't easily test the intermediate state, we verify the cleanup happened
	assert.NotContains(t, updatedPod.Labels, leaderLabelKey)
}

func TestPodLabeler_Start_PodNotFound(t *testing.T) {
	// Setup - no pod in the fake client
	scheme := runtime.NewScheme()
	err := corev1.AddToScheme(scheme)
	require.NoError(t, err)

	client := fake.NewClientBuilder().WithScheme(scheme).Build()

	logger := zap.New(zap.UseDevMode(true))
	labeler := &PodLabeler{
		Client:    client,
		Log:       logger,
		PodName:   "non-existent-pod",
		Namespace: "test-namespace",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// Execute
	err = labeler.Start(ctx)
	require.Error(t, err)
	assert.True(t, errors.IsNotFound(err))
}

func TestPodLabeler_addLabel_NewLabel(t *testing.T) {
	// Setup
	scheme := runtime.NewScheme()
	err := corev1.AddToScheme(scheme)
	require.NoError(t, err)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "test-namespace",
			Labels:    map[string]string{},
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pod).
		Build()

	logger := zap.New(zap.UseDevMode(true))
	labeler := &PodLabeler{
		Client:    client,
		Log:       logger,
		PodName:   "test-pod",
		Namespace: "test-namespace",
	}

	// Execute
	ctx := context.Background()
	err = labeler.addLabel(ctx, logger)
	require.NoError(t, err)

	// Verify
	updatedPod := &corev1.Pod{}
	err = client.Get(ctx, types.NamespacedName{
		Name:      "test-pod",
		Namespace: "test-namespace",
	}, updatedPod)
	require.NoError(t, err)

	assert.Equal(t, leaderLabelValue, updatedPod.Labels[leaderLabelKey])
}

func TestPodLabeler_addLabel_ExistingLabel(t *testing.T) {
	// Setup
	scheme := runtime.NewScheme()
	err := corev1.AddToScheme(scheme)
	require.NoError(t, err)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "test-namespace",
			Labels: map[string]string{
				leaderLabelKey: leaderLabelValue, // Already has the leader label
			},
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pod).
		Build()

	logger := zap.New(zap.UseDevMode(true))
	labeler := &PodLabeler{
		Client:    client,
		Log:       logger,
		PodName:   "test-pod",
		Namespace: "test-namespace",
	}

	// Execute
	ctx := context.Background()
	err = labeler.addLabel(ctx, logger)
	require.NoError(t, err)

	// Verify the label is still there (no error should occur)
	updatedPod := &corev1.Pod{}
	err = client.Get(ctx, types.NamespacedName{
		Name:      "test-pod",
		Namespace: "test-namespace",
	}, updatedPod)
	require.NoError(t, err)

	assert.Equal(t, leaderLabelValue, updatedPod.Labels[leaderLabelKey])
}

func TestPodLabeler_addLabel_NilLabels(t *testing.T) {
	// Setup
	scheme := runtime.NewScheme()
	err := corev1.AddToScheme(scheme)
	require.NoError(t, err)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "test-namespace",
			Labels:    nil, // Nil labels map
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pod).
		Build()

	logger := zap.New(zap.UseDevMode(true))
	labeler := &PodLabeler{
		Client:    client,
		Log:       logger,
		PodName:   "test-pod",
		Namespace: "test-namespace",
	}

	// Execute
	ctx := context.Background()
	err = labeler.addLabel(ctx, logger)
	require.NoError(t, err)

	// Verify
	updatedPod := &corev1.Pod{}
	err = client.Get(ctx, types.NamespacedName{
		Name:      "test-pod",
		Namespace: "test-namespace",
	}, updatedPod)
	require.NoError(t, err)

	assert.NotNil(t, updatedPod.Labels)
	assert.Equal(t, leaderLabelValue, updatedPod.Labels[leaderLabelKey])
}

func TestPodLabeler_removeLabel_ExistingLabel(t *testing.T) {
	// Setup
	scheme := runtime.NewScheme()
	err := corev1.AddToScheme(scheme)
	require.NoError(t, err)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "test-namespace",
			Labels: map[string]string{
				leaderLabelKey: leaderLabelValue,
				"other-label":  "other-value",
			},
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pod).
		Build()

	logger := zap.New(zap.UseDevMode(true))
	labeler := &PodLabeler{
		Client:    client,
		Log:       logger,
		PodName:   "test-pod",
		Namespace: "test-namespace",
	}

	// Execute
	ctx := context.Background()
	err = labeler.removeLabel(ctx, logger)
	require.NoError(t, err)

	// Verify
	updatedPod := &corev1.Pod{}
	err = client.Get(ctx, types.NamespacedName{
		Name:      "test-pod",
		Namespace: "test-namespace",
	}, updatedPod)
	require.NoError(t, err)

	assert.NotContains(t, updatedPod.Labels, leaderLabelKey)
	assert.Equal(t, "other-value", updatedPod.Labels["other-label"]) // Other labels preserved
}

func TestPodLabeler_removeLabel_NoLabel(t *testing.T) {
	// Setup
	scheme := runtime.NewScheme()
	err := corev1.AddToScheme(scheme)
	require.NoError(t, err)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "test-namespace",
			Labels: map[string]string{
				"other-label": "other-value",
			},
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pod).
		Build()

	logger := zap.New(zap.UseDevMode(true))
	labeler := &PodLabeler{
		Client:    client,
		Log:       logger,
		PodName:   "test-pod",
		Namespace: "test-namespace",
	}

	// Execute
	ctx := context.Background()
	err = labeler.removeLabel(ctx, logger)
	require.NoError(t, err)

	// Verify - should be no-op
	updatedPod := &corev1.Pod{}
	err = client.Get(ctx, types.NamespacedName{
		Name:      "test-pod",
		Namespace: "test-namespace",
	}, updatedPod)
	require.NoError(t, err)

	assert.NotContains(t, updatedPod.Labels, leaderLabelKey)
	assert.Equal(t, "other-value", updatedPod.Labels["other-label"])
}

func TestPodLabeler_removeLabel_PodNotFound(t *testing.T) {
	// Setup - no pod in the fake client
	scheme := runtime.NewScheme()
	err := corev1.AddToScheme(scheme)
	require.NoError(t, err)

	client := fake.NewClientBuilder().WithScheme(scheme).Build()

	logger := zap.New(zap.UseDevMode(true))
	labeler := &PodLabeler{
		Client:    client,
		Log:       logger,
		PodName:   "non-existent-pod",
		Namespace: "test-namespace",
	}

	// Execute
	ctx := context.Background()
	err = labeler.removeLabel(ctx, logger)
	require.NoError(t, err) // Should not error when pod is not found during cleanup
}

func TestPodLabeler_getPod_Success(t *testing.T) {
	// Setup
	scheme := runtime.NewScheme()
	err := corev1.AddToScheme(scheme)
	require.NoError(t, err)

	expectedPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "test-namespace",
			Labels: map[string]string{
				"test-label": "test-value",
			},
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(expectedPod).
		Build()

	labeler := &PodLabeler{
		Client:    client,
		PodName:   "test-pod",
		Namespace: "test-namespace",
	}

	// Execute
	ctx := context.Background()
	pod, err := labeler.getPod(ctx)
	require.NoError(t, err)
	assert.NotNil(t, pod)
	assert.Equal(t, "test-pod", pod.Name)
	assert.Equal(t, "test-namespace", pod.Namespace)
	assert.Equal(t, "test-value", pod.Labels["test-label"])
}

func TestPodLabeler_getPod_NotFound(t *testing.T) {
	// Setup - no pod in the fake client
	scheme := runtime.NewScheme()
	err := corev1.AddToScheme(scheme)
	require.NoError(t, err)

	client := fake.NewClientBuilder().WithScheme(scheme).Build()

	labeler := &PodLabeler{
		Client:    client,
		PodName:   "non-existent-pod",
		Namespace: "test-namespace",
	}

	// Execute
	ctx := context.Background()
	pod, err := labeler.getPod(ctx)
	require.Error(t, err)
	assert.True(t, errors.IsNotFound(err))
	assert.NotNil(t, pod) // getPod always returns a Pod object, even when not found
}

func TestGetPodName(t *testing.T) {
	// Test with environment variable set
	t.Setenv("POD_NAME", "test-pod-name")

	podName := GetPodName()
	assert.Equal(t, "test-pod-name", podName)
}

func TestGetPodName_Empty(t *testing.T) {
	// Test with environment variable unset
	t.Setenv("POD_NAME", "")

	podName := GetPodName()
	assert.Empty(t, podName)
}

func TestGetPodNamespace(t *testing.T) {
	// Test with environment variable set
	t.Setenv("POD_NAMESPACE", "test-namespace")

	podNamespace := GetPodNamespace()
	assert.Equal(t, "test-namespace", podNamespace)
}

func TestGetPodNamespace_Empty(t *testing.T) {
	// Test with environment variable unset
	t.Setenv("POD_NAMESPACE", "")

	podNamespace := GetPodNamespace()
	assert.Empty(t, podNamespace)
}

func TestLeaderLabelConstants(t *testing.T) {
	// Verify the constants are set correctly
	assert.Equal(t, "role", leaderLabelKey)
	assert.Equal(t, "leader", leaderLabelValue)
}

func TestPodLabeler_ConcurrentOperations(t *testing.T) {
	// Setup
	scheme := runtime.NewScheme()
	err := corev1.AddToScheme(scheme)
	require.NoError(t, err)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "test-namespace",
			Labels:    map[string]string{},
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pod).
		Build()

	logger := zap.New(zap.UseDevMode(true))
	labeler := &PodLabeler{
		Client:    client,
		Log:       logger,
		PodName:   "test-pod",
		Namespace: "test-namespace",
	}

	ctx := context.Background()

	// Execute concurrent add operations
	done := make(chan error, 2)

	go func() {
		done <- labeler.addLabel(ctx, logger)
	}()

	go func() {
		done <- labeler.addLabel(ctx, logger)
	}()

	// Wait for both operations to complete
	err1 := <-done
	err2 := <-done

	// Both should succeed (or at least one should succeed)
	assert.True(t, err1 == nil || err2 == nil, "At least one add operation should succeed")

	// Verify final state
	updatedPod := &corev1.Pod{}
	err = client.Get(ctx, types.NamespacedName{
		Name:      "test-pod",
		Namespace: "test-namespace",
	}, updatedPod)
	require.NoError(t, err)

	assert.Equal(t, leaderLabelValue, updatedPod.Labels[leaderLabelKey])
}

func TestPodLabeler_AddRemoveCycle(t *testing.T) {
	// Setup
	scheme := runtime.NewScheme()
	err := corev1.AddToScheme(scheme)
	require.NoError(t, err)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "test-namespace",
			Labels:    map[string]string{},
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pod).
		Build()

	logger := zap.New(zap.UseDevMode(true))
	labeler := &PodLabeler{
		Client:    client,
		Log:       logger,
		PodName:   "test-pod",
		Namespace: "test-namespace",
	}

	ctx := context.Background()

	// Add label
	err = labeler.addLabel(ctx, logger)
	require.NoError(t, err)

	// Verify label was added
	updatedPod := &corev1.Pod{}
	err = client.Get(ctx, types.NamespacedName{
		Name:      "test-pod",
		Namespace: "test-namespace",
	}, updatedPod)
	require.NoError(t, err)
	assert.Equal(t, leaderLabelValue, updatedPod.Labels[leaderLabelKey])

	// Remove label
	err = labeler.removeLabel(ctx, logger)
	require.NoError(t, err)

	// Verify label was removed
	err = client.Get(ctx, types.NamespacedName{
		Name:      "test-pod",
		Namespace: "test-namespace",
	}, updatedPod)
	require.NoError(t, err)
	assert.NotContains(t, updatedPod.Labels, leaderLabelKey)

	// Add label again
	err = labeler.addLabel(ctx, logger)
	require.NoError(t, err)

	// Verify label was added again
	err = client.Get(ctx, types.NamespacedName{
		Name:      "test-pod",
		Namespace: "test-namespace",
	}, updatedPod)
	require.NoError(t, err)
	assert.Equal(t, leaderLabelValue, updatedPod.Labels[leaderLabelKey])
}

func TestPodLabeler_WithExistingLabels(t *testing.T) {
	// Setup
	scheme := runtime.NewScheme()
	err := corev1.AddToScheme(scheme)
	require.NoError(t, err)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "test-namespace",
			Labels: map[string]string{
				"app":         "my-app",
				"version":     "v1.0.0",
				"environment": "production",
			},
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pod).
		Build()

	logger := zap.New(zap.UseDevMode(true))
	labeler := &PodLabeler{
		Client:    client,
		Log:       logger,
		PodName:   "test-pod",
		Namespace: "test-namespace",
	}

	ctx := context.Background()

	// Add leader label
	err = labeler.addLabel(ctx, logger)
	require.NoError(t, err)

	// Verify all labels are preserved
	updatedPod := &corev1.Pod{}
	err = client.Get(ctx, types.NamespacedName{
		Name:      "test-pod",
		Namespace: "test-namespace",
	}, updatedPod)
	require.NoError(t, err)

	expectedLabels := map[string]string{
		"app":          "my-app",
		"version":      "v1.0.0",
		"environment":  "production",
		leaderLabelKey: leaderLabelValue,
	}

	assert.Equal(t, expectedLabels, updatedPod.Labels)

	// Remove leader label
	err = labeler.removeLabel(ctx, logger)
	require.NoError(t, err)

	// Verify only leader label was removed
	err = client.Get(ctx, types.NamespacedName{
		Name:      "test-pod",
		Namespace: "test-namespace",
	}, updatedPod)
	require.NoError(t, err)

	expectedLabelsAfterRemoval := map[string]string{
		"app":         "my-app",
		"version":     "v1.0.0",
		"environment": "production",
	}

	assert.Equal(t, expectedLabelsAfterRemoval, updatedPod.Labels)
	assert.NotContains(t, updatedPod.Labels, leaderLabelKey)
}

func TestPodLabeler_ContextCancellation(t *testing.T) {
	// Setup
	scheme := runtime.NewScheme()
	err := corev1.AddToScheme(scheme)
	require.NoError(t, err)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "test-namespace",
			Labels:    map[string]string{},
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pod).
		Build()

	logger := zap.New(zap.UseDevMode(true))
	labeler := &PodLabeler{
		Client:    client,
		Log:       logger,
		PodName:   "test-pod",
		Namespace: "test-namespace",
	}

	// Create a context that gets canceled immediately
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	// Execute - Start should handle the canceled context gracefully
	err = labeler.Start(ctx)
	require.NoError(t, err) // Should not error, just exit cleanly
}
