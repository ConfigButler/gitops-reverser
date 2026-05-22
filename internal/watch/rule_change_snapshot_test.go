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

// makeRuleChangeTestManager constructs a Manager configured for these tests:
// fake controller-runtime client, real RuleStore, a trusted-but-minimal API
// resource catalog that does not serve configmaps (so rule resolution plans no
// informer GVRs and no real informers start), and a target GitTarget already in
// the fake client.
//
// The configmaps GVR is also pre-populated in activeInformers with a no-op
// cancel so compareGVRs sees the GVR as "already watched". This is the
// steady-state we want to model: an informer for that GVR is logically
// running, so adding a second rule for it should not cause an informer churn,
// but *should* re-trigger a snapshot.
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
	configMapGVR := GVR{
		Group:    "",
		Version:  "v1",
		Resource: "configmaps",
		Scope:    configv1alpha1.ResourceScopeNamespaced,
	}
	manager := &Manager{
		Client:    fakeClient,
		Log:       logr.Discard(),
		RuleStore: store,
		// The catalog does not serve configmaps, so these tests exercise
		// snapshot delivery after rule-set changes without planning real
		// informers.
		resourceCatalog: newSnapshotDeliveryTestCatalog(t),
		discoveryClient: snapshotDeliveryTestDiscoveryClient(),
		// Pretend the configmaps informer is already running cluster-wide,
		// so compareGVRs reports added=0, removed=0 for that GVR. No real
		// informers are started.
		activeInformers: map[GVR]map[string]context.CancelFunc{
			configMapGVR: {"": func() {}},
		},
	}

	return manager, store
}

// configMapRuleForTarget builds a ClusterWatchRule that selects core/v1
// configmaps, scoped at the given GitTarget (always in the shared "test-ns"
// namespace used across these tests).
func configMapRuleForTarget(name, gitTargetName string) configv1alpha1.ClusterWatchRule {
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
				Resources:   []string{"configmaps"},
				Scope:       configv1alpha1.ResourceScopeNamespaced,
			}},
		},
	}
}

// TestReconcileForRuleChange_AddingSecondRuleSameGVR_EmitsSnapshot exercises
// the central case. After the first rule is registered and reconciled, the
// informer set is steady-state. Adding a *second* rule that names the same GVR
// does not change the GVR set — only the rule-set changes. The contract: any
// rule-set change must still emit a snapshot for affected GitTargets, so
// resources that already match the new rule are backfilled to git.
func TestReconcileForRuleChange_AddingSecondRuleSameGVR_EmitsSnapshot(t *testing.T) {
	manager, store := makeRuleChangeTestManager(t)
	ctx := context.Background()

	// First rule: register, reconcile once. This brings the manager into a
	// stable state where the configmaps informer is logically active.
	store.AddOrUpdateClusterWatchRule(
		configMapRuleForTarget("rule-1", "test-target"),
		"test-target", "test-ns",
		"test-provider", "test-ns",
		"main", "test-path",
	)
	require.NoError(t, manager.ReconcileForRuleChange(ctx))

	// Reset counter so the assertion targets only the *second* reconcile.
	manager.snapshotEmitCount.Store(0)

	// Second rule for the same GVR. The GVR diff is empty; only the rule-set
	// changes — ReconcileForRuleChange must still emit a snapshot.
	store.AddOrUpdateClusterWatchRule(
		configMapRuleForTarget("rule-2", "test-target"),
		"test-target", "test-ns",
		"test-provider", "test-ns",
		"main", "test-path",
	)
	require.NoError(t, manager.ReconcileForRuleChange(ctx))

	assert.Positive(t,
		manager.SnapshotEmitCount(),
		"adding a ClusterWatchRule should emit a snapshot even when no new GVR is introduced; "+
			"existing matching resources must be backfilled to git")
}

