// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
)

const (
	wrsnTenantNS = "tenant-acme"
	wrsnSourceNS = "repo-config"
	wrsnTarget   = "acme"
	wrsnRule     = "repo-config-rule"
	wrsnProvider = "workspaces"
)

func wrsnGitTarget(policy *configbutleraiv1alpha3.NamespaceMatcher) *configbutleraiv1alpha3.GitTarget {
	return &configbutleraiv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: wrsnTarget, Namespace: wrsnTenantNS},
		Spec: configbutleraiv1alpha3.GitTargetSpec{
			ProviderRef:             configbutleraiv1alpha3.GitProviderReference{Name: "git"},
			ClusterProviderRef:      &configbutleraiv1alpha3.ClusterProviderReference{Name: wrsnProvider},
			Branch:                  "main",
			Path:                    "tenants/acme",
			AllowedSourceNamespaces: policy,
		},
	}
}

func wrsnGitProvider() *configbutleraiv1alpha3.GitProvider {
	return &configbutleraiv1alpha3.GitProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "git", Namespace: wrsnTenantNS},
	}
}

func wrsnClusterProvider(delegate bool) *configbutleraiv1alpha3.ClusterProvider {
	return &configbutleraiv1alpha3.ClusterProvider{
		ObjectMeta: metav1.ObjectMeta{Name: wrsnProvider},
		Spec: configbutleraiv1alpha3.ClusterProviderSpec{
			AllowedNamespaces: &configbutleraiv1alpha3.NamespaceMatcher{
				Names: []string{wrsnTenantNS},
			},
			AllowWatchRuleSourceNamespaceOverride: delegate,
		},
	}
}

func wrsnWatchRule(sourceNamespace string) *configbutleraiv1alpha3.WatchRule {
	return &configbutleraiv1alpha3.WatchRule{
		ObjectMeta: metav1.ObjectMeta{Name: wrsnRule, Namespace: wrsnTenantNS, Generation: 1},
		Spec: configbutleraiv1alpha3.WatchRuleSpec{
			TargetRef:       configbutleraiv1alpha3.LocalTargetReference{Name: wrsnTarget},
			SourceNamespace: sourceNamespace,
			Rules:           []configbutleraiv1alpha3.ResourceRule{{Resources: []string{"configmaps"}}},
		},
	}
}

type wrsnFixture struct {
	reconciler *WatchRuleReconciler
	store      *rulestore.RuleStore
	wm         *cwaWatchManager
	client     client.Client
}

func newWRSNFixture(t *testing.T, objects []client.Object) *wrsnFixture {
	t.Helper()

	cl := fake.NewClientBuilder().
		WithScheme(scScheme(t)).
		WithObjects(objects...).
		WithStatusSubresource(&configbutleraiv1alpha3.WatchRule{}).
		Build()

	store := rulestore.NewStore()
	wm := &cwaWatchManager{}

	return &wrsnFixture{
		reconciler: &WatchRuleReconciler{
			Client:       cl,
			Scheme:       cl.Scheme(),
			RuleStore:    store,
			WatchManager: wm,
		},
		store:  store,
		wm:     wm,
		client: cl,
	}
}

func (f *wrsnFixture) reconcile(ctx context.Context) (ctrl.Result, error) {
	return f.reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: k8stypes.NamespacedName{Name: wrsnRule, Namespace: wrsnTenantNS},
	})
}

func (f *wrsnFixture) compiledNames() []string {
	names := []string{}
	for _, r := range f.store.SnapshotWatchRules() {
		names = append(names, r.Source.Name)
	}
	return names
}

func (f *wrsnFixture) reloadRule(ctx context.Context, t *testing.T) *configbutleraiv1alpha3.WatchRule {
	t.Helper()
	var rule configbutleraiv1alpha3.WatchRule
	require.NoError(t, f.client.Get(ctx,
		k8stypes.NamespacedName{Name: wrsnRule, Namespace: wrsnTenantNS}, &rule))
	return &rule
}

