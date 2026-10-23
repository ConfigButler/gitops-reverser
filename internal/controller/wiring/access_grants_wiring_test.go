// SPDX-License-Identifier: Apache-2.0

package wiring

import (
	"context"
	"strings"
	"testing"
	"time"

	meta "github.com/fluxcd/pkg/apis/meta"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/authz"
	"github.com/ConfigButler/gitops-reverser/internal/controller"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/watch"
)

// Revocation and recovery are driven by watch events: a grant edit or a Namespace label change, and
// nothing on the consumer. Each wait is propagationBudget, shorter than any requeue that could
// rescue a missing watch.

// conditionsOf reads the conditions of a WatchRule, GitBranch or GitTarget.
func conditionsOf(obj client.Object) []metav1.Condition {
	switch typed := obj.(type) {
	case *configbutleraiv1alpha3.WatchRule:
		return typed.Status.Conditions
	case *configbutleraiv1alpha3.ClusterWatchRule:
		return typed.Status.Conditions
	case *configbutleraiv1alpha3.GitBranch:
		return typed.Status.Conditions
	case *configbutleraiv1alpha3.GitTarget:
		return typed.Status.Conditions
	case *configbutleraiv1alpha3.FluxClaim:
		return typed.Status.Conditions
	}
	return nil
}

// awaitCondition polls key until its condition conditionType has reason want, or fails.
func awaitCondition(
	ctx context.Context, t *testing.T, key types.NamespacedName, fresh func() client.Object,
	conditionType, want string, budget time.Duration,
) *metav1.Condition {
	t.Helper()
	deadline := time.Now().Add(budget)
	last := "absent"
	for time.Now().Before(deadline) {
		obj := fresh()
		if err := k8sClient.Get(ctx, key, obj); err == nil {
			if c := apimeta.FindStatusCondition(conditionsOf(obj), conditionType); c != nil {
				if c.Reason == want {
					return c
				}
				last = c.Reason + ": " + c.Message
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s %s never reached reason %q within %s (last seen %q)", key, conditionType, want, budget, last)
	return nil
}

// createNamespace creates a Namespace with labels.
func createNamespace(ctx context.Context, t *testing.T, name string, labels map[string]string) {
	t.Helper()
	require.NoError(
		t,
		k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}),
	)
}

// setLabels replaces a Namespace's labels.
func setLabels(ctx context.Context, t *testing.T, name string, labels map[string]string) {
	t.Helper()
	require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		ns := &corev1.Namespace{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, ns); err != nil {
			return err
		}
		ns.Labels = labels
		return k8sClient.Update(ctx, ns)
	}))
}

// TestWatchRule_ANarrowedScopeRefusesOnALabelChange: a namespace matched by two grants loses the label
// of the one granting "*". Its GitTarget stays admitted, so its readiness does not move; only the
// WatchRule Namespace watch re-evaluates the rule, and a restored label admits it again.
func TestWatchRule_ANarrowedScopeRefusesOnALabelChange(t *testing.T) {
	ctx := context.Background()
	startManager(t, func(m manager.Manager) error {
		return (&controller.WatchRuleReconciler{
			Client: m.GetClient(), Scheme: m.GetScheme(), RuleStore: rulestore.NewStore(),
		}).SetupWithManager(m)
	})

	const namespace = "wiring-scope"
	both := map[string]string{"mirror": "yes", "audit": "yes"}
	createNamespace(ctx, t, namespace, both)
	require.NoError(t, k8sClient.Create(ctx, &configbutleraiv1alpha3.KubeProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "source", Namespace: namespace},
		Spec: configbutleraiv1alpha3.KubeProviderSpec{
			InCluster: &configbutleraiv1alpha3.KubeProviderInCluster{},
			AllowedGitTargets: []configbutleraiv1alpha3.SourceGrant{
				{From: configbutleraiv1alpha3.GrantFromSelector,
					Selector:         &metav1.LabelSelector{MatchLabels: map[string]string{"mirror": "yes"}},
					SourceNamespaces: []configbutleraiv1alpha3.ScopeNamespace{"{namespace}"}},
				{From: configbutleraiv1alpha3.GrantFromSelector,
					Selector:         &metav1.LabelSelector{MatchLabels: map[string]string{"audit": "yes"}},
					SourceNamespaces: []configbutleraiv1alpha3.ScopeNamespace{"*"}},
			},
		},
	}))
	target := &configbutleraiv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: namespace},
		Spec: configbutleraiv1alpha3.GitTargetSpec{
			KubeProviderRef: meta.NamespacedObjectReference{Name: "source"},
			GitBranchRef:    meta.NamespacedObjectReference{Name: "provider-main"},
			Path:            "mirror",
		},
	}
	require.NoError(t, k8sClient.Create(ctx, target))
	rule := &configbutleraiv1alpha3.WatchRule{
		ObjectMeta: metav1.ObjectMeta{Name: "rule", Namespace: namespace},
		Spec: configbutleraiv1alpha3.WatchRuleSpec{
			GitTargetRef: meta.LocalObjectReference{Name: "target"},
			Rules: []configbutleraiv1alpha3.ResourceRule{
				{Resources: []string{"configmaps"}, SourceNamespace: configbutleraiv1alpha3.SourceNamespaceWildcard},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, rule))
	key := client.ObjectKeyFromObject(rule)
	fresh := func() client.Object { return &configbutleraiv1alpha3.WatchRule{} }

	awaitCondition(ctx, t, key, fresh, controller.ConditionTypeSourceNamespaceAuthorized,
		controller.WatchRuleReasonSourceNamespaceAllowed, settleBudget)

	setLabels(ctx, t, namespace, map[string]string{"mirror": "yes"})
	refused := awaitCondition(ctx, t, key, fresh, controller.ConditionTypeSourceNamespaceAuthorized,
		controller.WatchRuleReasonSourceNamespaceNotAllowed, propagationBudget)
	require.Contains(t, refused.Message, `watch "wiring-scope"`)
	decision, err := authz.GitTargetAdmitted(ctx, k8sClient, target)
	require.NoError(t, err)
	require.True(t, decision.Allowed, "the GitTarget stays admitted for its narrower scope")

	setLabels(ctx, t, namespace, both)
	awaitCondition(ctx, t, key, fresh, controller.ConditionTypeSourceNamespaceAuthorized,
		controller.WatchRuleReasonSourceNamespaceAllowed, propagationBudget)
}

