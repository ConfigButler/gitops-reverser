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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"

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
		clusterRuleForResource("rule-cm", "test-target", "configmaps"),
		"test-target", "test-ns", "test-provider", "test-ns", "main", "test-path",
	)
	store.AddOrUpdateClusterWatchRule(
		clusterRuleForResource("rule-sec", "test-target", "secrets"),
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
		clusterRuleForResource("rule-cm", "test-target", "configmaps"),
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