func wrsnCondition(t *testing.T, rule *configbutleraiv1alpha3.WatchRule, conditionType string) *metav1.Condition {
	t.Helper()
	cond := apimeta.FindStatusCondition(rule.Status.Conditions, conditionType)
	require.NotNil(t, cond, "condition %s must be published", conditionType)
	return cond
}

func wrsnBaseObjects(
	policy *configbutleraiv1alpha3.NamespaceMatcher,
	delegate bool,
	sourceNamespace string,
) []client.Object {
	return []client.Object{
		wrsnGitTarget(policy),
		wrsnGitProvider(),
		wrsnClusterProvider(delegate),
		wrsnWatchRule(sourceNamespace),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: wrsnTenantNS}},
	}
}

// TestReconcile_LegacyWatchRuleNeedsNoPolicyOrFlag is THE test, and it is first on purpose.
//
// A WatchRule that omits sourceNamespace must compile with no GitTarget policy and no delegation
// flag. If this fails, deny-by-default has broken every existing WatchRule on upgrade.
func TestReconcile_LegacyWatchRuleNeedsNoPolicyOrFlag(t *testing.T) {
	ctx := context.Background()
	f := newWRSNFixture(t, wrsnBaseObjects(nil, false, ""))

	_, err := f.reconcile(ctx)

	require.NoError(t, err)
	assert.Equal(t, []string{wrsnRule}, f.compiledNames(),
		"an existing own-namespace WatchRule must keep working with no new configuration")

	cond := wrsnCondition(t, f.reloadRule(ctx, t), ConditionTypeSourceNamespaceAuthorized)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, WatchRuleReasonLegacySourceNamespace, cond.Reason)
}

// TestReconcile_DeniedSourceNamespaceStartsNoWatch mirrors
// TestReconcile_UnauthorizedNamespaceStartsNoWatch: a denied override must leave NO compiled rule.
// The gate has to stop the data plane, not just describe it.
func TestReconcile_DeniedSourceNamespaceStartsNoWatch(t *testing.T) {
	ctx := context.Background()
	// The target names the namespace, but the provider does not delegate.
	f := newWRSNFixture(t, wrsnBaseObjects(
		&configbutleraiv1alpha3.NamespaceMatcher{Names: []string{wrsnSourceNS}},
		false, wrsnSourceNS))

	_, err := f.reconcile(ctx)

	require.NoError(t, err)
	assert.Empty(t, f.compiledNames(), "a denied override must start no watch")

	rule := f.reloadRule(ctx, t)
	cond := wrsnCondition(t, rule, ConditionTypeSourceNamespaceAuthorized)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, WatchRuleReasonSourceNamespaceNotAllowed, cond.Reason)
	assert.Contains(t, cond.Message, "allowWatchRuleSourceNamespaceOverride",
		"the message must name the fix")
}

// TestReconcile_DeniedSourceNamespacePublishesTheFailedTrio pins the whole kstatus verdict a
// refusal produces: Failed, under the one reason an operator greps for.
func TestReconcile_DeniedSourceNamespacePublishesTheFailedTrio(t *testing.T) {
	ctx := context.Background()
	f := newWRSNFixture(t, wrsnBaseObjects(nil, true, wrsnSourceNS))

	_, err := f.reconcile(ctx)
	require.NoError(t, err)

	rule := f.reloadRule(ctx, t)
	for _, want := range []struct {
		conditionType string
		status        metav1.ConditionStatus
	}{
		{ConditionTypeSourceNamespaceAuthorized, metav1.ConditionFalse},
		{ConditionTypeStreamsRunning, metav1.ConditionFalse},
		{ConditionTypeReady, metav1.ConditionFalse},
		{ConditionTypeReconciling, metav1.ConditionFalse},
		{ConditionTypeStalled, metav1.ConditionTrue},
	} {
		cond := wrsnCondition(t, rule, want.conditionType)
		assert.Equal(t, want.status, cond.Status, "condition %s status", want.conditionType)
		assert.Equal(t, WatchRuleReasonSourceNamespaceNotAllowed, cond.Reason,
			"condition %s reason", want.conditionType)
		assert.Equal(t, rule.Generation, cond.ObservedGeneration,
			"condition %s must carry the observed generation, or a stale verdict reads as current",
			want.conditionType)
	}
}

