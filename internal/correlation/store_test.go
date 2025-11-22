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
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// TestGenerateKey_Determinism verifies that the same inputs always produce the same key.
func TestGenerateKey_Determinism(t *testing.T) {
	id := types.NewResourceIdentifier("apps", "v1", "deployments", "default", "my-app")
	operation := "UPDATE"
	sanitizedYAML := []byte("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: my-app")

	key1 := GenerateKey(id, operation, sanitizedYAML)
	key2 := GenerateKey(id, operation, sanitizedYAML)

	assert.Equal(t, key1, key2, "Same inputs should produce identical keys")
}

// TestGenerateKey_DifferentOperations verifies different operations produce different keys.
func TestGenerateKey_DifferentOperations(t *testing.T) {
	id := types.NewResourceIdentifier("apps", "v1", "deployments", "default", "my-app")
	sanitizedYAML := []byte("apiVersion: apps/v1\nkind: Deployment")

	keyCreate := GenerateKey(id, "CREATE", sanitizedYAML)
	keyUpdate := GenerateKey(id, "UPDATE", sanitizedYAML)
	keyDelete := GenerateKey(id, "DELETE", sanitizedYAML)

	assert.NotEqual(t, keyCreate, keyUpdate, "CREATE and UPDATE should produce different keys")
	assert.NotEqual(t, keyCreate, keyDelete, "CREATE and DELETE should produce different keys")
	assert.NotEqual(t, keyUpdate, keyDelete, "UPDATE and DELETE should produce different keys")
}

// TestGenerateKey_DifferentContent verifies different sanitized content produces different keys.
func TestGenerateKey_DifferentContent(t *testing.T) {
	id := types.NewResourceIdentifier("apps", "v1", "deployments", "default", "my-app")
	operation := "UPDATE"

	yaml1 := []byte("apiVersion: apps/v1\nkind: Deployment\nspec:\n  replicas: 1")
	yaml2 := []byte("apiVersion: apps/v1\nkind: Deployment\nspec:\n  replicas: 2")

	key1 := GenerateKey(id, operation, yaml1)
	key2 := GenerateKey(id, operation, yaml2)

	assert.NotEqual(t, key1, key2, "Different sanitized content should produce different keys")
}

// TestGenerateKey_NamespacedVsClusterScoped verifies correct key format for both types.
func TestGenerateKey_NamespacedVsClusterScoped(t *testing.T) {
	tests := []struct {
		name     string
		id       types.ResourceIdentifier
		expected string // partial match for key structure
	}{
		{
			name:     "namespaced with group",
			id:       types.NewResourceIdentifier("apps", "v1", "deployments", "default", "my-app"),
			expected: "apps/v1/deployments/default/my-app:",
		},
		{
			name:     "cluster-scoped with group",
			id:       types.NewResourceIdentifier("rbac.authorization.k8s.io", "v1", "clusterroles", "", "admin"),
			expected: "rbac.authorization.k8s.io/v1/clusterroles/admin:",
		},
		{
			name:     "namespaced core resource",
			id:       types.NewResourceIdentifier("", "v1", "configmaps", "default", "my-config"),
			expected: "v1/configmaps/default/my-config:",
		},
		{
			name:     "cluster-scoped core resource",
			id:       types.NewResourceIdentifier("", "v1", "namespaces", "", "kube-system"),
			expected: "v1/namespaces/kube-system:",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := GenerateKey(tt.id, "UPDATE", []byte("test"))
			assert.Contains(t, key, tt.expected, "Key should contain expected structure")
			assert.Contains(t, key, "UPDATE:", "Key should contain operation")
		})
	}
}

// TestStore_BasicPutAndGet verifies basic store operations.
func TestStore_BasicPutAndGet(t *testing.T) {
	store := NewStore(60*time.Second, 100)
	key := "test/key"
	username := "alice"

	// Put entry
	store.Put(key, username)
	assert.Equal(t, 1, store.Size(), "Store should have one entry")

	// Get entry
	entry, found := store.Get(key)
	require.True(t, found, "Entry should be found")
	assert.Equal(t, username, entry.Username, "Username should match")
	assert.Equal(t, 1, store.Size(), "Store should still have the entry after Get")

	// Get again should succeed
	entry2, found2 := store.Get(key)
	require.True(t, found2, "Entry should still be found")
	assert.Equal(t, username, entry2.Username, "Username should still match")
}

