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

package correlation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ConfigButler/gitops-reverser/internal/sanitize"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// TestStatusOnlyChanges_CorrelationBehavior validates how correlation store handles
// multiple status-only updates that result in identical sanitized content.
func TestStatusOnlyChanges_CorrelationBehavior(t *testing.T) {
	store := NewStore(60*time.Second, 1000)

	// Base object (spec that won't change)
	baseObj := &unstructured.Unstructured{
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
						"name":  "app",
						"image": "nginx:1.0",
					},
				},
			},
		},
	}

	id := types.ResourceIdentifier{
		Group:     "",
		Version:   "v1",
		Resource:  "pods",
		Namespace: "default",
		Name:      "test-pod",
	}

	// Simulate 5 status-only updates by different users
	statusUpdates := []struct {
		status   string
		username string
	}{
		{"Pending", "user1@example.com"},
		{"Running", "user2@example.com"},
		{"Running", "user3@example.com"}, // Same status, different user
		{"Terminating", "user4@example.com"},
		{"Terminated", "user5@example.com"},
	}

	var correlationKeys []string
	var sanitizedYAMLs [][]byte

	t.Log("=== Phase 1: Webhook stores correlation entries ===")
	for i, update := range statusUpdates {
		// Create object with different status
		obj := baseObj.DeepCopy()
		unstructured.SetNestedField(obj.Object, update.status, "status", "phase")

		// Sanitize (removes status)
		sanitized := sanitize.Sanitize(obj)
		yaml, err := sanitize.MarshalToOrderedYAML(sanitized)
		require.NoError(t, err)
		sanitizedYAMLs = append(sanitizedYAMLs, yaml)

		// Generate correlation key
		key := GenerateKey(id, "UPDATE", yaml)
		correlationKeys = append(correlationKeys, key)

		// Store in correlation (like webhook does)
		store.Put(key, update.username)

		keyDisplay := key
		if len(key) > 50 {
			keyDisplay = key[:50] + "..."
		}
		t.Logf("  Update %d: status=%s, username=%s, key=%s",
			i+1, update.status, update.username, keyDisplay)
	}

	t.Log("")
	t.Log("=== Analysis: Correlation Key Uniqueness ===")
	// Check if all sanitized YAMLs are identical (they should be - status removed)
	allIdentical := true
	for i := 1; i < len(sanitizedYAMLs); i++ {
		if string(sanitizedYAMLs[i]) != string(sanitizedYAMLs[0]) {
			allIdentical = false
			t.Logf("  YAML %d differs from YAML 0", i)
		}
	}

	if allIdentical {
		t.Log("  ✓ All sanitized YAMLs are IDENTICAL (status removed)")
		t.Log("  ✓ This means all correlation keys are THE SAME")

		// Verify all keys are identical
		for i := 1; i < len(correlationKeys); i++ {
			assert.Equal(t, correlationKeys[0], correlationKeys[i],
				"All status-only updates should produce same correlation key")
		}
	} else {
		t.Log("  ✗ Sanitized YAMLs differ (unexpected)")
	}

	t.Log("")
	t.Log("=== Phase 2: Correlation Store State ===")
	t.Logf("  Total correlation entries: %d", store.Size())
	t.Log("  Expected: 5 entries queued under single key (FIFO)")

	// Since all keys are identical, store should have:
	// - 1 unique key
	// - 5 entries in the queue for that key
	assert.Equal(t, 5, store.Size(), "all 5 status updates queued under same key")

	t.Log("")
	t.Log("=== Phase 3: Watch informer fires (simulating actual cluster changes) ===")

	// Simulate watch informer firing for each status change
	var retrievedUsernames []string
	for i := range 5 {
		// Watch sees the change and tries to enrich
		entry, found := store.GetAndDelete(correlationKeys[0])

		if found {
			retrievedUsernames = append(retrievedUsernames, entry.Username)
			t.Logf("  Watch event %d: enriched with username=%s", i+1, entry.Username)
		} else {
			t.Logf("  Watch event %d: NO correlation found (miss)", i+1)
			retrievedUsernames = append(retrievedUsernames, "")
		}
	}

	t.Log("")
	t.Log("=== Results ===")
	t.Logf("  Retrieved usernames: %v", retrievedUsernames)
	t.Log("")

	// Verify FIFO behavior
	expectedUsernames := []string{
		"user1@example.com", // First in
		"user2@example.com",
		"user3@example.com",
		"user4@example.com",
		"user5@example.com", // Last in, last out (FIFO)
	}

	assert.Equal(t, expectedUsernames, retrievedUsernames,
		"Correlation queue should use FIFO ordering")

	// Store should now be empty
	assert.Equal(t, 0, store.Size(), "all correlation entries consumed")

	t.Log("")
	t.Log("=== Conclusion ===")
	t.Log("  • Webhook creates 5 correlation entries (same key, queued FIFO)")
	t.Log("  • Watch fires 5 times (one per actual status change)")
	t.Log("  • Each watch event pops the oldest correlation entry")
	t.Log("  • Result: 5 events enqueued, each with correct username")
	t.Log("")
	t.Log("  PROBLEM: Status-only changes create duplicate commits!")
	t.Log("  • Same sanitized content (spec unchanged)")
	t.Log("  • But watch fires for each status transition")
	t.Log("  • Creates 5 identical YAML files in Git")
	t.Log("")
	t.Log("  SOLUTION NEEDED: Deduplication at watch level")
}

