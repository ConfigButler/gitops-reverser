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
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/typeset"
)

// This file wires the demand axis of
// docs/design/stream/demand-driven-type-materialization-lifecycle.md into the watch
// Manager (step L-2): it feeds real demand into the typeset.Materializer leaf and ticks
// its periodic sweep. The Materializer stays a leaf (no client-go / Redis); all cluster
// resolution lives here in internal/watch.

// materializationWorkBuffer bounds the channel between the Materializer's dispatch (the
// observer enqueue) and the checkpoint driver goroutine. A full buffer drops the event rather
// than stalling dispatch; the next lifecycle transition or sweep re-derives demand, and the
// checkpoint is a cache, so a missed edge only delays a refresh — it never corrupts state.
const materializationWorkBuffer = 256

// materializationSweepInterval is the production cadence of the Materializer's periodic
// pass (DEC-L5 / parent DEC-4: ~1h). The same pass GCs withdrawn leases and — once a driver
// exists (L-3+) — re-anchors the still-claimed and releases the no-longer-claimed. Because a
// claim is "live" iff renewed since the PREVIOUS sweep, this interval IS the release grace,
// with no dedicated constant. The GitTarget reconcile renews every RequeueLongInterval
// (minutes), comfortably shorter than this, so a healthy consumer always renews between
// sweeps (§9).
const materializationSweepInterval = time.Hour

// lateNudgeMinInterval is the per-type floor between late-event resync nudges, so a
// sustained run of out-of-order arrivals coalesces into bounded LIST work instead of a
// re-anchor per event. Within the floor the type's phase gate (pendingResync / a sync in
// flight) already absorbs repeats; the periodic sweep remains the final backstop.
const lateNudgeMinInterval = 15 * time.Second

// NudgeTypeResyncForLateEvent is the ingestion layer's late-event hook (wired in cmd):
// the per-type mirror diverted an audit event whose RV was below its stream's high-water
// (rejected from the main stream), so the ordered log will never replay it and only a fresh
// checkpoint can fold its effect in promptly — without the nudge the ~1h sweep is the
// backstop and the mirror serves stale state until then. The per-type stream key carries
// only (group, resource), so the claimed GVR is resolved off the Materializer inventory;
// an unclaimed or not-currently-Synced type is a no-op (its next sync covers the event).
// Best-effort and non-blocking: it runs on the audit webhook's request goroutine.
func (m *Manager) NudgeTypeResyncForLateEvent(group, resource string) {
	gvr, ok := m.claimedGVRForGroupResource(group, resource)
	if !ok {
		return
	}

	m.lateNudgeMu.Lock()
	now := time.Now()
	if m.lateNudgeAt == nil {
		m.lateNudgeAt = make(map[schema.GroupVersionResource]time.Time)
	}
	limited := now.Sub(m.lateNudgeAt[gvr]) < lateNudgeMinInterval
	if !limited {
		m.lateNudgeAt[gvr] = now
	}
	m.lateNudgeMu.Unlock()
	if limited {
		return
	}

	if m.materializerInstance().RequestResync(gvr) {
		// Info (not V(1)): a divert nudge is rare (15s floor per type) and is the signal that a
		// type's tail missed an out-of-order event and needs a re-anchor heal. Surfacing it at info
		// lets an incidental e2e/CI failure show whether a missing late-join object was diverted
		// (residual-e2e-flakes-2026-06-19.md, Flake B), without enabling debug logging.
		m.Log.Info("late audit event nudged a type resync", "gvr", gvr.String())
	}
}

