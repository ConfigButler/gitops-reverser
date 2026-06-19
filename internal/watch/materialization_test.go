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
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/typeset"
)

// activate is the followability edge the materializer gate consumes in these tests.
func activate(gvr schema.GroupVersionResource) typeset.LifecycleEvent {
	return typeset.LifecycleEvent{Kind: typeset.TypeActivated, GVR: gvr}
}

// secretsGVR is defined in manager_snapshot_test.go (same package): core v1 secrets.

// TestDeclareForGitTarget_ClaimsResolvedSetAndIsIdempotent proves the L-2 wiring: a
// reconcile declares the GitTarget's full resolved type-set on the materialization axis,
// and re-reconciling is an idempotent renew (DEC-L3) — the claimant set stays stable.
func TestDeclareForGitTarget_ClaimsResolvedSetAndIsIdempotent(t *testing.T) {
	manager, store := makeWatchedTypeManager(t)
	store.AddOrUpdateClusterWatchRule(
		clusterRuleForResource("rule-cm", "configmaps"),
		"test-target", "test-ns", "test-provider", "test-ns", "main", "test-path",
	)
	store.AddOrUpdateClusterWatchRule(
		clusterRuleForResource("rule-sec", "secrets"),
		"test-target", "test-ns", "test-provider", "test-ns", "main", "test-path",
	)
	gitDest := gitDestRef("test-target")
	ref := typeset.GitTargetRef(gitDest.String())

	require.NoError(t, manager.DeclareForGitTarget(context.Background(), gitDest))
	mat := manager.materializerInstance()
	require.Equal(t, []typeset.GitTargetRef{ref}, mat.Claimants(configMapGVR))
	require.Equal(t, []typeset.GitTargetRef{ref}, mat.Claimants(secretsGVR))

	// Re-declaring the same resolved set is an idempotent renew: the claimants are stable.
	require.NoError(t, manager.DeclareForGitTarget(context.Background(), gitDest))
	require.Equal(t, []typeset.GitTargetRef{ref}, mat.Claimants(configMapGVR))
	require.Equal(t, []typeset.GitTargetRef{ref}, mat.Claimants(secretsGVR))
}

// TestDeclareForGitTarget_OnlyClaimsResolvedTypes proves a type no rule resolves to is never
// claimed. Combined with the sweep's lease GC (typeset.Materializer tests), this is how a
// type dropped from a WatchRule stops being renewed and is released at the next sweep: a
// later, smaller resolved set simply omits it.
func TestDeclareForGitTarget_OnlyClaimsResolvedTypes(t *testing.T) {
	manager, store := makeWatchedTypeManager(t)
	store.AddOrUpdateClusterWatchRule(
		clusterRuleForResource("rule-cm", "configmaps"),
		"test-target", "test-ns", "test-provider", "test-ns", "main", "test-path",
	)
	gitDest := gitDestRef("test-target")
	ref := typeset.GitTargetRef(gitDest.String())

	require.NoError(t, manager.DeclareForGitTarget(context.Background(), gitDest))
	mat := manager.materializerInstance()
	require.Equal(t, []typeset.GitTargetRef{ref}, mat.Claimants(configMapGVR))
	require.Empty(t, mat.Claimants(secretsGVR), "a type no rule resolves to must not be claimed")
}

// TestDeclareForGitTarget_FailsClosedDeclaresNothing proves the fail-closed discipline: an
// unobservable API surface returns an error and declares nothing — a partial or empty set on
// an unobserved surface would read as a withdrawal and wrongly age out live claims.
func TestDeclareForGitTarget_FailsClosedDeclaresNothing(t *testing.T) {
	manager := &Manager{
		Log: logr.Discard(),
		discoveryClient: func() (apiResourceDiscovery, error) {
			return nil, errors.New("discovery unavailable")
		},
	}
	gitDest := gitDestRef("test-target")

	err := manager.DeclareForGitTarget(context.Background(), gitDest)
	require.Error(t, err, "an unobservable API surface must fail closed")
	require.Empty(t, manager.materializerInstance().Claimants(configMapGVR),
		"a failed resolve must declare nothing")
}

