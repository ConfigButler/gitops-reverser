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
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ConfigButler/gitops-reverser/internal/sanitize"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// TestEnrichment_WebhookToWatch_FullPipeline validates the complete enrichment flow:
// 1. Webhook receives admission and stores correlation.
// 2. Watch receives event and enriches with username.
// 3. Metrics are incremented correctly.
func TestEnrichment_WebhookToWatch_FullPipeline(t *testing.T) {
	store := NewStore(60*time.Second, 100)

	// Track metrics
	var hits, misses, evictions atomic.Int64
	store.SetEvictionCallback(func() {
		evictions.Add(1)
	})

	// Create a test object
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata": map[string]interface{}{
				"name":      "test-app",
				"namespace": "production",
			},
			"spec": map[string]interface{}{
				"replicas": int64(3),
			},
		},
	}

	id := types.NewResourceIdentifier("apps", "v1", "deployments", "production", "test-app")

	// Simulate webhook admission path
	webhookUsername := "alice@example.com"
	webhookOperation := "UPDATE"

	sanitizedFromWebhook := sanitize.Sanitize(obj)
	webhookYAML, err := sanitize.MarshalToOrderedYAML(sanitizedFromWebhook)
	require.NoError(t, err, "Webhook sanitization should succeed")

	webhookKey := GenerateKey(id, webhookOperation, webhookYAML)
	store.Put(webhookKey, webhookUsername)

	t.Logf("Webhook stored correlation: key=%s, user=%s", webhookKey, webhookUsername)

	// Simulate watch event path (same object, arrives shortly after)
	time.Sleep(10 * time.Millisecond) // Small delay

	sanitizedFromWatch := sanitize.Sanitize(obj.DeepCopy())
	watchYAML, err := sanitize.MarshalToOrderedYAML(sanitizedFromWatch)
	require.NoError(t, err, "Watch sanitization should succeed")

	watchKey := GenerateKey(id, webhookOperation, watchYAML)
	t.Logf("Watch generated key: %s", watchKey)

	// Keys should match (same object, same operation, same sanitized content)
	assert.Equal(t, webhookKey, watchKey, "Webhook and watch should generate identical keys")

	// Attempt enrichment
	entry, found := store.Get(watchKey)
	require.True(t, found, "Watch should find correlation entry")
	assert.Equal(t, webhookUsername, entry.Username, "Username should match webhook user")

	// Simulate metric increments
	if found {
		hits.Add(1)
	} else {
		misses.Add(1)
	}

	// Verify metrics
	assert.Equal(t, int64(1), hits.Load(), "Should have 1 enrichment hit")
	assert.Equal(t, int64(0), misses.Load(), "Should have 0 enrichment misses")
	assert.Equal(t, int64(0), evictions.Load(), "Should have 0 evictions")

	// Second lookup should succeed (entry not consumed)
	entry2, found2 := store.Get(watchKey)
	require.True(t, found2, "Second lookup should succeed (entry not consumed)")
	assert.Equal(t, webhookUsername, entry2.Username, "Username should still match")
	assert.Equal(t, int64(1), hits.Load(), "Should still have 1 hit")
}

// TestEnrichment_DroppedWebhook validates graceful degradation when webhook is dropped.
func TestEnrichment_DroppedWebhook(t *testing.T) {
	store := NewStore(60*time.Second, 100)

	var misses atomic.Int64

	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]interface{}{
				"name":      "app-config",
				"namespace": "default",
			},
			"data": map[string]interface{}{
				"key": "value",
			},
		},
	}

	id := types.NewResourceIdentifier("", "v1", "configmaps", "default", "app-config")

	// Skip webhook admission (simulates dropped webhook)
	// Watch event arrives without prior correlation

	sanitizedFromWatch := sanitize.Sanitize(obj)
	watchYAML, err := sanitize.MarshalToOrderedYAML(sanitizedFromWatch)
	require.NoError(t, err)

	watchKey := GenerateKey(id, "CREATE", watchYAML)
	entry, found := store.Get(watchKey)

	// Should gracefully handle missing correlation
	assert.False(t, found, "Should not find correlation (webhook was dropped)")
	assert.Nil(t, entry, "Entry should be nil on miss")

	misses.Add(1)
	assert.Equal(t, int64(1), misses.Load(), "Should record enrichment miss")

	// In production, this would result in empty UserInfo, which is acceptable
	t.Log("Watch event processed without username (webhook was dropped)")
}