// claimedGVRForGroupResource resolves the (group, resource) pair a per-type stream key
// carries back to the full claimed GVR: the registry's version-less index supplies the
// served versions (including retained-under-grace ones, so a wobble does not break the
// resolution), and the claim table picks the one a GitTarget actually demands. Only a
// claimed type is returned — an unclaimed one has no consumer whose mirror could go
// stale.
func (m *Manager) claimedGVRForGroupResource(group, resource string) (schema.GroupVersionResource, bool) {
	for _, rec := range m.typeRegistryInstance().ByGroupResource(group, resource) {
		gvr := rec.Identity.GVR
		if len(m.materializerInstance().Claimants(gvr)) > 0 {
			return gvr, true
		}
	}
	return schema.GroupVersionResource{}, false
}

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
	ref := typeset.GitTargetRef(gitDest.String())
	// The rule's FULLY-SPECIFIED GVRs (concrete group+version+resource) are known from the spec alone,
	// so they are claimed UNCONDITIONALLY — independent of discovery (G1, first-event-loss-on-reclaim-
	// plan.md §6.1). This is the fix for the run-3 loss: a freshly (re)installed CRD whose discovery has
	// not yet settled to Followable would otherwise resolve to an empty set and be dropped; claiming it
	// from the spec keeps the claim stable, and the Materializer drives its first sync the moment it
	// activates (DEC-L9). Wildcard/version-less entries are NOT here — they stay discovery-driven below.
	specified := m.fullySpecifiedClaimGVRs(gitDest)

	gvrs, err := m.resolveSnapshotGVRs(ctx, gitDest)
	if err != nil {
		// Fail-closed: the discovered (wildcard/version-less) set is unknown while the surface is
		// unobserved or a watched type is wobbling. The fully-specified set does NOT depend on it, so
		// still claim + Require those — a wobbling/not-yet-discovered fully-specified type stays claimed
		// and converges on activation. Return the error so the controller retries the discovered part on
		// the settle cadence; declaring only `specified` renews them without withdrawing them.
		if len(specified) > 0 {
			m.Log.Info("materialization declare (fully-specified only; discovered resolve failed closed)",
				"gitDest", gitDest.String(), "claimedCount", len(specified),
				"claimed", gvrsToStrings(specified), "err", err.Error())
			m.materializerInstance().Declare(ref, specified)
			for _, gvr := range specified {
				m.requireTypeMirror(ctx, m.Log, gvr)
			}
		} else {
			m.Log.Info("materialization declare resolved nothing (surface not observable / wobbling)",
				"gitDest", gitDest.String(), "err", err.Error())
		}
		return err
	}
	// The claim key is (GitTargetRef, GVR), scope-independent (DEC-L3 / §9 open Q), so the
	// resolved (GVR, namespace-scope) stream set collapses to its distinct GVRs, unioned with the
	// fully-specified GVRs (so a not-yet-Followable specified type is still claimed). The ref is
	// the GitTarget's namespaced name (gitDest.String()), consistent with how rulestore keys
	// GitTargets and stable across reconciles; the object UID is the rejected alternative (it
	// would re-key on recreate and orphan the prior claims).
	claimed := unionClaimGVRs(distinctClaimGVRs(gvrs), specified)
	// Diagnostic (first-event-loss-on-reclaim-plan.md S0): record exactly which GVRs this GitTarget
	// claims, so an empty (or wrong-GVR) claim set for a fully-specified rule — the suspected
	// first-event-loss trigger — is visible in a real run. Per-reconcile (minutes) info volume.
	m.Log.Info("materialization declare",
		"gitDest", gitDest.String(), "claimedCount", len(claimed), "claimed", gvrsToStrings(claimed))
	m.materializerInstance().Declare(ref, claimed)

	// Open the demand gate SYNCHRONOUSLY for every claimed type (G2, first-event-loss-on-reclaim-plan.md
	// §6.2): a claimed type must be mirrored from its FIRST audit event, before any checkpoint sync — so
	// Require here on the control-plane reconcile goroutine, never deferred to the async SyncRequested
	// hop where the first event could be gated out and lost. Idempotent and self-healing: gate.Require is
	// an SADD that only pings on a real change, so re-asserting the full claimed set each reconcile is
	// cheap and recovers from a transient gate-write failure. The gate is closed on the Unclaimed event
	// (sweep GC of the last claim), so Required tracks claimed-ness exactly.
	for _, gvr := range claimed {
		m.requireTypeMirror(ctx, m.Log, gvr)
	}

	// Drop coverage watermarks for types this GitTarget no longer claims (a rule change), so a later
	// re-add restarts at NotReconciled instead of gating the tail against a stale boundary the fan-out
	// would honor before the fresh reconcile re-publishes one (§7.3.7). This runs only on a successful,
	// observable resolve above, matching the Declare's own fail-closed discipline.
	m.pruneTargetTypeWatermarks(gitDest, claimed)

	// Drive ONE initial-backfill splice reconcile per (GitTarget, type) — only for a type this
	// GitTarget NEWLY claims that is already Synced. That is the initial sync of an already-
	// materialized type (a newly-Ready target, a new rule, or a restored-Synced type after a
	// restart); after it the per-event audit tail owns live changes (with their authorship), so we
	// do NOT re-fold the log on every Declare (that would re-attribute live changes to the bulk
	// reconcile's default author and churn Git). A not-yet-Synced claimed type needs no trigger
	// here: its eventual TypeSynced fans the reconcile to every watcher. EmitTypeReconcileForGitDest
	// is idempotent and self-gating, so a one-shot per claim is sufficient and a re-anchor backstops.
	if m.EventRouter != nil {
		for _, gvr := range m.newlyDeclaredSyncedGVRs(gitDest, claimed) {
			// A newly-claimed already-Synced type's initial backfill (heal=false): establish the
			// mirror promptly, not deferred as housekeeping.
			if reconcileErr := m.EventRouter.EmitTypeReconcileForGitDest(
				ctx,
				gitDest,
				gvr,
				false,
			); reconcileErr != nil {
				// Rec 2 / Gap 2: a failed initial backfill must be RETRIED, not silently recorded as
				// done. Un-record the type so the NEXT GitTarget reconcile re-classifies it as
				// newly-declared and re-attempts the backfill, and do NOT start the freshness tail for a
				// target whose baseline never landed (no event-tail ahead of an un-backfilled target).
				// Without this the type stays recorded as declared forever and the hole is permanent.
				// The next reconcile (RequeueLongInterval, or sooner on any rule/provider change) retries
				// it; the periodic re-anchor heal (Slice C), which re-fans the reconcile to every watcher,
				// is the longer backstop.
				m.Log.V(1).Info("declare reconcile failed; will retry next reconcile",
					"gitDest", gitDest.String(), "gvr", gvr.String(), "err", reconcileErr.Error())
				m.forgetDeclaredGVR(gitDest, gvr)
				continue
			}
			// Start the freshness tail for this newly-claimed already-Synced type, anchored at its
			// checkpoint rv. TypeSynced is re-announced on (re)activation only for a type ALREADY
			// claimed when discovery activates it; the common boot order is the reverse — a GitTarget
			// Declares a restored-Synced type AFTER its activation — so without this the tail would not
			// start until the next discovery resync (minutes). Idempotent: a no-op if already running.
			rv, _ := m.materializerInstance().Checkpoint(gvr)
			m.startTypeAuditTail(ctx, m.Log, gvr, rv)
		}
	}
	return nil
}