// fullySpecifiedClusterRule builds a ClusterWatchRule naming a single type by its full
// group+version+resource — the shape of the e2e WatchRule that flaked
// (apiGroups:[crd-lifecycle…], apiVersions:[v1], resources:[icecreamorders]).
func fullySpecifiedClusterRule(name, group, version, resource string) configv1alpha1.ClusterWatchRule {
	return configv1alpha1.ClusterWatchRule{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: configv1alpha1.ClusterWatchRuleSpec{
			TargetRef: configv1alpha1.NamespacedTargetReference{Name: "test-target", Namespace: "test-ns"},
			Rules: []configv1alpha1.ClusterResourceRule{{
				APIGroups:   []string{group},
				APIVersions: []string{version},
				Resources:   []string{resource},
				Scope:       configv1alpha1.ResourceScopeNamespaced,
			}},
		},
	}
}

// TestDeclareForGitTarget_FullySpecifiedNotYetDiscoveredTypeClaimsNothing is the S0 reproducer
// (first-event-loss-on-reclaim-plan.md §1.1, W2): a rule that names a type by full
// group+version+resource is NOT claimed when that type is not (yet) in the registry's Followable set —
// because the claim is resolved through Followable() rather than from the rule spec. This is the
// deterministic, in-code shape of the run-3 loss: a freshly (re)installed CRD whose discovery has not
// settled is silently un-claimed, so the gate never Requires it and its first events are dropped.
//
// It documents TODAY's behaviour (claims nothing); slice S3 inverts the assertion — the same
// fully-specified rule must claim its GVR unconditionally — which is the heart of the fix.
func TestDeclareForGitTarget_FullySpecifiedNotYetDiscoveredTypeClaimsNothing(t *testing.T) {
	manager, store := makeWatchedTypeManager(t)
	// crd-lifecycle.e2e.example.com/v1 icecreamorders is NOT in the common test catalog (which only
	// serves icecreamorders under shop.example.com), so it stands in for a not-yet-discovered CRD.
	store.AddOrUpdateClusterWatchRule(
		fullySpecifiedClusterRule("rule-crd-lifecycle", "crd-lifecycle.e2e.example.com", "v1", "icecreamorders"),
		"test-target", "test-ns", "test-provider", "test-ns", "main", "test-path",
	)
	gitDest := gitDestRef("test-target")
	notYetDiscovered := schema.GroupVersionResource{
		Group: "crd-lifecycle.e2e.example.com", Version: "v1", Resource: "icecreamorders",
	}

	require.NoError(t, manager.DeclareForGitTarget(context.Background(), gitDest),
		"an observable surface that simply lacks the type is not an error — it resolves to empty")
	require.Empty(t, manager.materializerInstance().Claimants(notYetDiscovered),
		"TODAY (the bug): a fully-specified rule for a not-yet-discovered type claims NOTHING; "+
			"S3 will invert this to require the claim unconditionally")
}

// recordingGate is a fake TypeMirrorGate that records Require/Unrequire calls in order.
type recordingGate struct {
	required   []schema.GroupVersionResource
	unrequired []schema.GroupVersionResource
}

func (g *recordingGate) Require(_ context.Context, gvr schema.GroupVersionResource) error {
	g.required = append(g.required, gvr)
	return nil
}

func (g *recordingGate) Unrequire(_ context.Context, gvr schema.GroupVersionResource) error {
	g.unrequired = append(g.unrequired, gvr)
	return nil
}

// TestDeclareForGitTarget_OpensGateSynchronouslyForClaimedType is the S2 core (G2,
// first-event-loss-on-reclaim-plan.md §6.2): a claimed type must be Required in the demand gate BY THE
// TIME DeclareForGitTarget returns — synchronously, before any checkpoint sync — so the audit webhook is
// already mirroring it when its first event arrives. Before S2 the gate was only Required later, off the
// async SyncRequested hop, so a claimed type's first events could be gated out and lost.
func TestDeclareForGitTarget_OpensGateSynchronouslyForClaimedType(t *testing.T) {
	manager, store := makeWatchedTypeManager(t)
	gate := &recordingGate{}
	manager.MirrorGate = gate
	store.AddOrUpdateClusterWatchRule(
		clusterRuleForResource("rule-cm", "configmaps"),
		"test-target", "test-ns", "test-provider", "test-ns", "main", "test-path",
	)

	require.NoError(t, manager.DeclareForGitTarget(context.Background(), gitDestRef("test-target")))

	// Synchronous: the gate is Required already, with no sync having run.
	require.Contains(t, gate.required, configMapGVR,
		"a claimed type must be Required synchronously by the time Declare returns")
	ph, _ := manager.materializerInstance().Phase(configMapGVR)
	require.NotEqual(t, typeset.PhaseSynced, ph, "the gate opens before any checkpoint sync")
}

