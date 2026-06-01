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

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/reconcile"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// These tests guard against an issue #146-style regression: when a new
// ClusterWatchRule lands but its referenced GVR is *already* being watched
// (e.g. because another rule already covers that GVR), ReconcileForRuleChange
// must still emit a snapshot. An earlier version took a GVR-only-diff
// early-return and skipped the snapshot, so resources that already matched the
// new rule stayed out of git until they were next touched.
//
// Each test asserts the contract directly: the snapshot count goes up when the
// rule-set changes, regardless of whether the GVR set changed.

// makeRuleChangeTestManager constructs a single-GitTarget Manager for these
// tests: fake controller-runtime client, real RuleStore, the common catalog
// (which serves configmaps and secrets, so configmaps rules resolve to a real
// GVR and the target's effective watch plan is non-empty), and a target
// GitTarget already in the fake client.
//
// The configmaps GVR is pre-populated in activeInformers (cluster-wide) so
// compareGVRs reports added=0, removed=0 for it: the informer is logically
// already running, and no real informer is started. This isolates the snapshot
// *decision* (driven by the per-target effective-watch-plan hash) from informer
// lifecycle.
func makeRuleChangeTestManager(t *testing.T) (*Manager, *rulestore.RuleStore) {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, configv1alpha1.AddToScheme(scheme))

	gitTarget := &configv1alpha1.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "test-target", Namespace: "test-ns"},
		Spec: configv1alpha1.GitTargetSpec{
			ProviderRef: configv1alpha1.GitProviderReference{Name: "test-provider"},
			Branch:      "main",
			Path:        "test-path",
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gitTarget).
		Build()

	store := rulestore.NewStore()
	manager := &Manager{
		Client:          fakeClient,
		Log:             logr.Discard(),
		RuleStore:       store,
		resourceCatalog: newCommonTestCatalog(t),
		discoveryClient: commonTestDiscoveryClient(),
		// Pretend the configmaps informer is already running cluster-wide,
		// so compareGVRs reports added=0, removed=0 for that GVR. No real
		// informers are started.
		activeInformers: map[GVR]map[string]context.CancelFunc{
			namespacedGVR("configmaps"): {"": func() {}},
		},
	}

	return manager, store
}

// clusterRuleForResource builds a ClusterWatchRule that selects a single core/v1
// namespaced resource, scoped at the given GitTarget (always in the shared
// "test-ns" namespace used across these tests).
func clusterRuleForResource(name, gitTargetName, resource string) configv1alpha1.ClusterWatchRule {
	return configv1alpha1.ClusterWatchRule{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: configv1alpha1.ClusterWatchRuleSpec{
			TargetRef: configv1alpha1.NamespacedTargetReference{
				Name:      gitTargetName,
				Namespace: "test-ns",
			},
			Rules: []configv1alpha1.ClusterResourceRule{{
				APIGroups:   []string{""},
				APIVersions: []string{"v1"},
				Resources:   []string{resource},
				Scope:       configv1alpha1.ResourceScopeNamespaced,
			}},
		},
	}
}

// configMapRuleForTarget builds a configmaps ClusterWatchRule for the target.
func configMapRuleForTarget(name, gitTargetName string) configv1alpha1.ClusterWatchRule {
	return clusterRuleForResource(name, gitTargetName, "configmaps")
}