// forgetDeclaredGVR removes a single type from a GitTarget's recorded declaration so a failed
// initial backfill is re-attempted on the next reconcile (Rec 2): newlyDeclaredSyncedGVRs records the
// whole claimed set up front, which would otherwise mark a type whose backfill then errored as
// "already declared" and never retry it. A no-op for an unknown GitTarget or a type it never recorded.
func (m *Manager) forgetDeclaredGVR(gitDest types.ResourceReference, gvr schema.GroupVersionResource) {
	m.declaredGVRsMu.Lock()
	defer m.declaredGVRsMu.Unlock()
	if set := m.declaredGVRs[gitDest.String()]; set != nil {
		delete(set, gvr)
	}
}

// newlyDeclaredSyncedGVRs records this GitTarget's current claimed set and returns the subset that
// is BOTH newly claimed (absent from the prior Declare) AND already Synced — the types whose one
// initial-backfill splice this Declare should drive (see DeclareForGitTarget). Recording every
// current claim (Synced or not) is correct: a not-yet-Synced new claim is covered by its eventual
// TypeSynced fan, so it never needs a Declare-time backfill even after it later becomes Synced.
func (m *Manager) newlyDeclaredSyncedGVRs(
	gitDest types.ResourceReference,
	claimed []schema.GroupVersionResource,
) []schema.GroupVersionResource {
	key := gitDest.String()
	m.declaredGVRsMu.Lock()
	defer m.declaredGVRsMu.Unlock()
	prior := m.declaredGVRs[key]
	current := make(map[schema.GroupVersionResource]struct{}, len(claimed))
	var newlySynced []schema.GroupVersionResource
	for _, gvr := range claimed {
		current[gvr] = struct{}{}
		if _, had := prior[gvr]; had {
			continue
		}
		if phase, _ := m.materializerInstance().Phase(gvr); phase == typeset.PhaseSynced {
			newlySynced = append(newlySynced, gvr)
		}
	}
	if m.declaredGVRs == nil {
		m.declaredGVRs = map[string]map[schema.GroupVersionResource]struct{}{}
	}
	m.declaredGVRs[key] = current
	return newlySynced
}