// TestHandleMaterializationEvent_UnclaimedUnrequires proves the demand-gate CLOSE edge: an Unclaimed
// event (the sweep's GC of the last claim) Unrequires the type so it stops being mirrored.
func TestHandleMaterializationEvent_UnclaimedUnrequires(t *testing.T) {
	gate := &recordingGate{}
	m := &Manager{Log: logr.Discard(), MirrorGate: gate}

	m.handleMaterializationEvent(context.Background(), logr.Discard(),
		typeset.MaterializationEvent{Kind: typeset.Unclaimed, GVR: configMapGVR})

	require.Equal(t, []schema.GroupVersionResource{configMapGVR}, gate.unrequired,
		"Unclaimed must Unrequire the type")
	require.Empty(t, gate.required)
}

// TestHandleMaterializationEvent_ReleasedDoesNotUnrequire is the key S2 regression (reviewer): a
// Released event is a CHECKPOINT drop, which also fires on a followability wobble (TypeRemoved force-
// release) WHILE THE CLAIM SURVIVES. Such a type must keep being mirrored, so Released must NOT touch the
// gate. The gate flag moves only with the claim (Unclaimed). Before S2, Released Unrequired — silently
// stopping mirroring of a still-claimed type through a wobble.
func TestHandleMaterializationEvent_ReleasedDoesNotUnrequire(t *testing.T) {
	gate := &recordingGate{}
	mirror := &recordingObjectMirror{}
	m := &Manager{Log: logr.Discard(), MirrorGate: gate, ObjectMirror: mirror}

	m.handleMaterializationEvent(context.Background(), logr.Discard(),
		typeset.MaterializationEvent{Kind: typeset.Released, GVR: configMapGVR})

	require.Empty(t, gate.unrequired,
		"Released (a checkpoint drop, possibly a wobble with the claim surviving) must NOT Unrequire")
	require.Equal(t, "/configmaps", mirror.deletedKey, "Released still clears the checkpoint keyspace")
}

// TestDeclareBackfillRetry_FailedBackfillIsReDeclared proves Rec 2 / Gap 2: when a Declare-time
// initial backfill fails, forgetDeclaredGVR un-records the type so the NEXT Declare re-classifies it
// as newly-declared and re-attempts the backfill — instead of recording it as done up front and
// leaving a permanent per-target hole.
func TestDeclareBackfillRetry_FailedBackfillIsReDeclared(t *testing.T) {
	m := &Manager{Log: logr.Discard()}
	gitDest := gitDestRef("retry-target")
	ref := typeset.GitTargetRef(gitDest.String())

	// The type is claimed and Synced, so a Declare classifies it as a newly-declared already-Synced
	// type that needs an initial backfill.
	mat := m.materializerInstance()
	mat.Declare(ref, []schema.GroupVersionResource{configMapGVR})
	mat.OnLifecycleEvent(activate(configMapGVR))
	require.True(t, mat.BeginSync(configMapGVR))
	mat.SyncSucceeded(configMapGVR, "10")

	claimed := []schema.GroupVersionResource{configMapGVR}
	require.Equal(t, claimed, m.newlyDeclaredSyncedGVRs(gitDest, claimed),
		"the first Declare classifies the Synced type as needing a backfill")

	// Its backfill failed: un-record it — exactly what DeclareForGitTarget does on an
	// EmitTypeReconcileForGitDest error.
	m.forgetDeclaredGVR(gitDest, configMapGVR)

	// The next Declare re-classifies it as newly-declared, so the backfill is retried.
	require.Equal(t, claimed, m.newlyDeclaredSyncedGVRs(gitDest, claimed),
		"a failed backfill is retried on the next reconcile, not recorded as done")

	// A backfill that SUCCEEDS (no forget) records the type, so it is not re-driven.
	require.Empty(t, m.newlyDeclaredSyncedGVRs(gitDest, claimed),
		"a succeeded backfill is recorded and not re-driven")
}

// TestDistinctClaimGVRs_CollapsesScopes proves the claim keys on (ref, GVR) only, so the
// resolved (GVR, namespace-scope) stream set collapses to its distinct GVRs in resolver order.
func TestDistinctClaimGVRs_CollapsesScopes(t *testing.T) {
	deployGVR := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	out := distinctClaimGVRs([]snapshotGVR{
		{gvr: deployGVR, namespaces: []string{"ns-a"}},
		{gvr: deployGVR, namespaces: []string{"ns-b"}}, // same GVR, different scope -> collapses
		{gvr: configMapGVR},
	})
	require.Equal(t, []schema.GroupVersionResource{deployGVR, configMapGVR}, out)
}

