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

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/typeset"
)

// lifecycleEventBuffer bounds the channel between the registry's updater (which produces
// lifecycle events synchronously under its single-updater discipline) and the drain goroutine
// that turns them into per-type reconciles/sweeps. A full buffer drops the event rather than
// stalling the updater; the periodic whole-GitTarget reconcile and the next transition recover
// any missed edge, so the per-type path is an accelerator, never the sole source of truth.
const lifecycleEventBuffer = 256

// gitTargetSnapshotSyncedCondition mirrors the controller's GitTargetConditionSnapshotSynced.
// It is duplicated as a string (rather than imported) because the controller package imports
// watch, so watch must not import controller. The per-type path only acts on a GitTarget that
// has completed its initial whole-GitTarget snapshot, so a transition during bootstrap does
// not race or double-commit with the bootstrap resync.
const gitTargetSnapshotSyncedCondition = "SnapshotSynced"

// startTypeLifecycleConsumer subscribes to the registry's lifecycle transitions and launches
// the drain goroutine that drives M12 per-type reconcile/sweep. It is a no-op without an
// EventRouter (zero-value Managers in unit tests have none) and runs at most once per Manager.
// Subscription happens before the first registry Update (the initial ReconcileForRuleChange),
// so cold-start activations are observed.
func (m *Manager) startTypeLifecycleConsumer(ctx context.Context, log logr.Logger) {
	if m.EventRouter == nil {
		return
	}
	m.lifecycleConsumerOnce.Do(func() {
		m.lifecycleEvents = make(chan typeset.LifecycleEvent, lifecycleEventBuffer)
		m.typeRegistryInstance().Subscribe(m.enqueueLifecycleEvent)
		go m.drainTypeLifecycleEvents(ctx, log)

		// The materialization checkpoint driver is a second subscriber on the same
		// buffered-drain discipline (DEC-L4 / §5): it consumes the Materializer's demand
		// events and lists ONLY claimed types into the checkpoint keyspace, off the
		// Materializer's dispatch (observers must not block). The Materializer's gate is fed
		// from handleTypeLifecycleEvent below.
		m.materializationWork = make(chan typeset.MaterializationEvent, materializationWorkBuffer)
		m.materializerInstance().Subscribe(m.enqueueMaterializationWork)
		go m.driveMaterialization(ctx, log)

		log.V(1).Info("type-lifecycle consumer started")
	})
}

// enqueueLifecycleEvent is the registry Observer: a non-blocking hand-off so the registry's
// updater is never stalled by per-type git work. It runs on whatever goroutine triggered the
// registry Update, synchronously under the registry's dispatch serialization, so the log here
// is the authoritative emission order of every transition the registry produced.
func (m *Manager) enqueueLifecycleEvent(ev typeset.LifecycleEvent) {
	if m.lifecycleEvents == nil {
		return
	}
	logLifecycleEvent(m.Log, "type-lifecycle event emitted", ev)
	select {
	case m.lifecycleEvents <- ev:
	default:
		// A dropped edge is a real correctness signal (the recompute path must recover it), so
		// it is logged at Info, not V(1).
		logLifecycleEvent(m.Log, "type-lifecycle event buffer full; dropping", ev)
	}
}

// logLifecycleEvent emits one structured Info line for a lifecycle transition. Transitions are
// rare (per-type edges, not per-object), so Info volume is low and the stream is grep-able out
// of the controller pod logs as an ordered timeline. Fields mirror typeset.LifecycleEvent.
func logLifecycleEvent(log logr.Logger, msg string, ev typeset.LifecycleEvent) {
	log.Info(msg,
		"kind", ev.Kind,
		"gvk", ev.GVK.String(),
		"gvr", ev.GVR.String(),
		"from", string(ev.From),
		"to", string(ev.To),
		"reason", string(ev.Reason),
		"generation", ev.Generation,
		"at", ev.At)
}

// drainTypeLifecycleEvents processes lifecycle events off the buffer on a dedicated goroutine,
// so a slow per-type gather/commit never blocks the registry updater or the reconcile loop.
func (m *Manager) drainTypeLifecycleEvents(ctx context.Context, log logr.Logger) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-m.lifecycleEvents:
			m.handleTypeLifecycleEvent(ctx, log, ev)
		}
	}
}

// handleTypeLifecycleEvent turns one registry transition into materialization gate + per-type
// git work. Every transition feeds the Materializer's followability gate (DEC-L4): it decides,
// per type, whether demand ∩ followability warrant a checkpoint and emits the demand events the
// driver acts on, so the checkpoint LIST runs only for CLAIMED types (L-3) — no longer
// unconditionally on every activation (the one behavioural change of §5).
//
// The git per-type reconcile/sweep (M12) still fires on the followability edge: TypeActivated
// reconciles the type into each synced GitTarget that watches it; TypeRemoved sweeps only that
// type's documents. That path streams its own data and commits, independent of the demand-gated
// checkpoint; it moves onto the checkpoint handshake only once the api-source-of-truth reconcile
// (R-steps) consumes it. Wobbling/Recovered/Refused carry no git action here.
func (m *Manager) handleTypeLifecycleEvent(ctx context.Context, log logr.Logger, ev typeset.LifecycleEvent) {
	m.materializerInstance().OnLifecycleEvent(ev)

	switch ev.Kind {
	case typeset.TypeActivated:
		m.reconcileTypeForSyncedTargets(ctx, log, ev.GVR)
	case typeset.TypeRemoved:
		m.sweepTypeFromSyncedTargets(ctx, log, ev.GVR)
	case typeset.TypeWobbling, typeset.TypeRecovered, typeset.TypeRefused:
		log.Info("type-lifecycle transition handled (no git action)",
			"kind", ev.Kind, "gvr", ev.GVR.String(), "reason", string(ev.Reason))
	}
}