// TestReconcileForRuleChange_TargetGainsAlreadyWatchedGVR_Snapshots is the
// issue #146 case under the per-target plan model. A GVR can already be watched
// globally (because another target's rule covers it) while a particular target
// has never received it. When that target gains a rule for the GVR, no informer
// churn happens (added=0, removed=0 for that GVR), yet the target's effective
// watch plan grows — so it must still snapshot to backfill the already-present
// resources into its repo.
//
// Here target-a watches configmaps and target-b watches secrets. configmaps is
// already active. target-b then also starts watching configmaps: target-b's
// plan grows and it must be selected, even though no new configmaps informer is
// needed.
func TestReconcileForRuleChange_TargetGainsAlreadyWatchedGVR_Snapshots(t *testing.T) {
	manager, store := makeTwoTargetRuleChangeManager(t)

	store.AddOrUpdateClusterWatchRule(
		clusterRuleForResource("rule-a", "target-a", "configmaps"),
		"target-a", "test-ns", "test-provider", "test-ns", "main", "path-a",
	)
	store.AddOrUpdateClusterWatchRule(
		clusterRuleForResource("rule-b", "target-b", "secrets"),
		"target-b", "test-ns", "test-provider", "test-ns", "main", "path-b",
	)
	seedDeliveredBaseline(manager)

	// target-b now also watches configmaps — already watched by target-a, so no
	// new informer, but target-b's plan grows.
	store.AddOrUpdateClusterWatchRule(
		clusterRuleForResource("rule-b-configmaps", "target-b", "configmaps"),
		"target-b", "test-ns", "test-provider", "test-ns", "main", "path-b",
	)

	require.NoError(t, manager.ReconcileForRuleChange(ctx()))

	assert.True(t, targetPending(manager, "target-b"),
		"target-b gained coverage of an already-watched GVR; it must snapshot to backfill "+
			"those resources even though no informer churn occurred (issue #146)")
	assert.False(t, targetPending(manager, "target-a"),
		"target-a's plan is unchanged and must not snapshot")
}

// TestReconcileForRuleChange_NarrowingOperationsOnExistingRule_Snapshots covers
// editing a rule's operations without changing its GVR. The resolved GVR is
// unchanged, but operations are part of the effective watch plan, so the plan
// hash changes and the target must snapshot.
func TestReconcileForRuleChange_NarrowingOperationsOnExistingRule_Snapshots(t *testing.T) {
	manager, store := makeRuleChangeTestManager(t)
	ctx := context.Background()

	// Initial rule: all operations on configmaps.
	initial := configMapRuleForTarget("rule-1", "test-target")
	store.AddOrUpdateClusterWatchRule(
		initial,
		"test-target", "test-ns",
		"test-provider", "test-ns",
		"main", "test-path",
	)
	require.NoError(t, manager.ReconcileForRuleChange(ctx))

	manager.snapshotEmitCount.Store(0)

	// Narrow operations to UPDATE only. The GVR (core/v1/configmaps) is
	// unchanged; only the operation set in the plan changes.
	narrowed := configMapRuleForTarget("rule-1", "test-target")
	narrowed.Spec.Rules[0].Operations = []configv1alpha1.OperationType{
		configv1alpha1.OperationUpdate,
	}
	store.AddOrUpdateClusterWatchRule(
		narrowed,
		"test-target", "test-ns",
		"test-provider", "test-ns",
		"main", "test-path",
	)
	require.NoError(t, manager.ReconcileForRuleChange(ctx))

	assert.Positive(t,
		manager.SnapshotEmitCount(),
		"narrowing a rule's operations changes the effective watch plan and must emit a snapshot, "+
			"even though the resolved GVR set is unchanged")
}

// watchRuleForTarget builds a namespaced WatchRule selecting core/v1
// configmaps in the target's namespace.
func watchRuleForTarget(name, gitTargetName, namespace string) configv1alpha1.WatchRule {
	return configv1alpha1.WatchRule{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: configv1alpha1.WatchRuleSpec{
			TargetRef: configv1alpha1.LocalTargetReference{Name: gitTargetName},
			Rules: []configv1alpha1.ResourceRule{{
				APIGroups:   []string{""},
				APIVersions: []string{"v1"},
				Resources:   []string{"configmaps"},
			}},
		},
	}
}