// ForgetGitTargetDeclaration drops the watch-side record of a GitTarget's last-Declared type-set
// (the diff-wake's newly-claimed cache, newlyDeclaredSyncedGVRs) when the GitTarget is deleted.
// Without it a GitTarget recreated with the same namespaced name inherits the dead one's
// declaration, so its types read as "already declared" and its initial backfill splice never
// fires — the recreate silently produces no snapshot commit. Clearing on delete makes a recreate a
// genuine fresh claim. The materializer claim itself ages out on its own lease (sweep), so this
// only resets the diff-wake cache. A no-op for an unknown GitTarget.
//
// It also clears the GitTarget's per-type coverage watermarks (clearTargetTypeWatermarks): a
// recreated target must restart at NotReconciled so the audit tail suppresses every entry until its
// fresh reconcile re-establishes a boundary — inheriting a dead target's stale-high Hc would
// silently suppress the recreate's legitimate live events
// (signing-snapshot-tail-replay-failure-investigation.md §7.3).
func (m *Manager) ForgetGitTargetDeclaration(gitDest types.ResourceReference) {
	m.clearTargetTypeWatermarks(gitDest)
	m.declaredGVRsMu.Lock()
	defer m.declaredGVRsMu.Unlock()
	delete(m.declaredGVRs, gitDest.String())
}

// RestoreSyncedCheckpoint replays one durable per-type checkpoint into the Materializer on
// boot (DEC-L6), marking the type Synced at rv so a restart resumes serving it without a
// re-fill. It is the watch-side half of the HA seam: the composition root reads the checkpoints
// from the mirror (queue.RedisObjectsSnapshot.LoadSyncedCheckpoints) and replays each here, so
// neither typeset (a leaf) nor the queue (Redis-only) need to know about the other. Call it at
// boot, before the manager starts its reconcile/sweep/driver. A blank resource or rv is ignored.
func (m *Manager) RestoreSyncedCheckpoint(group, version, resource, rv string) {
	if resource == "" || rv == "" {
		return
	}
	m.materializerInstance().RestoreSynced(
		schema.GroupVersionResource{Group: group, Version: version, Resource: resource}, rv)
}

// GitTargetMaterializationSummary is a bounded per-GitTarget roll-up of demand-axis state —
// the data the GitTarget status surfaces (L-6/L10). It buckets on SERVICEABILITY (a usable
// checkpoint), not on phase == Synced, so a periodic re-anchor (Synced→Resyncing→Synced) does
// not flap any liveness signal built on it (Gap 6 / status-design §3.2):
//
//   - Synced counts serviceable types (Synced, Resyncing, or Failing-with-a-prior-checkpoint);
//   - Pending counts followable claimed types still building their FIRST checkpoint
//     (Dormant/Requested/Syncing) — genuinely not-yet-serviceable progressing work;
//   - Failing counts claimed types whose last sync errored (with or without a checkpoint), the
//     operator-visible stall signal; a Failing type WITH a checkpoint is also counted in Synced;
//   - NotFollowable counts claimed types the registry does not currently serve (claim-vs-refused);
//   - FailingNoCheckpoint is the subset of Failing with no checkpoint to serve — together with
//     NotFollowable it is what makes the data plane Degraded rather than merely Initializing. It
//     is not surfaced on the CR; it only feeds the controller's phase derivation.
type GitTargetMaterializationSummary struct {
	Claimed             int
	Synced              int
	Pending             int
	Failing             int
	NotFollowable       int
	FailingNoCheckpoint int
}