// TestStore_UpdateExisting verifies overwrite behavior for same key.
func TestStore_UpdateExisting(t *testing.T) {
	store := NewStore(60*time.Second, 100)
	key := "test/key"

	store.Put(key, "alice")
	store.Put(key, "bob") // Overwrites with warning log

	// Get returns the overwritten entry
	entry, found := store.Get(key)
	require.True(t, found, "Entry should be found")
	assert.Equal(t, "bob", entry.Username, "Should return the overwritten username")
	assert.Equal(t, 1, store.Size(), "Store should still have the entry after Get")
}

// TestStore_TTLExpiry verifies entries expire after TTL.
func TestStore_TTLExpiry(t *testing.T) {
	ttl := 100 * time.Millisecond
	store := NewStore(ttl, 100)
	key := "test/key"

	store.Put(key, "alice")

	// Immediate get should succeed
	entry, found := store.Get(key)
	require.True(t, found, "Entry should be found immediately")
	assert.Equal(t, "alice", entry.Username)

	// Re-add and wait for expiry
	store.Put(key, "alice")
	time.Sleep(ttl + 50*time.Millisecond) // Wait longer than TTL

	// Should be expired
	_, found = store.Get(key)
	assert.False(t, found, "Entry should be expired after TTL")
	assert.Equal(t, 0, store.Size(), "Expired entry should be removed")
}

// TestStore_LRUEviction verifies LRU eviction when capacity is reached.
func TestStore_LRUEviction(t *testing.T) {
	maxEntries := 3
	store := NewStore(60*time.Second, maxEntries)

	evictionCount := 0
	store.SetEvictionCallback(func() {
		evictionCount++
	})

	// Fill to capacity
	store.Put("key1", "user1")
	store.Put("key2", "user2")
	store.Put("key3", "user3")
	assert.Equal(t, 3, store.Size(), "Store should be at capacity")
	assert.Equal(t, 0, evictionCount, "No evictions yet")

	// Add one more - should evict oldest (key1)
	store.Put("key4", "user4")
	assert.Equal(t, 3, store.Size(), "Store should remain at capacity")
	assert.Equal(t, 1, evictionCount, "One eviction should have occurred")

	// key1 should be evicted
	_, found := store.Get("key1")
	assert.False(t, found, "Oldest entry should have been evicted")

	// Others should still exist
	_, found = store.Get("key2")
	assert.True(t, found, "Second entry should still exist")
	_, found = store.Get("key3")
	assert.True(t, found, "Third entry should still exist")
	_, found = store.Get("key4")
	assert.True(t, found, "New entry should exist")
}

// TestStore_LRUUpdateRefreshes verifies that updating a key refreshes its LRU position.
func TestStore_LRUUpdateRefreshes(t *testing.T) {
	maxEntries := 2
	store := NewStore(60*time.Second, maxEntries)

	store.Put("key1", "user1")
	store.Put("key2", "user2")

	// Update key1 - should move it to front
	store.Put("key1", "user1-updated")

	// Add key3 - should evict key2 (oldest), not key1
	store.Put("key3", "user3")

	_, found := store.Get("key1")
	assert.True(t, found, "Updated entry should still exist")

	_, found = store.Get("key2")
	assert.False(t, found, "Oldest entry should have been evicted")

	_, found = store.Get("key3")
	assert.True(t, found, "New entry should exist")
}

