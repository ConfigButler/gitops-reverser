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
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/typeset"
)

// This file wires the demand axis of
// docs/design/stream/demand-driven-type-materialization-lifecycle.md into the watch
// Manager (step L-2): it feeds real demand into the typeset.Materializer leaf and ticks
// its periodic sweep. The Materializer stays a leaf (no client-go / Redis); all cluster
// resolution lives here in internal/watch.

// materializationSweepInterval is the production cadence of the Materializer's periodic
// pass (DEC-L5 / parent DEC-4: ~1h). The same pass GCs withdrawn leases and — once a driver
// exists (L-3+) — re-anchors the still-claimed and releases the no-longer-claimed. Because a
// claim is "live" iff renewed since the PREVIOUS sweep, this interval IS the release grace,
// with no dedicated constant. The GitTarget reconcile renews every RequeueLongInterval
// (minutes), comfortably shorter than this, so a healthy consumer always renews between
// sweeps (§9).
const materializationSweepInterval = time.Hour

// materializerInstance returns the lazily-built demand/materialization sibling to the
// followability registry, so a zero-value Manager (used widely in tests) needs no explicit
// setup. It mirrors typeRegistryInstance (manager_catalog.go).
func (m *Manager) materializerInstance() *typeset.Materializer {
	m.materializerInit.Do(func() {
		if m.materializer == nil {
			m.materializer = typeset.NewMaterializer()
		}
	})
	return m.materializer
}

// DeclareForGitTarget asserts one GitTarget's entire watched-type set as a self-renewing
// lease on the materialization axis (DEC-L3). It resolves the full set with the same
// fail-closed discipline the snapshot gather uses (resolveSnapshotGVRs), collapses it to the
// distinct GVRs the claim keys on, and declares them in one idempotent call: new types are
// claimed, present types renewed, and any type the GitTarget previously declared but now
// omits is left un-renewed and ages out at the next sweep (the implicit withdrawal).
//
// Fail closed exactly like the snapshot resolve: an unobservable API surface (or a wobbling
// watched type) returns an error and declares NOTHING — declaring a partial or empty set on
// an unobserved surface would read as a withdrawal and wrongly age out live claims. A
// legitimately empty set on an OBSERVABLE surface (the GitTarget watches nothing) is
// authoritative, so it is declared and withdraws all of that GitTarget's claims.
func (m *Manager) DeclareForGitTarget(ctx context.Context, gitDest types.ResourceReference) error {
	gvrs, err := m.resolveSnapshotGVRs(ctx, gitDest)
	if err != nil {
		return err
	}
	// The claim key is (GitTargetRef, GVR), scope-independent (DEC-L3 / §9 open Q), so the
	// resolved (GVR, namespace-scope) stream set collapses to its distinct GVRs. The ref is
	// the GitTarget's namespaced name (gitDest.String()), consistent with how rulestore keys
	// GitTargets and stable across reconciles; the object UID is the rejected alternative (it
	// would re-key on recreate and orphan the prior claims).
	m.materializerInstance().Declare(typeset.GitTargetRef(gitDest.String()), distinctClaimGVRs(gvrs))
	return nil
}

// distinctClaimGVRs collapses the resolved (GVR, namespace-scope) stream set to the distinct
// GVRs the claim table keys on, preserving the resolver's sorted order.
func distinctClaimGVRs(gvrs []snapshotGVR) []schema.GroupVersionResource {
	seen := make(map[schema.GroupVersionResource]struct{}, len(gvrs))
	out := make([]schema.GroupVersionResource, 0, len(gvrs))
	for _, sg := range gvrs {
		if _, ok := seen[sg.gvr]; ok {
			continue
		}
		seen[sg.gvr] = struct{}{}
		out = append(out, sg.gvr)
	}
	return out
}

// startMaterializationSweep launches the periodic Materializer sweep once, mirroring the
// buffered-drain goroutine lifecycle (type_lifecycle.go): a context-cancellable goroutine
// that ticks Sweep on materializationSweepInterval. It realises the lease age-out now
// (DEC-L5); the per-type re-anchor/release of checkpoints only becomes observable once L-3
// produces Synced types. The interval is injectable (materializationSweepIntervalOverride)
// so tests run it fast.
func (m *Manager) startMaterializationSweep(ctx context.Context, log logr.Logger) {
	m.materializationSweepOnce.Do(func() {
		interval := m.materializationSweepIntervalOverride
		if interval <= 0 {
			interval = materializationSweepInterval
		}
		go m.runMaterializationSweep(ctx, log, interval)
		log.V(1).Info("materialization sweep started", "interval", interval.String())
	})
}

// runMaterializationSweep ticks the Materializer's one periodic pass until the context is
// cancelled. Each tick GCs leases not renewed since the previous tick and branches per type
// (DEC-L5); with no driver yet (L-2) there are no Synced types, so a tick only ages out
// withdrawn claims and releases Requested -> Dormant.
func (m *Manager) runMaterializationSweep(ctx context.Context, log logr.Logger, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.materializerInstance().Sweep()
			log.V(1).Info("materialization sweep tick")
		}
	}
}
