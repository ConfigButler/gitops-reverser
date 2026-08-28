// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cespare/xxhash/v2"
	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// A queue that grows silently is what made the resync storm hard to see, and the owner loop is a
// queue. These signals are part of the loop rather than a follow-up, and the one that matters
// most is the oldest dirty-target age: it goes up and stays up exactly when something is stuck.

// Pass outcome label values.
const (
	passOutcomeCompleted = "completed"
	passOutcomeFailed    = "failed"
	passOutcomeTimedOut  = "timed_out"
)

// recordPassOutcome publishes how one target's pass ended, both as the per-target state an
// operator can read off the GitTarget and as the counters and histogram an alert can watch.
//
// A failure is recorded where someone looks, not only in a log: a permanently unreachable source
// cluster looks exactly like a healthy quiet one unless the retry state is published.
func (m *Manager) recordPassOutcome(ref types.ResourceReference, started time.Time, passErr error) {
	elapsed := time.Since(started)

	// Republish only when something an outside reader would notice moved. The periodic sweep runs a
	// pass per target every 30s, and a steady-state pass changes nothing about this status, so an
	// unconditional republish cloned the whole snapshot on every one of them forever.
	m.mutateWatchPlane(func(s *watchPlaneState) bool {
		status := s.passes[ref.Key()]
		before := status
		if passErr == nil {
			status.Failures = 0
			status.LastError = ""
			status.Landed = true
		} else {
			status.Failures++
			status.LastError = passErr.Error()
		}
		s.passes[ref.Key()] = status
		return status != before
	})

	if telemetry.WatchPlanPassDurationSeconds != nil {
		telemetry.WatchPlanPassDurationSeconds.Record(context.Background(), elapsed.Seconds())
	}
	if telemetry.WatchPlanPassesTotal == nil {
		return
	}
	outcome := passOutcomeCompleted
	switch {
	case passErr == nil:
	case isDeadlineExceeded(passErr):
		outcome = passOutcomeTimedOut
	default:
		outcome = passOutcomeFailed
	}
	telemetry.WatchPlanPassesTotal.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("outcome", outcome),
		attribute.String("gittarget_namespace", ref.Namespace),
		attribute.String("gittarget_name", ref.Name),
	))
}

// isDeadlineExceeded reports whether a pass ended on its own deadline rather than on a real
// failure. The two want separate alerts: "running and failing" and "not finishing" have
// different causes.
//
// It matches on the wrapped sentinel rather than on the rendered message. passDeadline wraps
// ctx.Err() with %w for exactly this, and a string match would have gone on quietly agreeing with
// any error that happened to contain the words.
func isDeadlineExceeded(err error) bool {
	return errors.Is(err, context.DeadlineExceeded)
}

// recordTriggerLocked counts one trigger by the reason that produced it, and counts the ones the
// silence window absorbed. Together they prove the debounce is doing what it claims and show
// which source is noisy. The triggers mutex is held; both instruments are synchronous counters
// with no callback, so neither can re-enter it.
func recordTriggerLocked(reason string, coalesced bool) {
	if telemetry.WatchPlanTriggersTotal != nil {
		telemetry.WatchPlanTriggersTotal.Add(context.Background(), 1,
			metric.WithAttributes(attribute.String("reason", reason)))
	}
	if coalesced && telemetry.WatchPlanTriggersCoalescedTotal != nil {
		telemetry.WatchPlanTriggersCoalescedTotal.Add(context.Background(), 1)
	}
}

// publishDirtySetDepth records how deep the dirty set is and how long its oldest entry has been
// waiting, once per loop turn.
func (m *Manager) publishDirtySetDepth() {
	if telemetry.WatchPlanDirtyTargets == nil && telemetry.WatchPlanOldestDirtyAgeSeconds == nil {
		return
	}
	count, oldest := m.dirtySetDepth()
	ctx := context.Background()
	if telemetry.WatchPlanDirtyTargets != nil {
		telemetry.WatchPlanDirtyTargets.Record(ctx, int64(count))
	}
	if telemetry.WatchPlanOldestDirtyAgeSeconds != nil {
		telemetry.WatchPlanOldestDirtyAgeSeconds.Record(ctx, int64(oldest.Seconds()))
	}
}

// dirtySetDepth returns how many targets are dirty and how long the oldest has been so.
func (m *Manager) dirtySetDepth() (int, time.Duration) {
	t := m.triggers()
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	oldest := time.Duration(0)
	for _, entry := range t.dirty {
		if age := now.Sub(entry.firstDirty); age > oldest {
			oldest = age
		}
	}
	return len(t.dirty), oldest
}

// logOwnerHeartbeat makes the loop's liveness observable in logs and tests, and carries the same
// two numbers the metrics do so a log-only install is not blind to a stuck queue.
func (m *Manager) logOwnerHeartbeat(log logr.Logger) {
	count, oldest := m.dirtySetDepth()
	log.V(1).Info("watch plane owner heartbeat", "dirtyTargets", count, "oldestDirtyAge", oldest.String())
}

// clearDeclareForce consumes a satisfied force-recheck request. It runs only after a pass has
// actually put the watches in place, so a failed attempt leaves the request standing.
func (m *Manager) clearDeclareForce(ref types.ResourceReference) {
	t := m.triggers()
	t.mu.Lock()
	defer t.mu.Unlock()
	if intent := t.declares[ref.Key()]; intent != nil && matchesUID(intent.ref, ref) {
		intent.force = false
	}
}

// watchPlanFingerprint renders one GitTarget's declared stream set to a comparable value, so two
// projections of the tables can be diffed per target without keeping a type-to-target index.
//
// It hashes exactly what the plan is built from — the cell each stream covers, the version it
// opens at, and the operation filter it applies — so a catalog change that moves none of those
// for a target is, correctly, no reason to replan it.
func watchPlanFingerprint(table WatchedTypeTable) uint64 {
	specs := targetWatchSpecs(table)
	keys := sortedTargetWatchSpecKeys(specs)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s@%s=%s", key.GVR.String(), key.Namespace, specs[key]))
	}
	return xxhash.Sum64String(strings.Join(parts, "\x00"))
}