// TestStartMaterializationSweep_AgesOutUnrenewedLease proves the periodic sweep ticker
// actually drives Sweep on its (injected fast) interval: an unrenewed claim is GC'd once its
// renewal predates the previous tick (DEC-L5), and the goroutine stops on context cancel.
func TestStartMaterializationSweep_AgesOutUnrenewedLease(t *testing.T) {
	manager := &Manager{Log: logr.Discard(), materializationSweepIntervalOverride: 5 * time.Millisecond}
	ref := typeset.GitTargetRef("test-ns/lapsed-target")
	manager.materializerInstance().Declare(ref, []schema.GroupVersionResource{configMapGVR})
	require.NotEmpty(t, manager.materializerInstance().Claimants(configMapGVR))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager.startMaterializationSweep(ctx, logr.Discard())

	require.Eventually(t, func() bool {
		return len(manager.materializerInstance().Claimants(configMapGVR)) == 0
	}, 2*time.Second, 5*time.Millisecond, "the periodic sweep must age out an unrenewed lease")
}

// TestRunTypeCheckpointSync_ListsClaimedTypeAndMarksSynced proves the L-3 checkpoint driver:
// a claimed + activated type is listed into the checkpoint keyspace and reaches Synced at the
// list revision.
func TestRunTypeCheckpointSync_ListsClaimedTypeAndMarksSynced(t *testing.T) {
	dc := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), streamedCM("default", "a", "10"))
	mirror := &recordingObjectMirror{}
	m := &Manager{Log: logr.Discard(), dynamicClient: dc, ObjectMirror: mirror}
	m.watchCheckpointObjects = streamSeam(streamedCM("default", "a", "10"))

	// Claim + activate: the Materializer moves the type to Requested (demand ∩ followable).
	m.materializerInstance().Declare(typeset.GitTargetRef("test-ns/t"), []schema.GroupVersionResource{configMapGVR})
	m.materializerInstance().OnLifecycleEvent(activate(configMapGVR))

	m.runTypeCheckpointSync(context.Background(), logr.Discard(), configMapGVR)

	assert.Equal(t, "/configmaps", mirror.replacedKey, "a claimed, activated type is listed into the checkpoint")
	ph, _ := m.materializerInstance().Phase(configMapGVR)
	assert.Equal(t, typeset.PhaseSynced, ph)
	rv, ok := m.materializerInstance().Checkpoint(configMapGVR)
	assert.True(t, ok)
	assert.NotEmpty(t, rv, "the checkpoint is pinned to the list revision")
}

// TestRunTypeCheckpointSync_TrimsAuditLogOnReAnchor proves R1's trim half: a successful
// checkpoint sync trims the type's audit log to the just-pinned cursor (the §6 trim-cursor model),
// and a nil/failed trimmer never disturbs the Synced outcome.
func TestRunTypeCheckpointSync_TrimsAuditLogOnReAnchor(t *testing.T) {
	dc := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), streamedCM("default", "a", "10"))
	mirror := &recordingObjectMirror{}
	trimmer := &recordingTrimmer{}
	m := &Manager{Log: logr.Discard(), dynamicClient: dc, ObjectMirror: mirror, AuditLogTrimmer: trimmer}
	m.watchCheckpointObjects = streamSeam(streamedCM("default", "a", "10"))

	m.materializerInstance().Declare(typeset.GitTargetRef("test-ns/t"), []schema.GroupVersionResource{configMapGVR})
	m.materializerInstance().OnLifecycleEvent(activate(configMapGVR))
	m.runTypeCheckpointSync(context.Background(), logr.Discard(), configMapGVR)

	rv, _ := m.materializerInstance().Checkpoint(configMapGVR)
	assert.Equal(t, 1, trimmer.trimCount, "a successful re-anchor trims the log exactly once")
	assert.Equal(t, "/configmaps", trimmer.trimmedKey, "trims the type's own audit stream")
	assert.Equal(t, rv, trimmer.trimmedRV, "trims to the just-pinned checkpoint cursor")
}