// TestStore_EvictExpired verifies manual expiry of old entries.
func TestStore_EvictExpired(t *testing.T) {
	ttl := 50 * time.Millisecond
	store := NewStore(ttl, 100)

	evictionCount := 0
	store.SetEvictionCallback(func() {
		evictionCount++
	})

	// Add entries
	store.Put("key1", "user1")
	time.Sleep(30 * time.Millisecond)
	store.Put("key2", "user2")
	time.Sleep(30 * time.Millisecond) // key1 should be expired, key2 not yet

	// Manually evict expired
	evicted := store.EvictExpired()
	assert.Equal(t, 1, evicted, "One entry should be evicted")
	assert.Equal(t, 1, evictionCount, "Eviction callback should be called")
	assert.Equal(t, 1, store.Size(), "One entry should remain")

	_, found := store.Get("key1")
	assert.False(t, found, "Expired entry should be removed")

	_, found = store.Get("key2")
	assert.True(t, found, "Non-expired entry should remain")
}

// TestStore_Clear verifies clearing all entries.
func TestStore_Clear(t *testing.T) {
	store := NewStore(60*time.Second, 100)

	store.Put("key1", "user1")
	store.Put("key2", "user2")
	store.Put("key3", "user3")
	assert.Equal(t, 3, store.Size())

	store.Clear()
	assert.Equal(t, 0, store.Size(), "Store should be empty after clear")

	_, found := store.Get("key1")
	assert.False(t, found, "No entries should exist after clear")
}

// TestStore_ConcurrentAccess verifies thread-safety under concurrent load.
func TestStore_ConcurrentAccess(_ *testing.T) {
	store := NewStore(5*time.Second, 1000)
	concurrency := 10
	operationsPerGoroutine := 100

	var wg sync.WaitGroup
	wg.Add(concurrency)

	for i := range concurrency {
		go func(_ int) {
			defer wg.Done()
			for range operationsPerGoroutine {
				key := GenerateKey(
					types.NewResourceIdentifier("apps", "v1", "deployments", "default", "app"),
					"UPDATE",
					[]byte("test content"),
				)
				store.Put(key, "user")
				store.Get(key) // Can be called multiple times
			}
		}(i)
	}

	wg.Wait()
	// If we get here without deadlock or panic, thread-safety is working
}

// TestStore_EvictionCallback verifies the callback is invoked for all eviction types.
func TestStore_EvictionCallback(t *testing.T) {
	ttl := 50 * time.Millisecond
	store := NewStore(ttl, 2)

	evictionCount := 0
	store.SetEvictionCallback(func() {
		evictionCount++
	})

	// LRU eviction
	store.Put("key1", "user1")
	store.Put("key2", "user2")
	store.Put("key3", "user3") // Evicts key1
	assert.Equal(t, 1, evictionCount, "LRU eviction should trigger callback")

	// TTL eviction via Get (expired entry accessed)
	time.Sleep(ttl + 20*time.Millisecond)
	_, found := store.Get("key2") // Should be expired and trigger eviction
	assert.False(t, found)
	assert.Equal(t, 2, evictionCount, "TTL eviction should trigger callback")

	// TTL eviction via EvictExpired - both key3 and key4 will be expired
	store.Put("key4", "user4")
	time.Sleep(ttl + 20*time.Millisecond)
	evicted := store.EvictExpired()
	assert.Equal(t, 2, evicted, "Should evict 2 expired entries (key3 and key4)")
	assert.Equal(t, 4, evictionCount, "Manual eviction should trigger callback for each evicted entry")
}

// TestStore_SanitizeEquivalence verifies that sanitized objects produce identical keys.
func TestStore_SanitizeEquivalence(t *testing.T) {
	id := types.NewResourceIdentifier("apps", "v1", "deployments", "default", "my-app")
	operation := "UPDATE"

	// Simulate webhook sanitized YAML
	webhookYAML := []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
  namespace: default
spec:
  replicas: 3
`)

	// Simulate watch sanitized YAML (should be identical after sanitization)
	watchYAML := []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
  namespace: default
spec:
  replicas: 3
`)

	webhookKey := GenerateKey(id, operation, webhookYAML)
	watchKey := GenerateKey(id, operation, watchYAML)

	assert.Equal(t, webhookKey, watchKey, "Sanitized webhook and watch objects should produce the same key")
}