// TestCurrentRuleSetSnapshots_NamespacedWatchRulePlanByNamespace exercises the
// namespace dimension of the effective watch plan for namespaced WatchRules,
// directly at the hash level (no informer lifecycle). A WatchRule watches its
// own namespace, so the same GVR in a different namespace is a different watch
// surface and must change the target's plan hash — while a redundant duplicate
// WatchRule in an already-watched namespace must not.
func TestCurrentRuleSetSnapshots_NamespacedWatchRulePlanByNamespace(t *testing.T) {
	manager, store := makeRuleChangeTestManager(t)

	hashForTarget := func() uint64 {
		for _, target := range manager.currentRuleSetSnapshots() {
			if target.gitDest.Name == "test-target" {
				return target.hash
			}
		}
		t.Fatal("test-target not present in rule-set snapshots")
		return 0
	}

	// WatchRule watching configmaps in ns-a.
	store.AddOrUpdateWatchRule(
		watchRuleForTarget("rule-a", "test-target", "ns-a"),
		"test-target", "test-ns", "test-provider", "test-ns", "main", "test-path",
	)
	hashA := hashForTarget()

	// Second WatchRule in a different namespace (ns-b): the plan grows.
	store.AddOrUpdateWatchRule(
		watchRuleForTarget("rule-b", "test-target", "ns-b"),
		"test-target", "test-ns", "test-provider", "test-ns", "main", "test-path",
	)
	hashAB := hashForTarget()
	assert.NotEqual(t, hashA, hashAB,
		"a WatchRule adding the same GVR in a new namespace expands the watch surface and must change the plan hash")

	// Redundant duplicate WatchRule in an already-watched namespace (ns-a).
	store.AddOrUpdateWatchRule(
		watchRuleForTarget("rule-a-dup", "test-target", "ns-a"),
		"test-target", "test-ns", "test-provider", "test-ns", "main", "test-path",
	)
	assert.Equal(t, hashAB, hashForTarget(),
		"a redundant duplicate WatchRule in an already-watched namespace must not change the plan hash")
}