// TestRunTypeCheckpointSync_TrimFailureIsBenign proves a trim error is swallowed: the type stays
// Synced at its new revision (the splice still replays correctly against an over-long log).
func TestRunTypeCheckpointSync_TrimFailureIsBenign(t *testing.T) {
	dc := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), streamedCM("default", "a", "10"))
	m := &Manager{
		Log: logr.Discard(), dynamicClient: dc,
		ObjectMirror:    &recordingObjectMirror{},
		AuditLogTrimmer: &recordingTrimmer{err: errors.New("trim boom")},
	}
	m.watchCheckpointObjects = streamSeam(streamedCM("default", "a", "10"))

	m.materializerInstance().Declare(typeset.GitTargetRef("test-ns/t"), []schema.GroupVersionResource{configMapGVR})
	m.materializerInstance().OnLifecycleEvent(activate(configMapGVR))
	m.runTypeCheckpointSync(context.Background(), logr.Discard(), configMapGVR)

	ph, _ := m.materializerInstance().Phase(configMapGVR)
	assert.Equal(t, typeset.PhaseSynced, ph, "a trim failure never unsettles the checkpoint")
}

// TestRunTypeCheckpointSync_SkipsUnclaimedFollowableType is the L-3 gate: a followable type
// with no claim never lists — BeginSync is a no-op because the Materializer left it Dormant.
func TestRunTypeCheckpointSync_SkipsUnclaimedFollowableType(t *testing.T) {
	dc := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), streamedCM("default", "a", "10"))
	mirror := &recordingObjectMirror{}
	m := &Manager{Log: logr.Discard(), dynamicClient: dc, ObjectMirror: mirror}

	m.materializerInstance().OnLifecycleEvent(activate(configMapGVR)) // activated, but unclaimed
	m.runTypeCheckpointSync(context.Background(), logr.Discard(), configMapGVR)

	assert.Empty(t, mirror.replacedKey, "an unclaimed followable type holds no checkpoint")
	ph, _ := m.materializerInstance().Phase(configMapGVR)
	assert.Equal(t, typeset.PhaseDormant, ph)
}

// TestRunTypeCheckpointSync_FailedListMarksFailing proves a checkpoint LIST error lands the
// type in Failing with no checkpoint served (a first-sync failure → consumers hold).
func TestRunTypeCheckpointSync_FailedListMarksFailing(t *testing.T) {
	// Register an object so the fake knows the list kind, then force List to error.
	dc := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), streamedCM("default", "a", "10"))
	dc.PrependReactor("list", "*", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("boom")
	})
	mirror := &recordingObjectMirror{}
	m := &Manager{Log: logr.Discard(), dynamicClient: dc, ObjectMirror: mirror}
	// The watch rejects streaming-list, so the fill falls back to the (failing) LIST.
	m.watchCheckpointObjects = rejectWatchSeam()

	m.materializerInstance().Declare(typeset.GitTargetRef("test-ns/t"), []schema.GroupVersionResource{configMapGVR})
	m.materializerInstance().OnLifecycleEvent(activate(configMapGVR))

	m.runTypeCheckpointSync(context.Background(), logr.Discard(), configMapGVR)

	ph, _ := m.materializerInstance().Phase(configMapGVR)
	assert.Equal(t, typeset.PhaseFailing, ph, "a failed checkpoint list lands in Failing")
	_, ok := m.materializerInstance().Checkpoint(configMapGVR)
	assert.False(t, ok, "a first-sync failure serves no checkpoint")
	assert.Empty(t, mirror.replacedKey, "nothing is written on a failed list")
}

// TestHandleMaterializationEvent_ReleasedClearsCheckpoint proves a Released event drops the
// type's checkpoint keyspace (demand GC or followability loss).
func TestHandleMaterializationEvent_ReleasedClearsCheckpoint(t *testing.T) {
	mirror := &recordingObjectMirror{}
	m := &Manager{Log: logr.Discard(), ObjectMirror: mirror}

	m.handleMaterializationEvent(context.Background(), logr.Discard(),
		typeset.MaterializationEvent{Kind: typeset.Released, GVR: configMapGVR})

	assert.Equal(t, "/configmaps", mirror.deletedKey, "Released drops the type's checkpoint")
}