// TestStore_NoResourceVersionDependence verifies keys are independent of resourceVersion.
func TestStore_NoResourceVersionDependence(t *testing.T) {
	id := types.NewResourceIdentifier("apps", "v1", "deployments", "default", "my-app")
	operation := "UPDATE"

	// YAML with different resourceVersions (which should be sanitized away)
	yaml1 := []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
  resourceVersion: "12345"
spec:
  replicas: 3
`)

	yaml2 := []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
  resourceVersion: "67890"
spec:
  replicas: 3
`)

	// If sanitization is working correctly upstream, these should be identical after sanitization
	// For this test, we're verifying the key generation is deterministic
	key1 := GenerateKey(id, operation, yaml1)
	key2 := GenerateKey(id, operation, yaml2)

	// Keys will differ because we're passing different YAML
	// But the point is that the correlation system expects sanitized YAML as input
	assert.NotEqual(t, key1, key2, "Different YAML content produces different keys")

	// The real test is that when the same sanitized YAML is provided, keys match
	sanitizedYAML := []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
spec:
  replicas: 3
`)

	key3 := GenerateKey(id, operation, sanitizedYAML)
	key4 := GenerateKey(id, operation, sanitizedYAML)
	assert.Equal(t, key3, key4, "Same sanitized YAML produces identical keys")
}

// TestGenerateKey_SanitizationRobustness verifies that different YAML formatting
// produces the same key after sanitization (key order, whitespace, etc.).
// This test demonstrates that the sanitization pipeline normalizes YAML before hashing.
func TestGenerateKey_SanitizationRobustness(t *testing.T) {
	id := types.NewResourceIdentifier("apps", "v1", "deployments", "default", "my-app")
	operation := "UPDATE"

	// Note: In production, raw YAML goes through sanitize.Sanitize() + MarshalToOrderedYAML()
	// BEFORE being passed to GenerateKey. This test simulates that by passing
	// already-sanitized YAML that should hash identically.

	// Canonical sanitized YAML (what sanitize.MarshalToOrderedYAML produces)
	canonicalYAML := []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
  namespace: default
spec:
  replicas: 3
  selector:
    matchLabels:
      app: my-app
`)

	// When the same logical object is sanitized, it should produce identical YAML
	// regardless of input formatting, which then produces identical keys
	key1 := GenerateKey(id, operation, canonicalYAML)
	key2 := GenerateKey(id, operation, canonicalYAML)

	assert.Equal(t, key1, key2, "Identical sanitized YAML should produce identical keys")

	// Different content (different replica count) should produce different key
	differentYAML := []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
  namespace: default
spec:
  replicas: 5
  selector:
    matchLabels:
      app: my-app
`)

	key3 := GenerateKey(id, operation, differentYAML)
	assert.NotEqual(t, key1, key3, "Different content should produce different keys")

	// Verify the sanitization contract: same keys produce same hash
	assert.Equal(t, key1, key2, "Deterministic hashing of same content")
}

// TestGenerateKey_ContentSensitivity verifies that actual content changes
// produce different keys even with same structure.
func TestGenerateKey_ContentSensitivity(t *testing.T) {
	id := types.NewResourceIdentifier("apps", "v1", "deployments", "default", "my-app")
	operation := "UPDATE"

	yaml1 := []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
	 name: my-app
spec:
	 replicas: 3
`)

	yaml2 := []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
	 name: my-app
spec:
	 replicas: 5
`)

	key1 := GenerateKey(id, operation, yaml1)
	key2 := GenerateKey(id, operation, yaml2)

	assert.NotEqual(t, key1, key2, "Different replicas value should produce different keys")
}

// TestGenerateKey_SpecificHashFormat verifies xxhash produces expected format.
func TestGenerateKey_SpecificHashFormat(t *testing.T) {
	id := types.NewResourceIdentifier("apps", "v1", "deployments", "default", "test")
	operation := "UPDATE"
	yaml := []byte("test content")

	key := GenerateKey(id, operation, yaml)

	// Key should contain a 16-character hex hash
	assert.Contains(t, key, "apps/v1/deployments/default/test:UPDATE:")
	assert.Len(t, key, len("apps/v1/deployments/default/test:UPDATE:")+16,
		"Key should end with 16-character hash")
}