// TestStatusOnlyChanges_WatchDeduplication tests potential deduplication logic.
func TestStatusOnlyChanges_WatchDeduplication(t *testing.T) {
	// This test demonstrates what SHOULD happen with deduplication

	type watchEvent struct {
		sanitizedHash string
		username      string
	}

	// Simulate watch events with content tracking
	var events []watchEvent
	lastSeenHash := make(map[string]string) // resource ID → last content hash

	baseObj := &unstructured.Unstructured{
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
						"name":  "app",
						"image": "nginx:1.0",
					},
				},
			},
		},
	}

	resourceKey := "default/test-pod"

	t.Log("=== Simulating 5 status changes ===")
	statuses := []string{"Pending", "Running", "Running", "Terminating", "Terminated"}
	usernames := []string{"user1", "user2", "user3", "user4", "user5"}

	for i, status := range statuses {
		obj := baseObj.DeepCopy()
		unstructured.SetNestedField(obj.Object, status, "status", "phase")

		sanitized := sanitize.Sanitize(obj)
		yaml, _ := sanitize.MarshalToOrderedYAML(sanitized)
		hash := string(yaml) // In reality, would use xxhash

		t.Logf("  Update %d: status=%s, user=%s", i+1, status, usernames[i])

		// Deduplication logic
		if lastHash, seen := lastSeenHash[resourceKey]; seen && lastHash == hash {
			t.Logf("    → SKIPPED: identical to previous sanitized content")
			continue
		}

		// New content - enqueue it
		events = append(events, watchEvent{
			sanitizedHash: hash,
			username:      usernames[i],
		})
		lastSeenHash[resourceKey] = hash
		t.Logf("    → ENQUEUED: new sanitized content")
	}

	t.Log("")
	t.Log("=== Results with Deduplication ===")
	t.Logf("  Total watch callbacks: 5")
	t.Logf("  Events actually enqueued: %d", len(events))
	t.Logf("  Duplicates skipped: %d", 5-len(events))

	// With status-only changes, ALL should be identical after sanitization
	assert.Len(t, events, 1, "only first event should be enqueued")
	assert.Equal(t, "user1", events[0].username, "first user's change is recorded")

	t.Log("")
	t.Log("  ✓ Deduplication prevents redundant commits for status-only changes")
}

// TestSpecChanges_NoDeduplication validates that spec changes are NOT deduplicated.
func TestSpecChanges_NoDeduplication(t *testing.T) {
	lastSeenHash := make(map[string]string)
	resourceKey := "default/test-pod"

	baseObj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata": map[string]interface{}{
				"name":      "test-pod",
				"namespace": "default",
			},
		},
	}

	t.Log("=== Simulating 3 spec changes ===")
	specs := []map[string]interface{}{
		{"containers": []interface{}{map[string]interface{}{"name": "app", "image": "nginx:1.0"}}},
		{"containers": []interface{}{map[string]interface{}{"name": "app", "image": "nginx:1.1"}}},
		{"containers": []interface{}{map[string]interface{}{"name": "app", "image": "nginx:1.2"}}},
	}

	enqueueCount := 0
	for i, spec := range specs {
		obj := baseObj.DeepCopy()
		obj.Object["spec"] = spec
		unstructured.SetNestedField(obj.Object, "Running", "status", "phase")

		sanitized := sanitize.Sanitize(obj)
		yaml, _ := sanitize.MarshalToOrderedYAML(sanitized)
		hash := string(yaml)

		t.Logf("  Update %d: image=%s", i+1, spec["containers"].([]interface{})[0].(map[string]interface{})["image"])

		// Deduplication check
		if lastHash, seen := lastSeenHash[resourceKey]; seen && lastHash == hash {
			t.Logf("    → SKIPPED: duplicate content")
			continue
		}

		enqueueCount++
		lastSeenHash[resourceKey] = hash
		t.Logf("    → ENQUEUED: new content detected")
	}

	t.Log("")
	t.Logf("  Events enqueued: %d/3", enqueueCount)
	assert.Equal(t, 3, enqueueCount, "all spec changes should be enqueued")

	t.Log("  ✓ Spec changes are NOT deduplicated (correct behavior)")
}
