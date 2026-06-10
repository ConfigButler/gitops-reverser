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

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
)

// channelTailReader is a fake AuditTailReader whose "arrivals" are values pushed onto a channel.
// It returns the next buffered arrival immediately (hasNew), or a timeout (no new) once the channel
// is quiet for the block window — exactly the shape the settle/coalesce loop reads.
type channelTailReader struct {
	events chan string
	calls  atomic.Int32
}

func (r *channelTailReader) AwaitTypeAuditEntry(
	ctx context.Context, _, _, lastID string, block time.Duration,
) (string, bool, error) {
	r.calls.Add(1)
	select {
	case <-ctx.Done():
		return lastID, false, ctx.Err()
	case id := <-r.events:
		return id, true, nil
	case <-time.After(block):
		return lastID, false, nil
	}
}

// blockingTailReader just parks until the context cancels — for lifecycle tests that only care
// about start/stop bookkeeping, not arrivals.
type blockingTailReader struct{}

func (blockingTailReader) AwaitTypeAuditEntry(
	ctx context.Context, _, _, lastID string, _ time.Duration,
) (string, bool, error) {
	<-ctx.Done()
	return lastID, false, ctx.Err()
}

func (m *Manager) auditTailCount() int {
	m.auditTailsMu.Lock()
	defer m.auditTailsMu.Unlock()
	return len(m.auditTails)
}

// tailTestManager builds a Manager that resolves a secrets WatchRule but tails an UNWATCHED type, so
// the real reconcile (when not overridden) is a safe no-op. Block/settle windows are shrunk so the
// loop runs fast.
func tailTestManager(t *testing.T, reader AuditTailReader) *Manager {
	t.Helper()
	store := rulestore.NewStore()
	addWatchRule(store, "wr-secrets", "ns-a", "secrets")
	m := streamingManager(t, gitTargetFixture(), store, nil)
	m.AuditTailReader = reader
	m.auditTailBlockOverride = 40 * time.Millisecond
	m.auditTailSettleOverride = 25 * time.Millisecond
	return m
}

// TestAuditTail_CoalescesBurstIntoOneReconcile is the regression guard for the bi-directional race:
// two events that arrive within the settle window must drive ONE reconcile, not one each — so a
// mark-and-sweep never runs on a half-seen burst (which would sweep the not-yet-seen object).
func TestAuditTail_CoalescesBurstIntoOneReconcile(t *testing.T) {
	reader := &channelTailReader{events: make(chan string, 16)}
	var reconciles atomic.Int32
	m := tailTestManager(t, reader)
	m.auditTailReconcileOverride = func(context.Context, logr.Logger, schema.GroupVersionResource) {
		reconciles.Add(1)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m.startTypeAuditTail(ctx, logr.Discard(), configmapsGVR)

	// A two-event burst arriving together coalesces into one reconcile.
	reader.events <- "1-0"
	reader.events <- "2-0"
	require.Eventually(t, func() bool { return reconciles.Load() == 1 }, time.Second, 5*time.Millisecond,
		"the co-arriving burst drives exactly one reconcile")
	// It stays one — the settled burst is not re-reconciled.
	time.Sleep(60 * time.Millisecond)
	assert.Equal(t, int32(1), reconciles.Load(), "no second reconcile for the same burst")

	// A later, separate event drives a second reconcile.
	reader.events <- "3-0"
	require.Eventually(t, func() bool { return reconciles.Load() == 2 }, time.Second, 5*time.Millisecond,
		"a later event reconciles again")

	m.stopTypeAuditTail(configmapsGVR)
}

// TestStartTypeAuditTail_StopsCleanly proves a running tail is forgotten and its goroutine exits on
// stop.
func TestStartTypeAuditTail_StopsCleanly(t *testing.T) {
	m := tailTestManager(t, blockingTailReader{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m.startTypeAuditTail(ctx, logr.Discard(), configmapsGVR)
	assert.Equal(t, 1, m.auditTailCount(), "one tail is running")

	m.stopTypeAuditTail(configmapsGVR)
	assert.Equal(t, 0, m.auditTailCount(), "stop forgets the tail")
}

// TestStartTypeAuditTail_Idempotent proves a repeated start (a periodic re-anchor's TypeSynced)
// never spawns a second tail for the same type.
func TestStartTypeAuditTail_Idempotent(t *testing.T) {
	m := tailTestManager(t, blockingTailReader{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m.startTypeAuditTail(ctx, logr.Discard(), configmapsGVR)
	m.startTypeAuditTail(ctx, logr.Discard(), configmapsGVR)
	m.startTypeAuditTail(ctx, logr.Discard(), configmapsGVR)

	assert.Equal(t, 1, m.auditTailCount(), "repeat starts are no-ops")
	m.stopTypeAuditTail(configmapsGVR)
}

// TestStartTypeAuditTail_NilReaderIsNoop proves the wake is cleanly disabled when no reader is wired.
func TestStartTypeAuditTail_NilReaderIsNoop(t *testing.T) {
	m := &Manager{Log: logr.Discard()}
	m.startTypeAuditTail(context.Background(), logr.Discard(), configmapsGVR)
	assert.Equal(t, 0, m.auditTailCount(), "no tail without a reader")
}

// TestStopTypeAuditTail_UnknownIsNoop proves stopping a type that never tailed is harmless.
func TestStopTypeAuditTail_UnknownIsNoop(t *testing.T) {
	m := &Manager{Log: logr.Discard()}
	m.stopTypeAuditTail(configmapsGVR) // must not panic
	assert.Equal(t, 0, m.auditTailCount())
}