// TestHandleMaterializationEvent_ReAnchorReFansAsDeferredHeal is the 8f2ad84 regression: a
// TypeSynced that arrives AFTER the per-type audit tail is running (a periodic sweep re-anchor or a
// late-event nudge) must NOT be skipped — it must re-fan the per-type reconcile so refreshed
// checkpoint state (orphans, a deletecollection, a late-lane event) reaches git. Before the fix the
// re-splice was disabled outright to avoid stealing an open commit window; now it is re-enabled and
// routed as a HEAL (heal=true), which the worker defers until the window is idle instead of stealing
// it. The first TypeSynced (tail not yet running) is an initial backfill (heal=false).
func TestHandleMaterializationEvent_ReAnchorReFansAsDeferredHeal(t *testing.T) {
	m := &Manager{Log: logr.Discard()}
	var gotHeal []bool
	var gotGVR []schema.GroupVersionResource
	m.reconcileTypeFanOverride = func(
		_ context.Context, _ logr.Logger, gvr schema.GroupVersionResource, heal bool,
	) {
		gotGVR = append(gotGVR, gvr)
		gotHeal = append(gotHeal, heal)
	}

	// First TypeSynced: the tail is not yet running, so this is the initial backfill.
	m.handleMaterializationEvent(context.Background(), logr.Discard(),
		typeset.MaterializationEvent{Kind: typeset.TypeSynced, GVR: configMapGVR, RV: "10"})
	require.Equal(t, []bool{false}, gotHeal, "the first TypeSynced fans an initial backfill, not a heal")

	// The first sync has started the per-type audit tail; simulate it being live.
	m.auditTailsMu.Lock()
	if m.auditTails == nil {
		m.auditTails = map[schema.GroupVersionResource]context.CancelFunc{}
	}
	m.auditTails[configMapGVR] = func() {}
	m.auditTailsMu.Unlock()

	// A later TypeSynced (re-anchor) must re-fan — and as a deferred heal, not be skipped.
	m.handleMaterializationEvent(context.Background(), logr.Discard(),
		typeset.MaterializationEvent{Kind: typeset.TypeSynced, GVR: configMapGVR, RV: "20"})
	require.Equal(t, []bool{false, true}, gotHeal,
		"a re-anchor while the tail runs re-fans the reconcile as a heal (8f2ad84 regression), not a skip")
	require.Equal(t, []schema.GroupVersionResource{configMapGVR, configMapGVR}, gotGVR)
}

// syncedViaDriver brings a freshly-claimed type to Synced through the real driver path
// (Declare → activate → runTypeCheckpointSync), the common L-4 precondition.
func syncedViaDriver(t *testing.T, m *Manager, gvr schema.GroupVersionResource) {
	t.Helper()
	mat := m.materializerInstance()
	mat.Declare(typeset.GitTargetRef("test-ns/t"), []schema.GroupVersionResource{gvr})
	mat.OnLifecycleEvent(activate(gvr))
	m.runTypeCheckpointSync(context.Background(), logr.Discard(), gvr)
	if ph, _ := mat.Phase(gvr); ph != typeset.PhaseSynced {
		t.Fatalf("setup: phase = %q, want Synced", ph)
	}
}

// TestMaterializationSweep_ReAnchorsClaimedSyncedType proves the L-4 re-anchor: a sweep flags a
// still-claimed Synced type for a periodic re-anchor (SyncRequested), and driving it re-fills the
// checkpoint (Synced → Resyncing → Synced).
func TestMaterializationSweep_ReAnchorsClaimedSyncedType(t *testing.T) {
	dc := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), streamedCM("default", "a", "10"))
	mirror := &recordingObjectMirror{}
	m := &Manager{Log: logr.Discard(), dynamicClient: dc, ObjectMirror: mirror}
	m.watchCheckpointObjects = streamSeam(streamedCM("default", "a", "10"))
	syncedViaDriver(t, m, configMapGVR)
	require.Equal(t, 1, mirror.replaceCount)

	// The claim is still live, so the sweep flags a periodic re-anchor.
	m.materializerInstance().Sweep()
	require.Contains(t, m.materializerInstance().PendingSyncs(), configMapGVR,
		"a still-claimed Synced type is flagged for re-anchor")

	// Driving the flagged re-anchor re-lists the type and lands back on Synced.
	m.runTypeCheckpointSync(context.Background(), logr.Discard(), configMapGVR)
	require.Equal(t, 2, mirror.replaceCount, "the re-anchor re-fills the checkpoint")
	ph, _ := m.materializerInstance().Phase(configMapGVR)
	require.Equal(t, typeset.PhaseSynced, ph)
}

