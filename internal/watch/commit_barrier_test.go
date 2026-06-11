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

package watch

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
)

// barrierManager builds a Manager wired with the given reader. It returns the manager and
// the secrets GVR that the fixture's WatchRule targets — the type all barrier tests use.
func barrierManager(t *testing.T, reader AuditTailReader) (*Manager, schema.GroupVersionResource) {
	t.Helper()
	store := rulestore.NewStore()
	addSecretsWatchRule(store)
	m := streamingManager(t, gitTargetFixture(), store)
	m.AuditTailReader = reader
	secrets := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}
	return m, secrets
}

// TestDrainTailsToSnapshot covers the §6 barrier condition (canonical-stream-retirement.md),
// now using a per-type snapshot (map[GVR]string) instead of a single cross-type rv_C. Each
// type's watermark is its own stream top taken at CommitRequest creation time, so no
// cross-type RV ordering is assumed (correct for aggregated-API types with their own counters).
func TestDrainTailsToSnapshot(t *testing.T) {
	ctx := context.Background()

	t.Run("cursor at or past the per-type watermark passes immediately", func(t *testing.T) {
		m, secrets := barrierManager(t, &blockingTailReader{})
		m.setAuditTailCursor(secrets, "500-0")
		// snapshot watermark = 500 → cursor rv 500 >= 500 ✓
		assert.True(t, m.DrainTailsToSnapshot(ctx, snap(secrets, "500"), time.Second))
		// snapshot watermark = 499 → cursor rv 500 >= 499 ✓
		assert.True(t, m.DrainTailsToSnapshot(ctx, snap(secrets, "499"), time.Second))
	})

	t.Run("a lagging cursor blocks the barrier until it advances", func(t *testing.T) {
		m, secrets := barrierManager(t, &blockingTailReader{})
		m.setAuditTailCursor(secrets, "100-0")

		go func() {
			time.Sleep(350 * time.Millisecond)
			m.setAuditTailCursor(secrets, "505-0") // tail applies past the watermark
		}()
		start := time.Now()
		assert.True(t, m.DrainTailsToSnapshot(ctx, snap(secrets, "500"), 5*time.Second),
			"the barrier must pass once the tail's applied cursor reaches the watermark")
		assert.GreaterOrEqual(t, time.Since(start), 300*time.Millisecond,
			"the barrier must actually have waited for the cursor")
	})

	t.Run("a type whose stream top was its watermark passes once the tail drains to it", func(t *testing.T) {
		// snapshot[secrets]="100": the stream top at CR creation time was 100.
		// No cross-type rv_C comparison needed; the tail just needs to reach 100.
		m, secrets := barrierManager(t, &blockingTailReader{})
		m.setAuditTailCursor(secrets, "100-3")
		assert.True(t, m.DrainTailsToSnapshot(ctx, snap(secrets, "100"), time.Second))
	})

	t.Run("a stuck tail degrades to false at the deadline, never hangs", func(t *testing.T) {
		m, secrets := barrierManager(t, &blockingTailReader{})
		m.setAuditTailCursor(secrets, "100-0")
		start := time.Now()
		assert.False(t, m.DrainTailsToSnapshot(ctx, snap(secrets, "500"), 600*time.Millisecond))
		assert.Less(t, time.Since(start), 3*time.Second, "the degrade is bounded")
	})

	t.Run("a type with no running tail is skipped", func(t *testing.T) {
		m, secrets := barrierManager(t, &blockingTailReader{})
		// No cursor seeded: no tail runs for secrets. There is no freshness path to wait on;
		// correctness rides the checkpoint. The barrier passes immediately.
		assert.True(t, m.DrainTailsToSnapshot(ctx, snap(secrets, "500"), time.Second))
	})

	t.Run("a type absent from the snapshot is not waited on", func(t *testing.T) {
		m, secrets := barrierManager(t, &blockingTailReader{})
		m.setAuditTailCursor(secrets, "100-0") // low cursor, but secrets is not in the snapshot
		other := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
		// snapshot only covers deployments (no tail, skipped) → passes immediately
		assert.True(t, m.DrainTailsToSnapshot(ctx, snap(other, "500"), time.Second))
	})

	t.Run("an empty snapshot has nothing to wait for", func(t *testing.T) {
		m, _ := barrierManager(t, &blockingTailReader{})
		assert.True(t, m.DrainTailsToSnapshot(ctx, map[schema.GroupVersionResource]string{}, time.Second))
	})

	t.Run("a blank per-type watermark means the stream was empty at snapshot time", func(t *testing.T) {
		m, secrets := barrierManager(t, &blockingTailReader{})
		m.setAuditTailCursor(secrets, "100-0")
		// snapshot[secrets]="" → stream had no entries when the CR was created → nothing to wait for
		assert.True(t, m.DrainTailsToSnapshot(ctx, snap(secrets, ""), time.Second))
	})

	t.Run("an opaque (non-numeric) per-type watermark is skipped", func(t *testing.T) {
		// Aggregated-API types may have opaque RVs that cannot be compared numerically.
		// The barrier skips them rather than blocking forever.
		m, secrets := barrierManager(t, &blockingTailReader{})
		m.setAuditTailCursor(secrets, "100-0")
		assert.True(t, m.DrainTailsToSnapshot(ctx, snap(secrets, "not-an-rv"), time.Second))
	})
}

// snap builds a single-type snapshot for the test helpers above.
func snap(gvr schema.GroupVersionResource, hw string) map[schema.GroupVersionResource]string {
	return map[schema.GroupVersionResource]string{gvr: hw}
}