// TestReconcileForRuleChange_RestartLikeBootstrap_NoSnapshotDrops models the
// "restart with already-Ready GitTarget" scenario. The user-visible symptom:
//
//   - On controller restart, GitTarget.Status.SnapshotSynced is already True
//     from the prior run, so GitTargetReconciler takes the early-return at
//     gittarget_controller.go:384 and creates no FolderReconciler.
//   - WatchManager.Start runs its initial ReconcileForRuleChange before the
//     GitTargetReconciler reaches the target, so the snapshot is emitted
//     while no reconciler is registered. RouteClusterStateEvent / RouteRepoStateEvent
//     find no receiver and silently drop the snapshot output.
//   - Live update events still flow through (informer is running, the eventual
//     stream registration brings it to LiveProcessing), so the user sees
//     "updates write, but pre-existing resources never land in git."
//
// This test guards the fix: it emits a snapshot with no pre-registered
// FolderReconciler and asserts the drop counter stays at zero, so snapshot
// emission stays coordinated with reconciler registration.
//
// The setup populates activeInformers with a stale GVR and uses a catalog that
// does not serve configmaps, so the rule's GVR resolves to nothing and
// compareGVRs reports added=[], removed=[stale-gvr]. That bypasses the
// early-return without requiring any real informers to start.
func TestReconcileForRuleChange_RestartLikeBootstrap_NoSnapshotDrops(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, configv1alpha1.AddToScheme(scheme))

	gitTarget := &configv1alpha1.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "test-target", Namespace: "test-ns"},
		Spec: configv1alpha1.GitTargetSpec{
			ProviderRef: configv1alpha1.GitProviderReference{Name: "test-provider"},
			Branch:      "main",
			Path:        "test-path",
		},
		Status: configv1alpha1.GitTargetStatus{
			Conditions: []metav1.Condition{
				// Simulate the post-restart state that triggers the
				// gittarget_controller.go:384 early-return.
				{Type: "SnapshotSynced", Status: metav1.ConditionTrue, Reason: "Completed"},
			},
		},
	}
	preexistingCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "preexisting", Namespace: "test-ns"},
	}

	fakeK8s := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gitTarget).
		Build()

	store := rulestore.NewStore()
	store.AddOrUpdateClusterWatchRule(
		configMapRuleForTarget("rule-1", "test-target"),
		"test-target", "test-ns",
		"test-provider", "test-ns",
		"main", "test-path",
	)

	staleGVR := GVR{
		Group: "apps", Version: "v1", Resource: "deployments",
		Scope: configv1alpha1.ResourceScopeNamespaced,
	}
	manager := &Manager{
		Client:        fakeK8s,
		Log:           logr.Discard(),
		RuleStore:     store,
		dynamicClient: dynamicfake.NewSimpleDynamicClient(scheme, preexistingCM),
		// The common catalog serves configmaps, so the rule resolves to a real
		// GVR and the target's effective watch plan is non-empty (the target is
		// selected for a snapshot). configmaps is pre-activated so no informer is
		// started; the stale GVR is removed so compareGVRs avoids the
		// no-change early-return.
		resourceCatalog: newCommonTestCatalog(t),
		discoveryClient: commonTestDiscoveryClient(),
		activeInformers: map[GVR]map[string]context.CancelFunc{
			namespacedGVR("configmaps"): {"": func() {}},
			staleGVR:                    {"": func() {}},
		},
	}

	// Wire EventRouter with empty-but-non-nil sub-components. This is the
	// state immediately after WatchManager.Start: workers and reconcilers
	// have not yet been registered by their respective controllers.
	manager.EventRouter = &EventRouter{
		WorkerManager:     git.NewWorkerManager(fakeK8s, logr.Discard(), 0, types.SensitiveResourcePolicy{}),
		ReconcilerManager: reconcile.NewReconcilerManager(nil, logr.Discard()),
		WatchManager:      manager,
		Client:            fakeK8s,
		Log:               logr.Discard(),
		gitTargetStreams:  map[string]*reconcile.GitTargetEventStream{},
	}

	require.NoError(t, manager.ReconcileForRuleChange(ctx()))

	assert.Zero(t,
		manager.EventRouter.SnapshotDeliveryDrops(),
		"on a restart-like bootstrap (no FolderReconciler registered yet), the snapshot is emitted "+
			"but RouteClusterStateEvent/RouteRepoStateEvent silently drop it; the system must coordinate "+
			"emission with reconciler registration, or retry, so pre-existing matching resources land in git")
}

// makeTwoTargetRuleChangeManager builds a Manager with two GitTargets,
// "target-a" and "target-b", both in "test-ns".
//
// The common catalog serves configmaps and secrets, so rules for them resolve
// to real GVRs and each target's effective watch plan is non-empty. Those
// resolved GVRs are pre-populated in activeInformers, so compareGVRs reports
// them as already watched (added=0) and no real informer is started. A stale
// apps/v1 deployments GVR that no rule wants is also pre-populated: the next
// ReconcileForRuleChange drops it (removed>0), modelling unrelated *global*
// informer churn happening concurrently — again without starting any informer.
//
// EventRouter is left nil. snapshotTargetsNeedingDelivery records every selected
// target in pendingRuleSetHash *before* emission, and with no EventRouter the
// emit path is a no-op that never marks targets delivered, so pendingRuleSetHash
// is a faithful record of which targets were selected for a rule-change snapshot.
func makeTwoTargetRuleChangeManager(t *testing.T) (*Manager, *rulestore.RuleStore) {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, configv1alpha1.AddToScheme(scheme))

	targetA := &configv1alpha1.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "target-a", Namespace: "test-ns"},
		Spec: configv1alpha1.GitTargetSpec{
			ProviderRef: configv1alpha1.GitProviderReference{Name: "test-provider"},
			Branch:      "main",
			Path:        "path-a",
		},
	}
	targetB := &configv1alpha1.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "target-b", Namespace: "test-ns"},
		Spec: configv1alpha1.GitTargetSpec{
			ProviderRef: configv1alpha1.GitProviderReference{Name: "test-provider"},
			Branch:      "main",
			Path:        "path-b",
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(targetA, targetB).
		Build()

	staleGVR := GVR{
		Group: "apps", Version: "v1", Resource: "deployments",
		Scope: configv1alpha1.ResourceScopeNamespaced,
	}
	manager := &Manager{
		Client:          fakeClient,
		Log:             logr.Discard(),
		RuleStore:       rulestore.NewStore(),
		resourceCatalog: newCommonTestCatalog(t),
		discoveryClient: commonTestDiscoveryClient(),
		activeInformers: map[GVR]map[string]context.CancelFunc{
			namespacedGVR("configmaps"): {"": func() {}},
			namespacedGVR("secrets"):    {"": func() {}},
			staleGVR:                    {"": func() {}},
		},
	}

	return manager, manager.RuleStore
}