// MaterializationSummaryForGitTarget rolls up, for one GitTarget, how many types it claims and
// where they sit in the checkpoint lifecycle (L-6), bucketed on serviceability (§3.2). It is
// bounded (counts, not a per-type list) and scoped to the claims keyed by this GitTarget's ref
// (gitDest.String()).
func (m *Manager) MaterializationSummaryForGitTarget(gitDest types.ResourceReference) GitTargetMaterializationSummary {
	ref := typeset.GitTargetRef(gitDest.String())
	var s GitTargetMaterializationSummary
	for _, t := range m.materializerInstance().Inventory() {
		if !claimantsInclude(t.Claimants, ref) {
			continue
		}
		s.Claimed++
		switch {
		case !t.Followable:
			// A type the registry does not currently serve (a claim on a not-installed CRD or a
			// typo'd rule). Followability loss force-releases the checkpoint, so it is never
			// serviceable here — surfaced as the claim-vs-refused mismatch only.
			s.NotFollowable++
		case t.Serviceable():
			s.Synced++
			if t.Phase == typeset.PhaseFailing {
				s.Failing++
			}
		case t.Phase == typeset.PhaseFailing:
			// Failing with no prior checkpoint to serve — a first-sync stall, a degraded signal.
			s.Failing++
			s.FailingNoCheckpoint++
		default:
			// Dormant/Requested/Syncing and followable: the first checkpoint is in flight.
			s.Pending++
		}
	}
	return s
}

// claimantsInclude reports whether ref holds a claim in the sorted claimant slice.
func claimantsInclude(claimants []typeset.GitTargetRef, ref typeset.GitTargetRef) bool {
	for _, c := range claimants {
		if c == ref {
			return true
		}
	}
	return false
}

// fullySpecifiedClaimGVRs returns the GVRs a GitTarget's rules name with a CONCRETE group AND version
// AND resource (no "*", no omitted version) — claims fully determined by the rule spec, so they can be
// claimed WITHOUT consulting discovery. Claiming these unconditionally (rather than only the currently-
// Followable subset) is the G1 fix (first-event-loss-on-reclaim-plan.md §6.1): a not-yet-discovered or
// transiently-wobbling type stays claimed, and the Materializer drives its first sync the moment it
// becomes followable (DEC-L9). Wildcard/version-less entries are excluded — they are inherently
// discovery-driven and still resolve through the followable table. Group "" (core) is concrete.
func (m *Manager) fullySpecifiedClaimGVRs(gitDest types.ResourceReference) []schema.GroupVersionResource {
	if m.RuleStore == nil {
		return nil
	}
	acc := &gvrAccumulator{seen: map[schema.GroupVersionResource]struct{}{}}
	for _, rule := range m.RuleStore.SnapshotWatchRules() {
		if rule.GitTargetRef != gitDest.Name || rule.GitTargetNamespace != gitDest.Namespace {
			continue
		}
		for _, rr := range rule.ResourceRules {
			acc.addFullySpecified(rr.APIGroups, rr.APIVersions, rr.Resources)
		}
	}
	for _, rule := range m.RuleStore.SnapshotClusterWatchRules() {
		if rule.GitTargetRef != gitDest.Name || rule.GitTargetNamespace != gitDest.Namespace {
			continue
		}
		for _, rr := range rule.Rules {
			acc.addFullySpecified(rr.APIGroups, rr.APIVersions, rr.Resources)
		}
	}
	return acc.out
}

// gvrAccumulator collects distinct GVRs across rule entries for fullySpecifiedClaimGVRs.
type gvrAccumulator struct {
	seen map[schema.GroupVersionResource]struct{}
	out  []schema.GroupVersionResource
}

// addFullySpecified appends every concrete GVR named by one (groups, versions, resources) rule entry,
// skipping any "*"/blank component (those are discovery-driven, not claimable from the spec alone).
// Group "" (core) is concrete. Duplicates across entries are dropped.
func (a *gvrAccumulator) addFullySpecified(groups, versions, resources []string) {
	for _, g := range groups {
		for _, v := range versions {
			for _, r := range resources {
				a.addOne(g, v, r)
			}
		}
	}
}

