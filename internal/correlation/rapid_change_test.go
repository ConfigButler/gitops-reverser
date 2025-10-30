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

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// TestStore_RapidChangesWithContentReuse validates the critical scenario where:
// - User B changes spec to {"cool": false}
// - User B changes spec to {"cool": true}
// - User A changes spec to {"cool": false} (same content as first change)
//
// When watch events are delayed and arrive after all webhooks, we need to ensure:
// - Watch event 1 (false) -> attributed to user B (FIFO: first in queue)
// - Watch event 2 (true) -> attributed to user B
// - Watch event 3 (false) -> attributed to user A (FIFO: second in queue)
//
// This tests the store's FIFO queue handling of multiple changes to the same content
// by different users in rapid succession.
func TestStore_RapidChangesWithContentReuse(t *testing.T) {
	store := NewStore(60*time.Second, 100)
	id := types.NewResourceIdentifier("apps", "v1", "deployments", "default", "my-app")

	// Simulate sanitized YAML for different spec values
	specFalse := []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
  namespace: default
spec:
  cool: false
`)

	specTrue := []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
  namespace: default
spec:
  cool: true
`)

	// Webhook events (rapid succession):
	// 1. User B changes to false
	key1 := GenerateKey(id, "UPDATE", specFalse)
	store.Put(key1, "userB")

	// 2. User B changes to true
	key2 := GenerateKey(id, "UPDATE", specTrue)
	store.Put(key2, "userB")

	// 3. User A changes to false (same content as step 1, same key!)
	key3 := GenerateKey(id, "UPDATE", specFalse)
	store.Put(key3, "userA") // With FIFO queue: appends to queue[key_false]

	// Verify keys
	assert.Equal(t, key1, key3, "Same content should produce same key")
	assert.NotEqual(t, key1, key2, "Different content should produce different keys")

	// Watch events arrive (delayed, in order):
	// Watch event 1: spec changed to false (first time)
	entry1, found1 := store.GetAndDelete(key1)
	require.True(t, found1, "First watch event should find correlation entry")
	assert.Equal(t, "userB", entry1.Username,
		"FIFO: First webhook to false should be attributed to userB")

	// Watch event 2: spec changed to true
	entry2, found2 := store.GetAndDelete(key2)
	require.True(t, found2, "Second watch event should find correlation entry")
	assert.Equal(t, "userB", entry2.Username, "userB should be attributed")

	// Watch event 3: spec changed to false (second time)
	entry3, found3 := store.GetAndDelete(key3)
	require.True(t, found3, "Third watch event should find correlation entry")
	assert.Equal(t, "userA", entry3.Username,
		"FIFO: Second webhook to false should be attributed to userA")

	// Verify queue is now empty
	_, found4 := store.GetAndDelete(key1)
	assert.False(t, found4, "All entries should be consumed")
}

// TestStore_QueueSolution demonstrates a potential solution using multi-value queues.
// This is a design exploration for handling the rapid-change scenario.
func TestStore_QueueSolution(t *testing.T) {
	t.Skip("Design exploration - not yet implemented")

	// Proposed solution: Store multiple entries per key in a FIFO queue
	// - Put appends to queue (with max queue depth per key)
	// - GetAndDelete removes from front (FIFO)
	//
	// This would preserve the order of changes to the same content:
	// - Webhook: false by userB -> queue[key_false] = [userB]
	// - Webhook: true by userB -> queue[key_true] = [userB]
	// - Webhook: false by userA -> queue[key_false] = [userB, userA]
	// - Watch: false -> dequeues userB (correct!)
	// - Watch: true -> dequeues userB (correct!)
	// - Watch: false -> dequeues userA (correct!)
	//
	// Trade-offs:
	// - More complex implementation
	// - Higher memory usage (multiple entries per key)
	// - Requires bounded queue depth per key
}