// namespacedGVR is a terse constructor for a core/v1 namespaced GVR.
func namespacedGVR(resource string) GVR {
	return GVR{Group: "", Version: "v1", Resource: resource, Scope: configv1alpha1.ResourceScopeNamespaced}
}

// seedDeliveredBaseline registers the current rule set's hashes as already
// delivered, modelling the steady state where every target's snapshot is up to
// date. After this, only a target whose hash *changes* should be re-selected.
func seedDeliveredBaseline(m *Manager) {
	for _, target := range m.currentRuleSetSnapshots() {
		m.markRuleSetSnapshotDelivered(target)
	}
}

// targetPending reports whether the given GitTarget (in the shared "test-ns"
// namespace these tests use) was selected for a rule-change snapshot on the last
// reconcile.
func targetPending(m *Manager, name string) bool {
	key := types.NewResourceReference(name, "test-ns").Key()
	m.ruleSetSnapshotMu.Lock()
	defer m.ruleSetSnapshotMu.Unlock()
	_, ok := m.pendingRuleSetHash[key]
	return ok
}

// TestReconcileForRuleChange_UnrelatedTargetNotSnapshotted_OnGlobalGVRChurn is
// the GitTarget-isolation invariant from
// docs/design/gittarget-isolation-on-rule-change.md.
//
// Two GitTargets, A and B, both watch configmaps and are in steady state (their
// snapshots already delivered). Then *only* target B's effective watch plan
// changes — it starts watching secrets too — while at the same moment unrelated
// global informer churn occurs (a stale GVR is dropped). Target A's plan is
// untouched.
//
// Expected: only B is selected for a rule-change snapshot; A keeps processing
// live events and is not dragged into RECONCILING.
//
// This is the regression the design doc set out to remove: previously
// ReconcileForRuleChange passed `len(added) > 0 || len(removed) > 0` as a global
// `force` flag that bypassed the per-target hash check and selected *every*
// target, so unrelated churn dragged A into a snapshot too. Snapshot selection
// is now purely per-target, so this passes.
func TestReconcileForRuleChange_UnrelatedTargetNotSnapshotted_OnGlobalGVRChurn(t *testing.T) {
	manager, store := makeTwoTargetRuleChangeManager(t)

	// Both targets watch configmaps.
	store.AddOrUpdateClusterWatchRule(
		clusterRuleForResource("rule-a", "target-a", "configmaps"),
		"target-a", "test-ns", "test-provider", "test-ns", "main", "path-a",
	)
	store.AddOrUpdateClusterWatchRule(
		clusterRuleForResource("rule-b", "target-b", "configmaps"),
		"target-b", "test-ns", "test-provider", "test-ns", "main", "path-b",
	)

	// Steady state: both snapshots already delivered.
	seedDeliveredBaseline(manager)

	// Change ONLY target B's effective plan: it now also watches secrets.
	// A is untouched.
	store.AddOrUpdateClusterWatchRule(
		clusterRuleForResource("rule-b-secrets", "target-b", "secrets"),
		"target-b", "test-ns", "test-provider", "test-ns", "main", "path-b",
	)

	require.NoError(t, manager.ReconcileForRuleChange(ctx()))

	assert.True(t, targetPending(manager, "target-b"),
		"target B's effective watch plan changed (now watches secrets), so B must be selected")
	assert.False(t, targetPending(manager, "target-a"),
		"target A's effective watch plan did not change; unrelated global GVR churn must not "+
			"drag A into a rule-change snapshot. GitTargets are an isolation boundary — see "+
			"docs/design/gittarget-isolation-on-rule-change.md")
}