func (a *gvrAccumulator) addOne(group, version, resource string) {
	if group == "*" || version == "" || version == "*" || resource == "" || resource == "*" {
		return
	}
	gvr := schema.GroupVersionResource{Group: group, Version: version, Resource: normalizeResource(resource)}
	if _, dup := a.seen[gvr]; dup {
		return
	}
	a.seen[gvr] = struct{}{}
	a.out = append(a.out, gvr)
}

// unionClaimGVRs returns a ∪ b de-duplicated, preserving a's order then b's new entries.
func unionClaimGVRs(a, b []schema.GroupVersionResource) []schema.GroupVersionResource {
	if len(b) == 0 {
		return a
	}
	seen := make(map[schema.GroupVersionResource]struct{}, len(a)+len(b))
	out := make([]schema.GroupVersionResource, 0, len(a)+len(b))
	for _, set := range [][]schema.GroupVersionResource{a, b} {
		for _, gvr := range set {
			if _, ok := seen[gvr]; ok {
				continue
			}
			seen[gvr] = struct{}{}
			out = append(out, gvr)
		}
	}
	return out
}

// gvrsToStrings renders a GVR slice as strings for diagnostic logging (S0), preserving order.
func gvrsToStrings(gvrs []schema.GroupVersionResource) []string {
	out := make([]string, 0, len(gvrs))
	for _, g := range gvrs {
		out = append(out, g.String())
	}
	return out
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
// (DEC-L5): a still-claimed Synced type is flagged for a re-anchor (the driver picks it up via
// SyncRequested), a no-longer-claimed type is released.
func (m *Manager) runMaterializationSweep(ctx context.Context, log logr.Logger, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.materializerInstance().Sweep()
			m.recordMaterializationGauges(ctx)
			log.V(1).Info("materialization sweep tick")
		}
	}
}

// enqueueMaterializationWork is the Materializer observer: a non-blocking hand-off so the
// Materializer's dispatch is never stalled by the checkpoint LIST. It runs on whatever
// goroutine produced the event (the lifecycle drain for an activation, the GitTarget reconcile
// for a Declare, the sweep ticker for a re-anchor/release), synchronously under the
// Materializer's dispatch serialization.
func (m *Manager) enqueueMaterializationWork(ev typeset.MaterializationEvent) {
	if m.materializationWork == nil {
		return
	}
	select {
	case m.materializationWork <- ev:
	default:
		m.Log.V(1).Info("materialization work buffer full; dropping",
			"kind", ev.Kind, "gvr", ev.GVR.String())
	}
}

// driveMaterialization is the checkpoint driver goroutine: it consumes the Materializer's
// demand events off the buffer (so a slow LIST never blocks the Materializer or the registry
// updater) and turns them into checkpoint writes/drops. It mirrors drainTypeLifecycleEvents.
func (m *Manager) driveMaterialization(ctx context.Context, log logr.Logger) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-m.materializationWork:
			m.handleMaterializationEvent(ctx, log, ev)
		}
	}
}