// reconcileTypeForSyncedTargets fans a per-type reconcile to every snapshot-synced GitTarget
// whose resident table watches the activated type. A type the GitTarget does not watch is
// skipped; a target still mid-bootstrap is skipped (its whole-GitTarget resync covers the type).
func (m *Manager) reconcileTypeForSyncedTargets(ctx context.Context, log logr.Logger, gvr schema.GroupVersionResource) {
	for _, table := range m.allWatchedTypeTables() {
		if !tableWatchesGVR(table, gvr) {
			continue
		}
		// The SnapshotSynced gate is the seam the per-type scheduler will remove; logging the
		// skip makes visible how often a settled activation is currently suppressed by it.
		if !m.gitTargetSnapshotSynced(ctx, table.GitDest) {
			log.Info("per-type reconcile skipped: gitTarget not snapshot-synced",
				"gitDest", table.GitDest.String(), "gvr", gvr.String())
			continue
		}
		if err := m.EventRouter.EmitTypeReconcileForGitDest(ctx, table.GitDest, gvr); err != nil {
			log.Error(err, "per-type reconcile failed to enqueue",
				"gitDest", table.GitDest.String(), "gvr", gvr.String())
			continue
		}
		log.Info("per-type reconcile enqueued", "gitDest", table.GitDest.String(), "gvr", gvr.String())
		recordTypeLifecycleMetric(telemetry.TypeLifecycleReconcileTotal, table.GitDest)
	}
}

// sweepTypeFromSyncedTargets fans a per-type sweep to every snapshot-synced GitTarget. The
// removed type is no longer in any resident table, so the fan is over all targets; the sweep is
// idempotent and self-limiting — a GitTarget holding no documents of the type commits nothing —
// so this is safe even though it touches targets that never mirrored the type. Removals are
// rare (a CRD deletion), so the broad fan is acceptable.
func (m *Manager) sweepTypeFromSyncedTargets(ctx context.Context, log logr.Logger, gvr schema.GroupVersionResource) {
	for _, table := range m.allWatchedTypeTables() {
		if !m.gitTargetSnapshotSynced(ctx, table.GitDest) {
			log.Info("per-type sweep skipped: gitTarget not snapshot-synced",
				"gitDest", table.GitDest.String(), "gvr", gvr.String())
			continue
		}
		if err := m.EventRouter.EmitTypeSweepForGitDest(ctx, table.GitDest, gvr); err != nil {
			log.Error(err, "per-type sweep failed to enqueue", "gitDest", table.GitDest.String(), "gvr", gvr.String())
			continue
		}
		log.Info("per-type sweep enqueued", "gitDest", table.GitDest.String(), "gvr", gvr.String())
		recordTypeLifecycleMetric(telemetry.TypeLifecycleSweepTotal, table.GitDest)
	}
}

// gitTargetSnapshotSynced reports whether a GitTarget has completed its initial snapshot, the
// gate that keeps the per-type path additive to (never racing) the bootstrap resync. A
// GitTarget that cannot be read is treated as not synced, so the per-type path waits.
func (m *Manager) gitTargetSnapshotSynced(ctx context.Context, gitDest types.ResourceReference) bool {
	if m.Client == nil {
		return false
	}
	var gt configv1alpha1.GitTarget
	if err := m.Client.Get(
		ctx,
		k8stypes.NamespacedName{Name: gitDest.Name, Namespace: gitDest.Namespace},
		&gt,
	); err != nil {
		return false
	}
	return apimeta.IsStatusConditionTrue(gt.Status.Conditions, gitTargetSnapshotSyncedCondition)
}

// tableWatchesGVR reports whether a GitTarget's resident table includes the given type.
func tableWatchesGVR(table WatchedTypeTable, gvr schema.GroupVersionResource) bool {
	for _, wt := range table.Types {
		if wt.GVR == gvr {
			return true
		}
	}
	return false
}

// recordTypeLifecycleMetric increments a per-(GitTarget) lifecycle counter, a no-op until the
// counter is registered.
func recordTypeLifecycleMetric(counter metric.Int64Counter, gitDest types.ResourceReference) {
	if counter == nil {
		return
	}
	counter.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("gittarget_namespace", gitDest.Namespace),
		attribute.String("gittarget_name", gitDest.Name),
	))
}