// TestReconcileForRuleChange_RedundantDuplicateRule_DoesNotSnapshot pins the
// flip side of the isolation fix: because the hash is over the *effective watch
// plan* (resolved GVR + scope + operations), a second rule that resolves to the
// same surface a target already watches does not change the plan and must not
// trigger a no-op snapshot. This is what dropping source rule identity buys.
func TestReconcileForRuleChange_RedundantDuplicateRule_DoesNotSnapshot(t *testing.T) {
	manager, store := makeTwoTargetRuleChangeManager(t)

	store.AddOrUpdateClusterWatchRule(
		clusterRuleForResource("rule-a", "target-a", "configmaps"),
		"target-a", "test-ns", "test-provider", "test-ns", "main", "path-a",
	)
	seedDeliveredBaseline(manager)

	// A second rule that resolves to the exact same surface (configmaps) for the
	// same target — a redundant duplicate. The effective plan is unchanged.
	store.AddOrUpdateClusterWatchRule(
		clusterRuleForResource("rule-a-dup", "target-a", "configmaps"),
		"target-a", "test-ns", "test-provider", "test-ns", "main", "path-a",
	)

	require.NoError(t, manager.ReconcileForRuleChange(ctx()))

	assert.False(t, targetPending(manager, "target-a"),
		"a redundant duplicate rule does not change the effective watch plan and must not snapshot")
}

// TestSnapshotTargetsNeedingDelivery_PerTargetHashIsolatesTargets exercises the
// selection function directly (no full reconcile): with two targets in steady
// state and only B's plan changing, only B is returned. This is the unit-level
// guard for the per-target hash diff that snapshot selection now relies on.
func TestSnapshotTargetsNeedingDelivery_PerTargetHashIsolatesTargets(t *testing.T) {
	manager, store := makeTwoTargetRuleChangeManager(t)

	store.AddOrUpdateClusterWatchRule(
		clusterRuleForResource("rule-a", "target-a", "configmaps"),
		"target-a", "test-ns", "test-provider", "test-ns", "main", "path-a",
	)
	store.AddOrUpdateClusterWatchRule(
		clusterRuleForResource("rule-b", "target-b", "configmaps"),
		"target-b", "test-ns", "test-provider", "test-ns", "main", "path-b",
	)
	seedDeliveredBaseline(manager)

	store.AddOrUpdateClusterWatchRule(
		clusterRuleForResource("rule-b-secrets", "target-b", "secrets"),
		"target-b", "test-ns", "test-provider", "test-ns", "main", "path-b",
	)

	selected := manager.snapshotTargetsNeedingDelivery()

	keyA := types.NewResourceReference("target-a", "test-ns").Key()
	keyB := types.NewResourceReference("target-b", "test-ns").Key()
	selectedKeys := make(map[string]struct{}, len(selected))
	for _, target := range selected {
		selectedKeys[target.gitDest.Key()] = struct{}{}
	}

	assert.Contains(t, selectedKeys, keyB,
		"B (whose effective plan changed) is correctly selected")
	assert.NotContains(t, selectedKeys, keyA,
		"A (whose effective plan is unchanged) is correctly skipped")
}

// ctx returns a background context. Wrapped for terse use in test setup.
func ctx() context.Context { return context.Background() }

// Compile-time sanity: corev1 is used by tests that may extend this file with
// pre-populated ConfigMap objects to assert backfill content. Today only the
// snapshot-emit counter is asserted; keep the import for cheap follow-up tests.
var _ = corev1.ConfigMap{}