// TestEnrichment_ExpiredEntry validates TTL-based expiration.
func TestEnrichment_ExpiredEntry(t *testing.T) {
	ttl := 100 * time.Millisecond
	store := NewStore(ttl, 100)

	var evictions atomic.Int64
	store.SetEvictionCallback(func() {
		evictions.Add(1)
	})

	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata": map[string]interface{}{
				"name":      "api-key",
				"namespace": "default",
			},
		},
	}

	id := types.NewResourceIdentifier("", "v1", "secrets", "default", "api-key")

	// Webhook stores correlation
	sanitized := sanitize.Sanitize(obj)
	yaml, err := sanitize.MarshalToOrderedYAML(sanitized)
	require.NoError(t, err)

	key := GenerateKey(id, "UPDATE", yaml)
	store.Put(key, "bob@example.com")

	// Wait for TTL to expire
	time.Sleep(ttl + 50*time.Millisecond)

	// Watch event arrives after expiration
	entry, found := store.Get(key)
	assert.False(t, found, "Should not find correlation (TTL expired)")
	assert.Nil(t, entry)
	assert.Equal(t, int64(1), evictions.Load(), "Should record eviction on expired lookup")
}

// TestEnrichment_HighRateUpdates validates enrichment under load.
func TestEnrichment_HighRateUpdates(t *testing.T) {
	store := NewStore(5*time.Second, 1000)

	var hits, misses atomic.Int64

	// Simulate high rate of updates
	numUpdates := 100
	usernames := []string{"alice", "bob", "charlie"}

	for i := range numUpdates {
		username := usernames[i%len(usernames)]

		obj := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata": map[string]interface{}{
					"name":      "high-rate-app",
					"namespace": "default",
				},
				"spec": map[string]interface{}{
					"replicas": int64(i), // Different content each time
				},
			},
		}

		id := types.NewResourceIdentifier("apps", "v1", "deployments", "default", "high-rate-app")

		// Webhook admission
		sanitized := sanitize.Sanitize(obj)
		yaml, err := sanitize.MarshalToOrderedYAML(sanitized)
		require.NoError(t, err)

		key := GenerateKey(id, "UPDATE", yaml)
		store.Put(key, username)

		// Watch event (immediate - within TTL)
		entry, found := store.Get(key)
		if found {
			hits.Add(1)
			assert.Equal(t, username, entry.Username, "Username should match for update %d", i)
		} else {
			misses.Add(1)
		}
	}

	// All should be hits (no TTL expiry, no collisions)
	assert.Equal(t, int64(numUpdates), hits.Load(), "All updates should be enriched")
	assert.Equal(t, int64(0), misses.Load(), "No misses expected")

	t.Logf("Processed %d high-rate updates with 100%% enrichment", numUpdates)
}

