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
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// barrierTailReader is a blocking tail reader with the optional high-water capability the
// barrier's quiet-type shortcut probes for.
type barrierTailReader struct {
	blockingTailReader

	highWater atomic.Value // string
}

func (r *barrierTailReader) TypeAuditHighWater(_ context.Context, _, _ string) string {
	if v, ok := r.highWater.Load().(string); ok {
		return v
	}
	return ""
}

// barrierManager builds a Manager whose watched table for my-target holds core/v1 secrets
// (the standard fixture), with the given reader wired as the tail reader.
func barrierManager(
	t *testing.T, reader AuditTailReader,
) (*Manager, types.ResourceReference, schema.GroupVersionResource) {
	t.Helper()
	store := rulestore.NewStore()
	addSecretsWatchRule(store)
	m := streamingManager(t, gitTargetFixture(), store)
	m.AuditTailReader = reader
	gitDest := types.NewResourceReference("my-target", "gitops-reverser")
	secrets := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}
	return m, gitDest, secrets
}

// TestDrainTailsToWatermark covers the §6 barrier condition (canonical-stream-retirement.md):
// it holds while a tail's applied cursor is below the watermark, passes the moment the cursor
// reaches it (the "finalize includes all pre-rv_C upserts" guarantee — the cursor only advances
// after a batch is applied onto the FIFO worker), passes immediately for a quiet type whose
// stream high-water is below the watermark, skips types with no running tail, and degrades to
// false — never hangs — when a tail cannot reach the watermark in time (Option A).
func TestDrainTailsToWatermark(t *testing.T) {
	ctx := context.Background()

	t.Run("cursor at or past the watermark passes immediately", func(t *testing.T) {
		reader := &barrierTailReader{}
		m, gitDest, secrets := barrierManager(t, reader)
		m.setAuditTailCursor(secrets, "500-0")
		assert.True(t, m.DrainTailsToWatermark(ctx, gitDest, "500", time.Second))
		assert.True(t, m.DrainTailsToWatermark(ctx, gitDest, "499", time.Second))
	})

	t.Run("a lagging cursor blocks the barrier until it advances", func(t *testing.T) {
		reader := &barrierTailReader{}
		reader.highWater.Store("600") // entries past the watermark exist; no quiet shortcut
		m, gitDest, secrets := barrierManager(t, reader)
		m.setAuditTailCursor(secrets, "100-0")

		go func() {
			time.Sleep(350 * time.Millisecond)
			m.setAuditTailCursor(secrets, "505-0") // the tail applies past rv_C
		}()
		start := time.Now()
		assert.True(t, m.DrainTailsToWatermark(ctx, gitDest, "500", 5*time.Second),
			"the barrier must pass once the tail's applied cursor reaches the watermark")
		assert.GreaterOrEqual(t, time.Since(start), 300*time.Millisecond,
			"the barrier must actually have waited for the cursor")
	})

	t.Run("a quiet type passes via the high-water shortcut", func(t *testing.T) {
		reader := &barrierTailReader{}
		reader.highWater.Store("100") // nothing as new as the watermark exists
		m, gitDest, secrets := barrierManager(t, reader)
		m.setAuditTailCursor(secrets, "100-3") // drained to the high-water
		assert.True(t, m.DrainTailsToWatermark(ctx, gitDest, "500", time.Second))
	})

	t.Run("a stuck tail degrades to false at the deadline, never hangs", func(t *testing.T) {
		reader := &barrierTailReader{}
		reader.highWater.Store("600")
		m, gitDest, secrets := barrierManager(t, reader)
		m.setAuditTailCursor(secrets, "100-0")
		start := time.Now()
		assert.False(t, m.DrainTailsToWatermark(ctx, gitDest, "500", 600*time.Millisecond))
		assert.Less(t, time.Since(start), 3*time.Second, "the degrade is bounded")
	})

	t.Run("a type with no running tail is skipped", func(t *testing.T) {
		reader := &barrierTailReader{}
		reader.highWater.Store("600")
		m, gitDest, _ := barrierManager(t, reader)
		// No cursor seeded: no tail runs for secrets (e.g. not yet Synced). There is no
		// freshness path to wait on; correctness rides the checkpoint.
		assert.True(t, m.DrainTailsToWatermark(ctx, gitDest, "500", time.Second))
	})

	t.Run("a non-numeric watermark has nothing orderable to wait for", func(t *testing.T) {
		reader := &barrierTailReader{}
		m, gitDest, secrets := barrierManager(t, reader)
		m.setAuditTailCursor(secrets, "100-0")
		assert.True(t, m.DrainTailsToWatermark(ctx, gitDest, "not-an-rv", time.Second))
	})
}
