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

	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
)

// channelTailReader is a fake AuditTailReader whose reads return batches of per-event changes pushed
// onto a channel, or a timeout (no changes) once the channel is quiet for the block window.
type channelTailReader struct {
	batches chan []git.Event
	calls   atomic.Int32
}

func (r *channelTailReader) ReadTypeAuditChanges(
	ctx context.Context, _, _, lastID string, block time.Duration,
) ([]git.Event, string, error) {
	r.calls.Add(1)
	select {
	case <-ctx.Done():
		return nil, lastID, ctx.Err()
	case b := <-r.batches:
		return b, "1-0", nil
	case <-time.After(block):
		return nil, lastID, nil
	}
}

// blockingTailReader just parks until the context cancels — for lifecycle tests that only care
// about start/stop bookkeeping, not arrivals.
type blockingTailReader struct{}

func (blockingTailReader) ReadTypeAuditChanges(
	ctx context.Context, _, _, lastID string, _ time.Duration,
) ([]git.Event, string, error) {
	<-ctx.Done()
	return nil, lastID, ctx.Err()
}

func (m *Manager) auditTailCount() int {
	m.auditTailsMu.Lock()
	defer m.auditTailsMu.Unlock()
	return len(m.auditTails)
}

// tailTestManager builds a Manager that resolves a secrets WatchRule but tails an UNWATCHED type, so
// the real apply (when not overridden) is a safe no-op. The block window is shrunk so the loop runs
// fast.
func tailTestManager(t *testing.T, reader AuditTailReader) *Manager {
	t.Helper()
	store := rulestore.NewStore()
	addSecretsWatchRule(store)
	m := streamingManager(t, gitTargetFixture(), store)
	m.AuditTailReader = reader
	m.auditTailBlockOverride = 40 * time.Millisecond
	return m
}

// TestAuditTail_AppliesChangesPerEvent proves the tail applies each batch of changes it reads (the
// sweep-free freshness apply), and keeps applying subsequent batches.
func TestAuditTail_AppliesChangesPerEvent(t *testing.T) {
	reader := &channelTailReader{batches: make(chan []git.Event, 16)}
	var applied atomic.Int32
	m := tailTestManager(t, reader)
	m.auditTailApplyOverride = func(_ context.Context, _ logr.Logger, _ schema.GroupVersionResource, changes []git.Event) {
		for range changes {
			applied.Add(1)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m.startTypeAuditTail(ctx, logr.Discard(), configmapsGVR, "100")

	// A two-event batch is applied as two per-event changes.
	reader.batches <- []git.Event{{Operation: "CREATE"}, {Operation: "CREATE"}}
	require.Eventually(t, func() bool { return applied.Load() == 2 }, time.Second, 5*time.Millisecond,
		"both changes in the batch are applied")

	// A later batch keeps being applied (the tail loops).
	reader.batches <- []git.Event{{Operation: "DELETE"}}
	require.Eventually(t, func() bool { return applied.Load() == 3 }, time.Second, 5*time.Millisecond,
		"a later batch is applied too")

	m.stopTypeAuditTail(configmapsGVR)
}

// TestStartTypeAuditTail_StopsCleanly proves a running tail is forgotten and its goroutine exits on
// stop.
func TestStartTypeAuditTail_StopsCleanly(t *testing.T) {
	m := tailTestManager(t, blockingTailReader{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m.startTypeAuditTail(ctx, logr.Discard(), configmapsGVR, "100")
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

	m.startTypeAuditTail(ctx, logr.Discard(), configmapsGVR, "100")
	m.startTypeAuditTail(ctx, logr.Discard(), configmapsGVR, "100")
	m.startTypeAuditTail(ctx, logr.Discard(), configmapsGVR, "100")

	assert.Equal(t, 1, m.auditTailCount(), "repeat starts are no-ops")
	m.stopTypeAuditTail(configmapsGVR)
}

// TestStartTypeAuditTail_NilReaderIsNoop proves the wake is cleanly disabled when no reader is wired.
func TestStartTypeAuditTail_NilReaderIsNoop(t *testing.T) {
	m := &Manager{Log: logr.Discard()}
	m.startTypeAuditTail(context.Background(), logr.Discard(), configmapsGVR, "100")
	assert.Equal(t, 0, m.auditTailCount(), "no tail without a reader")
}

// TestStopTypeAuditTail_UnknownIsNoop proves stopping a type that never tailed is harmless.
func TestStopTypeAuditTail_UnknownIsNoop(t *testing.T) {
	m := &Manager{Log: logr.Discard()}
	m.stopTypeAuditTail(configmapsGVR) // must not panic
	assert.Equal(t, 0, m.auditTailCount())
}

// TestAuditTailAnchor proves the tail resumes strictly after the checkpoint revision (so an event
// between the checkpoint LIST and the tail starting is replayed), and falls back to present-only
// when there is no checkpoint rv.
func TestAuditTailAnchor(t *testing.T) {
	assert.Equal(t, "$", auditTailAnchor(""))
	assert.Equal(t, "$", auditTailAnchor("   "))
	assert.Equal(t, "4928-18446744073709551615", auditTailAnchor("4928"),
		"anchor is strictly after every entry at rv 4928")
}