// TestReconcile_AuthorizedOverrideCompilesWithItsSourceNamespace is the grant path end to end: all
// three legs pass, and the compiled rule carries the SOURCE namespace rather than the rule's own.
func TestReconcile_AuthorizedOverrideCompilesWithItsSourceNamespace(t *testing.T) {
	ctx := context.Background()
	f := newWRSNFixture(t, wrsnBaseObjects(
		&configbutleraiv1alpha3.NamespaceMatcher{Names: []string{wrsnSourceNS}},
		true, wrsnSourceNS))

	_, err := f.reconcile(ctx)
	require.NoError(t, err)

	compiled := f.store.SnapshotWatchRules()
	require.Len(t, compiled, 1)
	assert.Equal(t, wrsnSourceNS, compiled[0].SourceNamespace)
	assert.Equal(t, wrsnTenantNS, compiled[0].Source.Namespace)

	cond := wrsnCondition(t, f.reloadRule(ctx, t), ConditionTypeSourceNamespaceAuthorized)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, WatchRuleReasonSourceNamespaceAllowed, cond.Reason)
}

// TestReconcile_RevokedSourceNamespaceRemovesTheCompiledRuleAndReplans is the REVOCATION contract.
// A rule accepted and then denied by a tightened policy must have its compiled rule REMOVED and
// the watch manager replanned — and the removal must already have happened by the time the replan
// runs, because status is published only after that. A gate that reports without stopping is not a
// gate.
func TestReconcile_RevokedSourceNamespaceRemovesTheCompiledRuleAndReplans(t *testing.T) {
	ctx := context.Background()
	f := newWRSNFixture(t, wrsnBaseObjects(
		&configbutleraiv1alpha3.NamespaceMatcher{Names: []string{wrsnSourceNS}},
		true, wrsnSourceNS))

	_, err := f.reconcile(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{wrsnRule}, f.compiledNames(), "precondition: the rule is compiled")

	// The target owner tightens the policy so it no longer admits the namespace.
	var target configbutleraiv1alpha3.GitTarget
	require.NoError(t, f.client.Get(ctx,
		k8stypes.NamespacedName{Name: wrsnTarget, Namespace: wrsnTenantNS}, &target))
	target.Spec.AllowedSourceNamespaces = &configbutleraiv1alpha3.NamespaceMatcher{
		Names: []string{"a-completely-different-namespace"},
	}
	require.NoError(t, f.client.Update(ctx, &target))

	// Observe the world at the exact moment the data plane is replanned.
	var compiledAtReplan []string
	f.wm.onReconcile = func() { compiledAtReplan = f.compiledNames() }

	_, err = f.reconcile(ctx)
	require.NoError(t, err)

	assert.Empty(t, f.compiledNames(), "a revoked rule must be removed from the store")
	assert.Empty(t, compiledAtReplan,
		"the compiled rule must already be gone when the watch manager is replanned")

	cond := wrsnCondition(t, f.reloadRule(ctx, t), ConditionTypeSourceNamespaceAuthorized)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
}

// TestReconcile_DeclaredPolicyDeniesCoResidentLegacyRule is the no-self-namespace-exception rule at
// the reconciler, plus its mitigation: the denial must NAME the fix, since this is the design's
// acknowledged authoring footgun.
func TestReconcile_DeclaredPolicyDeniesCoResidentLegacyRule(t *testing.T) {
	ctx := context.Background()
	// A policy was added for some other namespace; this rule watches its OWN namespace.
	f := newWRSNFixture(t, wrsnBaseObjects(
		&configbutleraiv1alpha3.NamespaceMatcher{Names: []string{wrsnSourceNS}},
		true, ""))

	_, err := f.reconcile(ctx)
	require.NoError(t, err)

	assert.Empty(t, f.compiledNames(), "a declared policy is exhaustive, own namespace included")

	cond := wrsnCondition(t, f.reloadRule(ctx, t), ConditionTypeSourceNamespaceAuthorized)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, WatchRuleReasonSourceNamespaceNotAllowed, cond.Reason)
	assert.Contains(t, cond.Message, "add it to keep watching this rule's own namespace",
		"the footgun is only acceptable because the denial names the exact fix")
}