// TestReconcileForRuleChange_NarrowingSelectorOnExistingRule_EmitsSnapshot
// covers a sibling gap: editing a rule's selectors without changing its GVR.
// A common operator action is "I added a label selector to my existing rule to
// scope it down" — the GVR set does not change, but the matched resource set
// does. Same root cause as the test above. Uses a distinct GitTarget name
// from the prior test so the same helper exercises more than one input.
func TestReconcileForRuleChange_NarrowingSelectorOnExistingRule_EmitsSnapshot(t *testing.T) {
	manager, store := makeRuleChangeTestManager(t)
	ctx := context.Background()

	// Initial rule: selects all configmaps via narrow-target.
	initial := configMapRuleForTarget("rule-1", "narrow-target")
	store.AddOrUpdateClusterWatchRule(
		initial,
		"narrow-target", "test-ns",
		"test-provider", "test-ns",
		"main", "test-path",
	)
	require.NoError(t, manager.ReconcileForRuleChange(ctx))

	manager.snapshotEmitCount.Store(0)

	// Edit the rule — narrow its selectors. The GVR (core/v1/configmaps) is
	// unchanged; only the rule body changes.
	narrowed := initial
	narrowed.Spec.Rules[0].Operations = []configv1alpha1.OperationType{
		configv1alpha1.OperationUpdate,
	}
	store.AddOrUpdateClusterWatchRule(
		narrowed,
		"narrow-target", "test-ns",
		"test-provider", "test-ns",
		"main", "test-path",
	)
	require.NoError(t, manager.ReconcileForRuleChange(ctx))

	assert.Positive(t,
		manager.SnapshotEmitCount(),
		"editing a ClusterWatchRule should emit a snapshot when the matched resource set may change, "+
			"even if the GVR set is unchanged")
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

// TestReconcileForRuleChange_AddingSecondWatchRuleSameGVR_EmitsSnapshot is the
// namespaced-WatchRule analogue of the ClusterWatchRule test above. The
// underlying code path is the same (rules feed into ComputeRequestedGVRs and
// collectAffectedGitTargets via the shared RuleStore), so the same gap
// applies. Documenting it as a separate test makes the regression coverage
// for both rule kinds explicit.
func TestReconcileForRuleChange_AddingSecondWatchRuleSameGVR_EmitsSnapshot(t *testing.T) {
	manager, store := makeRuleChangeTestManager(t)
	ctx := context.Background()

	store.AddOrUpdateWatchRule(
		watchRuleForTarget("rule-1", "test-target", "test-ns"),
		"test-target", "test-ns",
		"test-provider", "test-ns",
		"main", "test-path",
	)
	require.NoError(t, manager.ReconcileForRuleChange(ctx))

	manager.snapshotEmitCount.Store(0)

	store.AddOrUpdateWatchRule(
		watchRuleForTarget("rule-2", "test-target", "test-ns"),
		"test-target", "test-ns",
		"test-provider", "test-ns",
		"main", "test-path",
	)
	require.NoError(t, manager.ReconcileForRuleChange(ctx))

	assert.Positive(t,
		manager.SnapshotEmitCount(),
		"adding a second WatchRule should emit a snapshot even when no new GVR is introduced; "+
			"the bug is symmetric across WatchRule and ClusterWatchRule")
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
		// The catalog does not serve configmaps, so the rule's GVR resolves to
		// nothing and does not hit startInformersForGVRs. Combined with a stale
		// entry in activeInformers, compareGVRs reports removed > 0 and the
		// snapshot-emit path runs.
		resourceCatalog: newSnapshotDeliveryTestCatalog(t),
		discoveryClient: snapshotDeliveryTestDiscoveryClient(),
		activeInformers: map[GVR]map[string]context.CancelFunc{
			staleGVR: {"": func() {}},
		},
	}

	// Wire EventRouter with empty-but-non-nil sub-components. This is the
	// state immediately after WatchManager.Start: workers and reconcilers
	// have not yet been registered by their respective controllers.
	manager.EventRouter = &EventRouter{
		WorkerManager:     git.NewWorkerManager(fakeK8s, logr.Discard(), 0),
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

// ctx returns a background context. Wrapped for terse use in test setup.
func ctx() context.Context { return context.Background() }

// Compile-time sanity: corev1 is used by tests that may extend this file with
// pre-populated ConfigMap objects to assert backfill content. Today only the
// snapshot-emit counter is asserted; keep the import for cheap follow-up tests.
var _ = corev1.ConfigMap{}