// registerBranchAndTargetControllers installs the GitBranch and GitTarget controllers, the GitBranch
// index they find a provider's GitBranches through, and the worker manager that holds the claims.
func registerBranchAndTargetControllers(m manager.Manager) error {
	if err := controller.IndexGitBranches(context.Background(), m.GetFieldIndexer()); err != nil {
		return err
	}
	workers := git.NewWorkerManager(m.GetClient(), logr.Discard(), git.BranchWorkerLimits{},
		itypes.SensitiveResourcePolicy{})
	if err := m.Add(workers); err != nil {
		return err
	}
	if err := (&controller.GitBranchReconciler{
		Client: m.GetClient(), Scheme: m.GetScheme(), WorkerManager: workers,
	}).SetupWithManager(m); err != nil {
		return err
	}
	return (&controller.GitTargetReconciler{
		Client: m.GetClient(), Scheme: m.GetScheme(), WorkerManager: workers,
		EventRouter: watch.NewEventRouter(workers, nil, m.GetClient(), logr.Discard()),
	}).SetupWithManager(m)
}

// TestGitBranch_ALabelRemovedRevokesTheClaimAndItsGitTargetsFollow: a label removed from a claimant's
// namespace revokes its GitBranch through the GitBranch Namespace watch, and a GitTarget in another
// namespace writing through it follows. Restoring the label re-admits the GitBranch, which then
// tries to prove itself again. Then the branch owner's writer grant is withdrawn and restored, and
// the GitTarget is refused and admitted again, each on the GitBranch event alone.
func TestGitBranch_ALabelRemovedRevokesTheClaimAndItsGitTargetsFollow(t *testing.T) {
	ctx := context.Background()
	startManager(t, registerBranchAndTargetControllers)

	tenant := map[string]string{"tenant": "true"}
	createNamespace(ctx, t, "wiring-platform", nil)
	createNamespace(ctx, t, "wiring-tenant", tenant)
	createNamespace(ctx, t, "wiring-writer", nil)
	require.NoError(t, k8sClient.Create(ctx, &configbutleraiv1alpha3.GitProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-repo", Namespace: "wiring-platform"},
		Spec: configbutleraiv1alpha3.GitProviderSpec{
			// Nothing listens here, so the push-session proof fails at once: the reasons tell an
			// admitted GitBranch (PushSessionFailed) from a revoked one (NamespaceNotAuthorized).
			URL: "https://127.0.0.1:1/platform/config.git",
			AllowedGitBranches: []configbutleraiv1alpha3.GitBranchGrant{{
				From:     configbutleraiv1alpha3.GrantFromSelector,
				Selector: &metav1.LabelSelector{MatchLabels: tenant},
				Branches: []configbutleraiv1alpha3.BranchPattern{"teams/{namespace}/*"},
			}},
		},
	}))
	branch := &configbutleraiv1alpha3.GitBranch{
		ObjectMeta: metav1.ObjectMeta{Name: "live", Namespace: "wiring-tenant"},
		Spec: configbutleraiv1alpha3.GitBranchSpec{
			GitProviderRef: meta.NamespacedObjectReference{Name: "shared-repo", Namespace: "wiring-platform"},
			Branch:         "teams/wiring-tenant/live",
			AllowedGitTargets: []configbutleraiv1alpha3.NamespaceGrant{
				{
					From:  configbutleraiv1alpha3.GrantFromNames,
					Names: []configbutleraiv1alpha3.GrantNamespace{"wiring-writer"},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, branch))
	require.NoError(t, k8sClient.Create(ctx, &configbutleraiv1alpha3.KubeProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "source", Namespace: "wiring-writer"},
		Spec: configbutleraiv1alpha3.KubeProviderSpec{
			InCluster: &configbutleraiv1alpha3.KubeProviderInCluster{},
			AllowedGitTargets: []configbutleraiv1alpha3.SourceGrant{{From: configbutleraiv1alpha3.GrantFromSame,
				SourceNamespaces: []configbutleraiv1alpha3.ScopeNamespace{"{namespace}"}}},
		},
	}))
	target := &configbutleraiv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "apps", Namespace: "wiring-writer"},
		Spec: configbutleraiv1alpha3.GitTargetSpec{
			KubeProviderRef: meta.NamespacedObjectReference{Name: "source"},
			GitBranchRef:    meta.NamespacedObjectReference{Name: "live", Namespace: "wiring-tenant"},
			Path:            "apps",
		},
	}
	require.NoError(t, k8sClient.Create(ctx, target))
	branchKey := client.ObjectKeyFromObject(branch)
	freshBranch := func() client.Object { return &configbutleraiv1alpha3.GitBranch{} }
	awaitCondition(ctx, t, branchKey, freshBranch, controller.ConditionTypeReady,
		controller.GitBranchReasonPushSessionFailed, settleBudget)

	setLabels(ctx, t, "wiring-tenant", nil)
	awaitCondition(ctx, t, branchKey, freshBranch, controller.ConditionTypeReady,
		controller.GitBranchReasonNamespaceNotAuthorized, propagationBudget)
	targetKey := client.ObjectKeyFromObject(target)
	awaitTarget(ctx, t, targetKey, controller.GitTargetConditionGitBranchReady,
		func(c *metav1.Condition) bool { return strings.Contains(c.Message, "spec.allowedGitBranches") })

	setLabels(ctx, t, "wiring-tenant", tenant)
	awaitCondition(ctx, t, branchKey, freshBranch, controller.ConditionTypeReady,
		controller.GitBranchReasonPushSessionFailed, propagationBudget)

	// The branch owner removes the writer's namespace from allowedGitTargets, and adds it back: the
	// GitBranch edit reaches the GitTarget in the other namespace each time.
	refused := func(c *metav1.Condition) bool { return c.Reason == controller.GitTargetReasonNamespaceNotAuthorized }
	admitted := func(c *metav1.Condition) bool { return !refused(c) }
	writers := branch.Spec.AllowedGitTargets
	setWriters(ctx, t, branchKey, []configbutleraiv1alpha3.NamespaceGrant{{From: configbutleraiv1alpha3.GrantFromSame}})
	awaitTarget(ctx, t, targetKey, controller.GitTargetConditionValidated, refused)
	setWriters(ctx, t, branchKey, writers)
	awaitTarget(ctx, t, targetKey, controller.GitTargetConditionValidated, admitted)
}

// awaitTarget polls a GitTarget until its condition conditionType satisfies want, within
// propagationBudget.
func awaitTarget(
	ctx context.Context, t *testing.T, key types.NamespacedName, conditionType string,
	want func(*metav1.Condition) bool,
) {
	t.Helper()
	deadline := time.Now().Add(propagationBudget)
	var last *metav1.Condition
	for time.Now().Before(deadline) {
		got := &configbutleraiv1alpha3.GitTarget{}
		require.NoError(t, k8sClient.Get(ctx, key, got))
		if last = apimeta.FindStatusCondition(got.Status.Conditions, conditionType); last != nil && want(last) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("GitTarget %s %s never reached what was waited for within %s: %+v", key, conditionType,
		propagationBudget, last)
}

// setWriters replaces a GitBranch's allowedGitTargets.
func setWriters(
	ctx context.Context, t *testing.T, key types.NamespacedName, grants []configbutleraiv1alpha3.NamespaceGrant,
) {
	t.Helper()
	require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		stored := &configbutleraiv1alpha3.GitBranch{}
		if err := k8sClient.Get(ctx, key, stored); err != nil {
			return err
		}
		stored.Spec.AllowedGitTargets = grants
		return k8sClient.Update(ctx, stored)
	}))
}