// TestReconcile_DeclaredPolicyAdmittingOwnNamespaceCompiles is the other half: listing the rule's
// own namespace explicitly is how a legacy rule co-exists with a policy.
func TestReconcile_DeclaredPolicyAdmittingOwnNamespaceCompiles(t *testing.T) {
	ctx := context.Background()
	f := newWRSNFixture(t, wrsnBaseObjects(
		&configbutleraiv1alpha3.NamespaceMatcher{Names: []string{wrsnTenantNS, wrsnSourceNS}},
		true, ""))

	_, err := f.reconcile(ctx)
	require.NoError(t, err)

	assert.Equal(t, []string{wrsnRule}, f.compiledNames())
	cond := wrsnCondition(t, f.reloadRule(ctx, t), ConditionTypeSourceNamespaceAuthorized)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, WatchRuleReasonSourceNamespaceAllowed, cond.Reason)
}

// TestReconcile_SelectorPolicyWithNoSourceScopeIsInProgress covers the Unknown row of the status
// table: with no source-scope service wired, a selector policy is "cannot say yet". It must be
// InProgress (Reconciling=True, Stalled=False), never Failed — turning a transient into a terminal
// state is precisely what the three-valued result exists to prevent.
func TestReconcile_SelectorPolicyWithNoSourceScopeIsInProgress(t *testing.T) {
	ctx := context.Background()
	f := newWRSNFixture(t, wrsnBaseObjects(
		&configbutleraiv1alpha3.NamespaceMatcher{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"mirrorable": "true"}},
		},
		true, wrsnSourceNS))

	_, err := f.reconcile(ctx)
	require.NoError(t, err)

	assert.Empty(t, f.compiledNames(), "no grant is established, so nothing runs")

	rule := f.reloadRule(ctx, t)
	cond := wrsnCondition(t, rule, ConditionTypeSourceNamespaceAuthorized)
	assert.Equal(t, metav1.ConditionUnknown, cond.Status)
	assert.Equal(t, WatchRuleReasonCheckingSourceNamespacePolicy, cond.Reason)

	assert.Equal(t, metav1.ConditionFalse, wrsnCondition(t, rule, ConditionTypeReady).Status)
	assert.Equal(t, metav1.ConditionTrue, wrsnCondition(t, rule, ConditionTypeReconciling).Status)
	assert.Equal(t, metav1.ConditionFalse, wrsnCondition(t, rule, ConditionTypeStalled).Status,
		"a cache that has not synced is not a stalled rule")
}

// TestReconcile_ClusterProviderReadErrorRequeuesWithoutDenying: a transient apiserver failure must
// surface as an error the controller requeues on, and must NOT tear down an already-compiled rule.
func TestReconcile_ClusterProviderReadErrorRequeuesWithoutDenying(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("etcdserver: request timed out")

	f := newWRSNFixture(t, wrsnBaseObjects(
		&configbutleraiv1alpha3.NamespaceMatcher{Names: []string{wrsnSourceNS}},
		true, wrsnSourceNS))

	// Compile it once cleanly.
	_, err := f.reconcile(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{wrsnRule}, f.compiledNames())

	// Now make the ClusterProvider read fail.
	f.reconciler.Client = fake.NewClientBuilder().
		WithScheme(scScheme(t)).
		WithObjects(wrsnBaseObjects(
			&configbutleraiv1alpha3.NamespaceMatcher{Names: []string{wrsnSourceNS}},
			true, wrsnSourceNS)...).
		WithStatusSubresource(&configbutleraiv1alpha3.WatchRule{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(
				ctx context.Context, c client.WithWatch, key client.ObjectKey,
				obj client.Object, opts ...client.GetOption,
			) error {
				if _, ok := obj.(*configbutleraiv1alpha3.ClusterProvider); ok {
					return boom
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).
		Build()

	_, err = f.reconcile(ctx)

	require.Error(t, err, "a transient read error must requeue, not silently deny")
	assert.Equal(t, []string{wrsnRule}, f.compiledNames(),
		"a running stream must survive an apiserver blip")
}