// handleMaterializationEvent acts on one demand event. SyncRequested drives the checkpoint LIST
// (T1/T4); Unclaimed closes the demand gate when the last claim is withdrawn; Released drops the
// checkpoint keyspace (demand GC or followability loss); TypeSynced is the R2 wake — a freshly-
// serviceable checkpoint fans a per-type splice reconcile to every GitTarget watching the type.
// SyncStarted/SyncFailed are observability. The demand-gate OPEN edge is not here — it is synchronous
// on the claim (DeclareForGitTarget), so a claimed type is mirrored before any sync.
func (m *Manager) handleMaterializationEvent(ctx context.Context, log logr.Logger, ev typeset.MaterializationEvent) {
	if telemetry.MaterializationSyncEventsTotal != nil {
		telemetry.MaterializationSyncEventsTotal.Add(ctx, 1,
			metric.WithAttributes(attribute.String("kind", string(ev.Kind))))
	}
	switch ev.Kind {
	case typeset.SyncRequested:
		// Drive the demand-gated checkpoint LIST. The gate was already opened synchronously when the
		// type was claimed (DeclareForGitTarget Requires it), so it is mirroring by now — we do NOT
		// gate here, so a claimed type's first events are never lost to the async SyncRequested hop
		// (first-event-loss-on-reclaim-plan.md §6.2).
		m.runTypeCheckpointSync(ctx, log, ev.GVR)
	case typeset.Unclaimed:
		// Demand-gate CLOSE edge: the last claim was withdrawn (sweep GC), so stop mirroring this type
		// across pods. This tracks the CLAIM, not the checkpoint — a followability wobble force-releases
		// the checkpoint (Released) while the claim survives, and such a type must keep being mirrored.
		m.unrequireTypeMirror(ctx, log, ev.GVR)
	case typeset.Released:
		m.stopTypeAuditTail(ev.GVR)
		m.clearTypeObjects(ctx, log, ev.GVR)
		// Reclaim the released checkpoint's audit footprint (DG2) — the audit-side twin of
		// clearTypeObjects. The gate flag is NOT touched here (it moves with the claim, via Unclaimed):
		// a wobble force-release keeps the claim, so the type stays Required and keeps mirroring.
		m.deleteTypeAuditKeys(ctx, log, ev.GVR)
	case typeset.TypeSynced:
		// The checkpoint just became serviceable. The FIRST TypeSynced (tail not yet running) fans
		// the initial backfill splice to every watching GitTarget. A LATER TypeSynced — a periodic
		// sweep re-anchor or a late-event nudge, with the tail already live — re-folds the refreshed
		// checkpoint as a HEAL resync: it catches drift the in-order tail cannot express (orphans, a
		// deletecollection, a diverted event) and was DISABLED by 8f2ad84 because a force-finalizing
		// re-splice stole an open CommitRequest window. Routing it through heal=true lets the worker
		// DEFER it until no window is open, restoring the checkpoint's correctness role without the
		// steal (Rec 1). The tail keeps git fresh for in-order live edits between re-anchors.
		log.V(1).Info("materialization event", "kind", ev.Kind, "gvr", ev.GVR.String(), "rv", ev.RV)
		m.reconcileTypeFan()(ctx, log, ev.GVR, m.isAuditTailRunning(ev.GVR))
		m.startTypeAuditTail(ctx, log, ev.GVR, ev.RV)
	case typeset.SyncStarted, typeset.SyncFailed:
		log.V(1).Info("materialization event",
			"kind", ev.Kind, "gvr", ev.GVR.String(), "phase", ev.Phase, "rv", ev.RV)
	}
	// Refresh the phase-distribution / demand gauges after every transition — materialization
	// events are per-type (rare), and a gauge only needs re-recording when state actually moved.
	m.recordMaterializationGauges(ctx)
}

// recordMaterializationGauges publishes the current phase distribution and demand surface from
// one Inventory snapshot (L-6/L10): how many types sit in each phase, how many are claimed, and
// how many are claimed-but-not-followable (the claim-vs-refused mismatch). All zero phases are
// re-recorded so an emptied phase reads 0 rather than a stale value.
func (m *Manager) recordMaterializationGauges(ctx context.Context) {
	if telemetry.MaterializationTypePhase == nil &&
		telemetry.MaterializationClaimedTypes == nil &&
		telemetry.MaterializationClaimedUnfollowable == nil {
		return
	}
	phaseCounts := map[typeset.Phase]int64{
		typeset.PhaseDormant: 0, typeset.PhaseRequested: 0, typeset.PhaseSyncing: 0,
		typeset.PhaseSynced: 0, typeset.PhaseResyncing: 0, typeset.PhaseFailing: 0,
	}
	var claimed, claimedUnfollowable int64
	for _, t := range m.materializerInstance().Inventory() {
		phaseCounts[t.Phase]++
		if len(t.Claimants) > 0 {
			claimed++
			if !t.Followable {
				claimedUnfollowable++
			}
		}
	}
	if telemetry.MaterializationTypePhase != nil {
		for phase, n := range phaseCounts {
			telemetry.MaterializationTypePhase.Record(ctx, n,
				metric.WithAttributes(attribute.String("phase", string(phase))))
		}
	}
	if telemetry.MaterializationClaimedTypes != nil {
		telemetry.MaterializationClaimedTypes.Record(ctx, claimed)
	}
	if telemetry.MaterializationClaimedUnfollowable != nil {
		telemetry.MaterializationClaimedUnfollowable.Record(ctx, claimedUnfollowable)
	}
}