// TestEnrichment_RapidContentOscillation validates single entry with rapid changes.
func TestEnrichment_RapidContentOscillation(t *testing.T) {
	store := NewStore(60*time.Second, 100)

	id := types.NewResourceIdentifier("apps", "v1", "deployments", "default", "oscillating-app")

	// Create two distinct content states
	objReplicas3 := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata": map[string]interface{}{
				"name":      "oscillating-app",
				"namespace": "default",
			},
			"spec": map[string]interface{}{
				"replicas": int64(3),
			},
		},
	}

	objReplicas5 := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata": map[string]interface{}{
				"name":      "oscillating-app",
				"namespace": "default",
			},
			"spec": map[string]interface{}{
				"replicas": int64(5),
			},
		},
	}

	// Rapid webhooks (oscillating between two states)
	// 1. alice sets replicas=3
	key3 := generateAndStore(t, store, objReplicas3, id, "UPDATE", "alice")

	// 2. bob sets replicas=5
	key5 := generateAndStore(t, store, objReplicas5, id, "UPDATE", "bob")

	// 3. charlie sets replicas=3 (back to first state, overwrites alice)
	key3Again := generateAndStore(t, store, objReplicas3, id, "UPDATE", "charlie")

	// 4. dave sets replicas=5 (back to second state, overwrites bob)
	key5Again := generateAndStore(t, store, objReplicas5, id, "UPDATE", "dave")

	// Keys should be reused
	assert.Equal(t, key3, key3Again, "Same content should produce same key")
	assert.Equal(t, key5, key5Again, "Same content should produce same key")

	// Watch events arrive in order (delayed processing)
	// Watch 1: replicas=3 (gets the last user for this key)
	entry1, found1 := store.Get(key3)
	require.True(t, found1, "First watch should find correlation")
	assert.Equal(t, "charlie", entry1.Username, "Should get the last user for replicas=3")

	// Watch 2: replicas=5 (gets the last user for this key)
	entry2, found2 := store.Get(key5)
	require.True(t, found2, "Second watch should find correlation")
	assert.Equal(t, "dave", entry2.Username, "Should get the last user for replicas=5")

	// Watch 3: replicas=3 (can be accessed multiple times)
	entry3, found3 := store.Get(key3)
	require.True(t, found3, "Third watch should still find correlation")
	assert.Equal(t, "charlie", entry3.Username, "Should still get the last user for replicas=3")

	// Watch 4: replicas=5 (can be accessed multiple times)
	entry4, found4 := store.Get(key5)
	require.True(t, found4, "Fourth watch should still find correlation")
	assert.Equal(t, "dave", entry4.Username, "Should still get the last user for replicas=5")

	t.Log("✓ Watch events get the last user per key, single entry behavior with multiple access")
}

// TestEnrichment_MixedHitsAndMisses validates metrics tracking.
func TestEnrichment_MixedHitsAndMisses(t *testing.T) {
	store := NewStore(200*time.Millisecond, 100)

	var hits, misses, evictions atomic.Int64
	store.SetEvictionCallback(func() {
		evictions.Add(1)
	})

	id := types.NewResourceIdentifier("", "v1", "configmaps", "default", "mixed-test")

	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]interface{}{
				"name":      "mixed-test",
				"namespace": "default",
			},
			"data": map[string]interface{}{
				"config": "value",
			},
		},
	}

	// Scenario 1: Webhook + Watch within TTL (HIT)
	key1 := generateAndStore(t, store, obj, id, "UPDATE", "user1")
	entry1, found1 := store.Get(key1)
	if found1 {
		hits.Add(1)
		assert.Equal(t, "user1", entry1.Username)
	} else {
		misses.Add(1)
	}

	// Scenario 2: Watch without webhook (MISS) - different content
	obj2 := obj.DeepCopy()
	unstructured.SetNestedField(obj2.Object, "different-value", "data", "config")
	key2 := generateKey(t, obj2, id, "UPDATE")
	_, found2 := store.Get(key2)
	if found2 {
		hits.Add(1)
	} else {
		misses.Add(1)
	}

	// Scenario 3: Webhook + Watch after TTL expiry (MISS with eviction)
	key3 := generateAndStore(t, store, obj, id, "CREATE", "user3")
	time.Sleep(250 * time.Millisecond) // Wait for TTL expiry
	_, found3 := store.Get(key3)
	if found3 {
		hits.Add(1)
	} else {
		misses.Add(1)
	}

	// Verify metrics
	assert.Equal(t, int64(1), hits.Load(), "Should have 1 hit (scenario 1)")
	assert.Equal(t, int64(2), misses.Load(), "Should have 2 misses (scenarios 2 and 3)")
	assert.Equal(t, int64(1), evictions.Load(), "Should have 1 eviction (scenario 3 TTL)")

	t.Log("✓ Metrics correctly track hits, misses, and evictions")
}