// TestMaterializationSweep_ReleasesAndClearsWhenDemandStops proves the L-4 release end-to-end:
// once demand stops, a sweep releases the type and the driver drops its checkpoint. The emitted
// events are drained synchronously here as a deterministic stand-in for the driver goroutine.
func TestMaterializationSweep_ReleasesAndClearsWhenDemandStops(t *testing.T) {
	dc := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), streamedCM("default", "a", "10"))
	mirror := &recordingObjectMirror{}
	m := &Manager{Log: logr.Discard(), dynamicClient: dc, ObjectMirror: mirror}
	m.watchCheckpointObjects = streamSeam(streamedCM("default", "a", "10"))
	mat := m.materializerInstance()

	var swept []typeset.MaterializationEvent
	mat.Subscribe(func(ev typeset.MaterializationEvent) { swept = append(swept, ev) })
	syncedViaDriver(t, m, configMapGVR)
	swept = nil // drop setup events; keep only what the sweeps emit

	// Demand stops (no further Declare). The first sweep still sees the claim live; the second,
	// after the renewal predates the previous sweep, GCs the lease and releases the checkpoint.
	mat.Sweep()
	mat.Sweep()
	for _, ev := range swept {
		m.handleMaterializationEvent(context.Background(), logr.Discard(), ev)
	}

	ph, _ := mat.Phase(configMapGVR)
	require.Equal(t, typeset.PhaseDormant, ph, "demand stopped → released to Dormant at the sweep")
	require.Equal(t, "/configmaps", mirror.deletedKey, "the release drives a checkpoint clear")
}

// TestRestoreSyncedCheckpoint_MarksSyncedWithoutFill proves the L-5 boot rebuild seam: replaying
// a durable checkpoint marks the type Synced at rv with no re-list, and a blank GVR/rv is ignored.
func TestRestoreSyncedCheckpoint_MarksSyncedWithoutFill(t *testing.T) {
	mirror := &recordingObjectMirror{}
	m := &Manager{Log: logr.Discard(), ObjectMirror: mirror}
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

	m.RestoreSyncedCheckpoint("apps", "v1", "deployments", "900")
	ph, ok := m.materializerInstance().Phase(gvr)
	require.True(t, ok)
	require.Equal(t, typeset.PhaseSynced, ph)
	rv, ok := m.materializerInstance().Checkpoint(gvr)
	require.True(t, ok)
	require.Equal(t, "900", rv)
	require.Zero(t, mirror.replaceCount, "a boot restore must not re-list")

	m.RestoreSyncedCheckpoint("", "v1", "configmaps", "") // blank rv → ignored
	_, ok = m.materializerInstance().Phase(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"})
	require.False(t, ok)
}

// TestMaterializationSummaryForGitTarget_RollsUpClaimedPhases proves the L-6 per-GitTarget
// status roll-up: bounded counts over the types this GitTarget claims, including the
// claim-vs-refused mismatch, and an empty summary for a GitTarget that claims nothing.
func TestMaterializationSummaryForGitTarget_RollsUpClaimedPhases(t *testing.T) {
	dc := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), streamedCM("default", "a", "10"))
	mirror := &recordingObjectMirror{}
	m := &Manager{Log: logr.Discard(), dynamicClient: dc, ObjectMirror: mirror}
	m.watchCheckpointObjects = streamSeam(streamedCM("default", "a", "10"))
	mat := m.materializerInstance()
	gitDest := gitDestRef("roll-target")
	ref := typeset.GitTargetRef(gitDest.String())

	// configmaps: claimed + synced. secrets: claimed but never followable (the mismatch).
	mat.Declare(ref, []schema.GroupVersionResource{configMapGVR, secretsGVR})
	mat.OnLifecycleEvent(activate(configMapGVR))
	m.runTypeCheckpointSync(context.Background(), logr.Discard(), configMapGVR)

	sum := m.MaterializationSummaryForGitTarget(gitDest)
	require.Equal(t, 2, sum.Claimed)
	require.Equal(t, 1, sum.Synced)
	require.Equal(t, 1, sum.NotFollowable, "a claimed type that never became followable is a mismatch")
	require.Zero(t, sum.Failing)

	// A GitTarget that claims nothing has an empty roll-up.
	require.Equal(t, GitTargetMaterializationSummary{}, m.MaterializationSummaryForGitTarget(gitDestRef("other")))
}