// runTypeCheckpointSync performs one demand-driven checkpoint sync for a type: it asks the
// Materializer to begin (which gates on followable ∩ unfrozen ∩ the right phase, so a wobbling
// type or an in-flight sync is a no-op), lists the type into the checkpoint keyspace, and
// records the outcome. A failed LIST leaves the prior checkpoint serving (L5); a success pins
// the new revision. This is the only place that lists a type for materialization — it runs for
// CLAIMED types only, because the Materializer emits SyncRequested only for claimed types.
func (m *Manager) runTypeCheckpointSync(ctx context.Context, log logr.Logger, gvr schema.GroupVersionResource) {
	if !m.materializerInstance().BeginSync(gvr) {
		return
	}
	rv, err := m.mirrorTypeObjects(ctx, log, gvr)
	if err != nil {
		log.Error(err, "materialization checkpoint sync failed", "gvr", gvr.String())
		m.materializerInstance().SyncFailed(gvr)
		return
	}
	m.materializerInstance().SyncSucceeded(gvr, rv)
	m.trimTypeAuditLog(ctx, log, gvr, rv)
}

// trimTypeAuditLog bounds the type's audit log to the just-pinned checkpoint cursor (R1, §6).
// It runs right after SyncSucceeded, when the new checkpoint is the single serving one, so the
// trim cursor is exactly rv. Best-effort: a trim failure leaves a longer-than-necessary log (the
// splice still replays correctly), so it is logged and swallowed, never failing the sync. A nil
// trimmer or a blank rv is a no-op.
func (m *Manager) trimTypeAuditLog(ctx context.Context, log logr.Logger, gvr schema.GroupVersionResource, rv string) {
	if m.AuditLogTrimmer == nil || rv == "" {
		return
	}
	if err := m.AuditLogTrimmer.TrimTypeAuditLog(ctx, gvr.Group, gvr.Resource, rv); err != nil {
		log.Error(err, "materialization audit-log trim failed", "gvr", gvr.String(), "cursor", rv)
		return
	}
	log.V(1).Info("materialization audit-log trimmed", "gvr", gvr.String(), "cursor", rv)
}

// requireTypeMirror marks a type as wanted in the shared demand gate. It is called synchronously for
// every claimed GVR on each GitTarget reconcile (DeclareForGitTarget), so the audit webhook is already
// mirroring a claimed type by the time its first event arrives — before any checkpoint sync. Idempotent:
// re-asserting a present type adds nothing and does not ping. Best-effort and nil-safe — a gate write
// failure is logged and retried on the next reconcile, never fatal.
func (m *Manager) requireTypeMirror(ctx context.Context, log logr.Logger, gvr schema.GroupVersionResource) {
	if m.MirrorGate == nil {
		return
	}
	if err := m.MirrorGate.Require(ctx, gvr); err != nil {
		log.Error(err, "demand-gate require failed", "gvr", gvr.String())
	}
}

// unrequireTypeMirror marks a type as no longer wanted on the Unclaimed event (the sweep's GC of the
// last claim), stopping new mirroring across pods within a ping. It is keyed to the CLAIM, not the
// checkpoint Released event — a followability wobble force-releases the checkpoint while the claim
// survives, and such a type must keep being mirrored. Best-effort and nil-safe.
func (m *Manager) unrequireTypeMirror(ctx context.Context, log logr.Logger, gvr schema.GroupVersionResource) {
	if m.MirrorGate == nil {
		return
	}
	if err := m.MirrorGate.Unrequire(ctx, gvr); err != nil {
		log.Error(err, "demand-gate unrequire failed", "gvr", gvr.String())
	}
}

// deleteTypeAuditKeys reclaims a released type's audit footprint (DG2) — the audit-side twin of
// clearTypeObjects, fired on the same grace-protected Released event. Best-effort and nil-safe.
func (m *Manager) deleteTypeAuditKeys(ctx context.Context, log logr.Logger, gvr schema.GroupVersionResource) {
	if m.AuditKeyDeleter == nil {
		return
	}
	if err := m.AuditKeyDeleter.DeleteType(ctx, gvr.Group, gvr.Resource); err != nil {
		log.Error(err, "demand-gate delete-type failed", "gvr", gvr.String())
	}
}