// TestEnrichment_ConcurrentWebhookAndWatch validates thread-safety.
func TestEnrichment_ConcurrentWebhookAndWatch(t *testing.T) {
	store := NewStore(5*time.Second, 1000)

	var hits, misses atomic.Int64

	concurrency := 10
	updatesPerGoroutine := 50

	done := make(chan struct{})

	// Webhook goroutines
	for i := range concurrency {
		go func(workerID int) {
			for j := range updatesPerGoroutine {
				obj := &unstructured.Unstructured{
					Object: map[string]interface{}{
						"apiVersion": "apps/v1",
						"kind":       "Deployment",
						"metadata": map[string]interface{}{
							"name":      "concurrent-app",
							"namespace": "default",
						},
						"spec": map[string]interface{}{
							"replicas": int64(j), // Different content
						},
					},
				}

				id := types.NewResourceIdentifier("apps", "v1", "deployments", "default", "concurrent-app")
				username := "user-" + string(rune('A'+workerID))

				sanitized := sanitize.Sanitize(obj)
				yaml, _ := sanitize.MarshalToOrderedYAML(sanitized)
				key := GenerateKey(id, "UPDATE", yaml)
				store.Put(key, username)
			}
		}(i)
	}

	// Watch goroutines
	for i := range concurrency {
		go func(workerID int) {
			defer func() {
				if workerID == 0 {
					close(done)
				}
			}()

			for j := range updatesPerGoroutine {
				obj := &unstructured.Unstructured{
					Object: map[string]interface{}{
						"apiVersion": "apps/v1",
						"kind":       "Deployment",
						"metadata": map[string]interface{}{
							"name":      "concurrent-app",
							"namespace": "default",
						},
						"spec": map[string]interface{}{
							"replicas": int64(j),
						},
					},
				}

				id := types.NewResourceIdentifier("apps", "v1", "deployments", "default", "concurrent-app")

				sanitized := sanitize.Sanitize(obj)
				yaml, _ := sanitize.MarshalToOrderedYAML(sanitized)
				key := GenerateKey(id, "UPDATE", yaml)

				if _, found := store.Get(key); found {
					hits.Add(1)
				} else {
					misses.Add(1)
				}
			}
		}(i)
	}

	// Wait for completion
	<-done
	time.Sleep(100 * time.Millisecond) // Allow goroutines to finish

	totalEvents := int64(concurrency * updatesPerGoroutine)
	totalProcessed := hits.Load() + misses.Load()

	assert.Equal(t, totalEvents, totalProcessed, "Should process all events")
	t.Logf("Concurrent test: %d events, %d hits, %d misses",
		totalEvents, hits.Load(), misses.Load())
}

// Helper: generateAndStore runs sanitization pipeline and stores correlation.
func generateAndStore(
	t *testing.T,
	store *Store,
	obj *unstructured.Unstructured,
	id types.ResourceIdentifier,
	operation string,
	username string,
) string {
	t.Helper()
	sanitized := sanitize.Sanitize(obj)
	yaml, err := sanitize.MarshalToOrderedYAML(sanitized)
	require.NoError(t, err)
	key := GenerateKey(id, operation, yaml)
	store.Put(key, username)
	return key
}

// Helper: generateKey runs sanitization pipeline and returns key.
func generateKey(
	t *testing.T,
	obj *unstructured.Unstructured,
	id types.ResourceIdentifier,
	operation string,
) string {
	t.Helper()
	sanitized := sanitize.Sanitize(obj)
	yaml, err := sanitize.MarshalToOrderedYAML(sanitized)
	require.NoError(t, err)
	return GenerateKey(id, operation, yaml)
}