// TestMaterializationSummary_ServiceabilityDoesNotFlapOnReAnchor is the Gap-6 regression: the
// roll-up buckets on serviceability (a usable checkpoint), not on phase == Synced, so a periodic
// re-anchor (Synced → Resyncing → Synced) keeps the type counted as Synced and Pending at zero
// throughout. Before the fix a Resyncing type was bucketed as Pending, flapping every liveness
// signal built on the roll-up on every ~1h re-anchor.
func TestMaterializationSummary_ServiceabilityDoesNotFlapOnReAnchor(t *testing.T) {
	m := &Manager{Log: logr.Discard()}
	gitDest := gitDestRef("svc-target")
	ref := typeset.GitTargetRef(gitDest.String())
	mat := m.materializerInstance()

	mat.Declare(ref, []schema.GroupVersionResource{configMapGVR})
	mat.OnLifecycleEvent(activate(configMapGVR))
	require.True(t, mat.BeginSync(configMapGVR)) // Requested → Syncing
	mat.SyncSucceeded(configMapGVR, "10")        // Syncing → Synced, serves rv 10

	atSynced := m.MaterializationSummaryForGitTarget(gitDest)
	require.Equal(t, 1, atSynced.Synced)
	require.Zero(t, atSynced.Pending)

	// Periodic re-anchor begins: still serves the prior checkpoint, so still serviceable.
	require.True(t, mat.RequestResync(configMapGVR)) // pending re-anchor + SyncRequested
	require.True(t, mat.BeginSync(configMapGVR))     // Synced(pending) → Resyncing
	atResyncing := m.MaterializationSummaryForGitTarget(gitDest)
	require.Equal(t, 1, atResyncing.Synced, "a Resyncing type still serves its prior checkpoint")
	require.Zero(t, atResyncing.Pending, "a re-anchor must not read as not-yet-serviceable")

	mat.SyncSucceeded(configMapGVR, "20") // Resyncing → Synced at the refreshed rv
	atReSynced := m.MaterializationSummaryForGitTarget(gitDest)
	require.Equal(t, 1, atReSynced.Synced)
	require.Zero(t, atReSynced.Pending)
}

// TestMaterializationSummary_FailingWithPriorCheckpointStaysServiceable proves a re-anchor that
// errors keeps serving its prior checkpoint: the type is counted as Synced (serviceable) AND
// Failing (the operator stall signal), but not as a degraded first-sync stall.
func TestMaterializationSummary_FailingWithPriorCheckpointStaysServiceable(t *testing.T) {
	m := &Manager{Log: logr.Discard()}
	gitDest := gitDestRef("fail-after-synced")
	ref := typeset.GitTargetRef(gitDest.String())
	mat := m.materializerInstance()

	mat.Declare(ref, []schema.GroupVersionResource{configMapGVR})
	mat.OnLifecycleEvent(activate(configMapGVR))
	require.True(t, mat.BeginSync(configMapGVR))
	mat.SyncSucceeded(configMapGVR, "10")
	require.True(t, mat.RequestResync(configMapGVR))
	require.True(t, mat.BeginSync(configMapGVR)) // → Resyncing
	mat.SyncFailed(configMapGVR)                 // → Failing, prior checkpoint retained

	sum := m.MaterializationSummaryForGitTarget(gitDest)
	assert.Equal(t, 1, sum.Synced, "still serves its prior checkpoint")
	assert.Equal(t, 1, sum.Failing)
	assert.Zero(t, sum.FailingNoCheckpoint, "it has a checkpoint, so not a degraded first-sync stall")
	assert.Zero(t, sum.Pending)
}

// TestMaterializationSummary_FailingWithoutCheckpointIsDegradedSignal proves a first-sync failure
// (no prior checkpoint to serve) is counted as Failing AND FailingNoCheckpoint and NOT as Synced —
// the signal the controller turns into phase=Degraded.
func TestMaterializationSummary_FailingWithoutCheckpointIsDegradedSignal(t *testing.T) {
	m := &Manager{Log: logr.Discard()}
	gitDest := gitDestRef("fail-first-sync")
	ref := typeset.GitTargetRef(gitDest.String())
	mat := m.materializerInstance()

	mat.Declare(ref, []schema.GroupVersionResource{configMapGVR})
	mat.OnLifecycleEvent(activate(configMapGVR))
	require.True(t, mat.BeginSync(configMapGVR)) // → Syncing
	mat.SyncFailed(configMapGVR)                 // → Failing, never landed a checkpoint

	sum := m.MaterializationSummaryForGitTarget(gitDest)
	assert.Zero(t, sum.Synced)
	assert.Equal(t, 1, sum.Failing)
	assert.Equal(t, 1, sum.FailingNoCheckpoint)
	assert.Zero(t, sum.Pending)
}
