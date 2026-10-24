// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"

	meta "github.com/fluxcd/pkg/apis/meta"
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
	// wrsnProviderNS holds the KubeProvider, so wrsnTenantNS reaches it only through a grant.
	wrsnProviderNS = "platform"
)

func wrsnGitTarget() *configbutleraiv1alpha3.GitTarget {
	return &configbutleraiv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: wrsnTarget, Namespace: wrsnTenantNS},
		Spec: configbutleraiv1alpha3.GitTargetSpec{
			GitBranchRef:    meta.NamespacedObjectReference{Name: gitBranchNameForTest("git", "main")},
			KubeProviderRef: meta.NamespacedObjectReference{Namespace: wrsnProviderNS, Name: wrsnProvider},
			Path:            "tenants/acme",
		},
	}
}

func wrsnGitProvider() *configbutleraiv1alpha3.GitProvider {
	return &configbutleraiv1alpha3.GitProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "git", Namespace: wrsnTenantNS},
	}
}

// wrsnKubeProvider admits wrsnTenantNS to watch its own namespace, and every namespace when wide.
func wrsnKubeProvider(wide bool) *configbutleraiv1alpha3.KubeProvider {
	grants := mirrorGrants(wrsnTenantNS)
	if wide {
		grants[0].SourceNamespaces = append(grants[0].SourceNamespaces, "*")
	}
	return &configbutleraiv1alpha3.KubeProvider{
		ObjectMeta: metav1.ObjectMeta{Name: wrsnProvider, Namespace: wrsnProviderNS},
		Spec: configbutleraiv1alpha3.KubeProviderSpec{
			SecretRef:         &meta.SecretKeyReference{Name: "kc"},
			AllowedGitTargets: grants,
		},
	}
}

// wrsnWatchRule builds a rule with one item per given rules[].sourceNamespace ("" = omitted).
func wrsnWatchRule(sourceNamespaces ...string) *configbutleraiv1alpha3.WatchRule {
	items := make([]configbutleraiv1alpha3.ResourceRule, 0, len(sourceNamespaces))
	for _, ns := range sourceNamespaces {
		items = append(items, configbutleraiv1alpha3.ResourceRule{
			Resources: []string{"configmaps"}, SourceNamespace: ns,
		})
	}
	return &configbutleraiv1alpha3.WatchRule{
		ObjectMeta: metav1.ObjectMeta{Name: wrsnRule, Namespace: wrsnTenantNS, Generation: 1},
		Spec: configbutleraiv1alpha3.WatchRuleSpec{
			GitTargetRef: meta.LocalObjectReference{Name: wrsnTarget},
			Rules:        items,
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
	wide bool,
	sourceNamespaces ...string,
) []client.Object {
	return []client.Object{
		wrsnGitTarget(),
		wrsnGitProvider(),
		wrsnKubeProvider(wide),
		wrsnWatchRule(sourceNamespaces...),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: wrsnTenantNS}},
	}
}

// TestReconcile_OwnNamespaceRuleNeedsOnlyANamespaceGrant: a WatchRule that omits sourceNamespace
// watches its own namespace, which a grant giving sourceNamespaces '{namespace}' covers. It needs no
// wider grant.
func TestReconcile_OwnNamespaceRuleNeedsOnlyANamespaceGrant(t *testing.T) {
	ctx := context.Background()
	f := newWRSNFixture(t, wrsnBaseObjects(false, ""))

	_, err := f.reconcile(ctx)

	require.NoError(t, err)
	assert.Equal(t, []string{wrsnRule}, f.compiledNames(),
		"an own-namespace WatchRule needs only a '{namespace}' grant")

	cond := wrsnCondition(t, f.reloadRule(ctx, t), ConditionTypeSourceNamespaceAuthorized)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, WatchRuleReasonSourceNamespaceAllowed, cond.Reason)
}

// TestReconcile_DeniedSourceNamespaceStartsNoWatch mirrors
// TestReconcile_UnauthorizedNamespaceStartsNoWatch: a denied override must leave NO compiled rule.
// The gate has to stop the data plane, not just describe it.
func TestReconcile_DeniedSourceNamespaceStartsNoWatch(t *testing.T) {
	ctx := context.Background()
	// The rule asks for another namespace, but the grant gives only '{namespace}'.
	f := newWRSNFixture(t, wrsnBaseObjects(
		false, wrsnSourceNS))

	_, err := f.reconcile(ctx)

	require.NoError(t, err)
	assert.Empty(t, f.compiledNames(), "a denied override must start no watch")

	rule := f.reloadRule(ctx, t)
	cond := wrsnCondition(t, rule, ConditionTypeSourceNamespaceAuthorized)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, WatchRuleReasonSourceNamespaceNotAllowed, cond.Reason)
	assert.Contains(t, cond.Message, "spec.allowedGitTargets[].sourceNamespaces",
		"the message must name the fix")
}

// TestReconcile_DeniedSourceNamespacePublishesTheFailedTrio pins the whole kstatus verdict a
// refusal produces: Failed, under the one reason an operator greps for.
func TestReconcile_DeniedSourceNamespacePublishesTheFailedTrio(t *testing.T) {
	ctx := context.Background()
	f := newWRSNFixture(t, wrsnBaseObjects(false, wrsnSourceNS))

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
		true, wrsnSourceNS))

	_, err := f.reconcile(ctx)
	require.NoError(t, err)

	compiled := f.store.SnapshotWatchRules()
	require.Len(t, compiled, 1)
	assert.Equal(t, []string{wrsnSourceNS}, compiled[0].ResourceRules[0].SourceNamespaces)
	assert.Equal(t, wrsnTenantNS, compiled[0].Source.Namespace)

	cond := wrsnCondition(t, f.reloadRule(ctx, t), ConditionTypeSourceNamespaceAuthorized)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, WatchRuleReasonSourceNamespaceAllowed, cond.Reason)
}

// TestReconcile_RevokedSourceNamespaceRemovesTheCompiledRuleAndReplans is the REVOCATION contract.
// A rule accepted and then denied by a withdrawn grant must have its compiled rule REMOVED and
// the watch manager replanned — and the removal must already have happened by the time the replan
// runs, because status is published only after that. A gate that reports without stopping is not a
// gate.
func TestReconcile_RevokedSourceNamespaceRemovesTheCompiledRuleAndReplans(t *testing.T) {
	ctx := context.Background()
	f := newWRSNFixture(t, wrsnBaseObjects(
		true, wrsnSourceNS))

	_, err := f.reconcile(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{wrsnRule}, f.compiledNames(), "precondition: the rule is compiled")

	// The platform admin narrows the grant to the tenant's own namespace.
	var provider configbutleraiv1alpha3.KubeProvider
	require.NoError(t, f.client.Get(ctx,
		k8stypes.NamespacedName{Namespace: wrsnProviderNS, Name: wrsnProvider}, &provider))
	provider.Spec.AllowedGitTargets[0].SourceNamespaces = []configbutleraiv1alpha3.ScopeNamespace{
		configbutleraiv1alpha3.NamespaceVariable}
	require.NoError(t, f.client.Update(ctx, &provider))

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

// A grant giving sourceNamespaces "*" puts NO further namespace policy in the way: the source
// credential's own RBAC is the bound, and this gate does not try to predict it.
func TestReconcile_WildcardGrantIsTheOnlyPolicy(t *testing.T) {
	ctx := context.Background()
	f := newWRSNFixture(t, wrsnBaseObjects(true, "some-namespace-nobody-declared"))

	_, err := f.reconcile(ctx)
	require.NoError(t, err)

	assert.Equal(t, []string{wrsnRule}, f.compiledNames(),
		"with \"*\" granted, no second allow-list stands between the rule and the watch")
	cond := wrsnCondition(t, f.reloadRule(ctx, t), ConditionTypeSourceNamespaceAuthorized)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, WatchRuleReasonSourceNamespaceAllowed, cond.Reason)
}

// TestReconcile_KubeProviderReadErrorRequeuesWithoutDenying: a transient apiserver failure must
// surface as an error the controller requeues on, and must NOT tear down an already-compiled rule.
func TestReconcile_KubeProviderReadErrorRequeuesWithoutDenying(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("etcdserver: request timed out")

	f := newWRSNFixture(t, wrsnBaseObjects(
		true, wrsnSourceNS))

	// Compile it once cleanly.
	_, err := f.reconcile(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{wrsnRule}, f.compiledNames())

	// Now make the KubeProvider read fail.
	f.reconciler.Client = fake.NewClientBuilder().
		WithScheme(scScheme(t)).
		WithObjects(wrsnBaseObjects(
			true, wrsnSourceNS)...).
		WithStatusSubresource(&configbutleraiv1alpha3.WatchRule{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(
				ctx context.Context, c client.WithWatch, key client.ObjectKey,
				obj client.Object, opts ...client.GetOption,
			) error {
				if _, ok := obj.(*configbutleraiv1alpha3.KubeProvider); ok {
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

// TestReconcile_MixedItemsCompileTheirOwnScopes is the point of moving the field onto the items,
// asserted through the real reconciler: one rule can follow one type in its own namespace and
// another in a different, admitted one, and the compiled rule carries both scopes independently.
func TestReconcile_MixedItemsCompileTheirOwnScopes(t *testing.T) {
	ctx := context.Background()
	f := newWRSNFixture(t, wrsnBaseObjects(
		true, "", wrsnSourceNS, configbutleraiv1alpha3.SourceNamespaceWildcard))

	_, err := f.reconcile(ctx)
	require.NoError(t, err)

	compiled := f.store.SnapshotWatchRules()
	require.Len(t, compiled, 1)
	require.Len(t, compiled[0].ResourceRules, 3)
	assert.Equal(t, []string{wrsnTenantNS}, compiled[0].ResourceRules[0].SourceNamespaces,
		"an omitted item resolves to the rule's own namespace")
	assert.Equal(t, []string{wrsnSourceNS}, compiled[0].ResourceRules[1].SourceNamespaces,
		"an explicit item resolves to exactly what it named")
	assert.Equal(t, []string{""}, compiled[0].ResourceRules[2].SourceNamespaces,
		`"*" is one cluster-wide list and watch, which is the empty namespace`)

	cond := wrsnCondition(t, f.reloadRule(ctx, t), ConditionTypeSourceNamespaceAuthorized)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, WatchRuleReasonSourceNamespaceAllowed, cond.Reason)
}

// TestReconcile_DeniedItemRefusesTheWholeRule is decision 5 at the reconciler: a denied explicit
// item is never trimmed away so the other items can run. Mirroring two of the three namespaces a
// rule asked for is worse than a loud failure — and the message must name the offending item.
func TestReconcile_DeniedItemRefusesTheWholeRule(t *testing.T) {
	ctx := context.Background()
	// Item 0 watches the rule's own namespace, which the grant gives; item 1 asks for one it does not.
	f := newWRSNFixture(t, wrsnBaseObjects(false, "", "tenant-zen"))

	_, err := f.reconcile(ctx)
	require.NoError(t, err)

	assert.Empty(t, f.compiledNames(),
		"one denied item stops the WHOLE rule; a partial mirror is worse than a loud failure")

	cond := wrsnCondition(t, f.reloadRule(ctx, t), ConditionTypeSourceNamespaceAuthorized)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, WatchRuleReasonSourceNamespaceNotAllowed, cond.Reason)
	assert.Contains(t, cond.Message, "spec.rules[1]", "the message names the failing item by index...")
	assert.Contains(t, cond.Message, "tenant-zen", "...and by what it asked for")
}

// "*" is refused outright while no grant gives sourceNamespaces "*", and the refusal names the field. It
// is the widest request in the API — every namespace the credential can read — so it must not be
// the one that slips past a provider granting nothing.
func TestReconcile_WildcardWithoutAWildcardGrantIsRefused(t *testing.T) {
	ctx := context.Background()
	f := newWRSNFixture(t, wrsnBaseObjects(false, configbutleraiv1alpha3.SourceNamespaceWildcard))

	_, err := f.reconcile(ctx)
	require.NoError(t, err)

	assert.Empty(t, f.compiledNames())
	rule := f.reloadRule(ctx, t)
	cond := wrsnCondition(t, rule, ConditionTypeSourceNamespaceAuthorized)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, WatchRuleReasonSourceNamespaceNotAllowed, cond.Reason)
	assert.Contains(t, cond.Message, "spec.allowedGitTargets[].sourceNamespaces")
	assert.Equal(t, metav1.ConditionTrue, wrsnCondition(t, rule, ConditionTypeStalled).Status,
		"a refusal is terminal: nothing this controller does will change the verdict")
}

// TestKubeProviderToWatchRules: a provider change requeues exactly the WatchRules whose GitTarget
// mirrors through THAT provider. The match is on the provider's namespace and name: a provider of
// the same name in another namespace is another provider.
func TestKubeProviderToWatchRules(t *testing.T) {
	sameName := wrsnGitTarget()
	sameName.Name = "same-name"
	sameName.Spec.KubeProviderRef = meta.NamespacedObjectReference{Name: wrsnProvider}
	sameNameRule := wrsnWatchRule(wrsnSourceNS)
	sameNameRule.Name = "same-name-rule"
	sameNameRule.Spec.GitTargetRef.Name = sameName.Name

	cl := fake.NewClientBuilder().WithScheme(scScheme(t)).
		WithObjects(wrsnGitTarget(), sameName, wrsnWatchRule(wrsnSourceNS), sameNameRule).Build()
	r := &WatchRuleReconciler{Client: cl}

	reqs := r.kubeProviderToWatchRules(context.Background(), wrsnKubeProvider(true))
	keys := make([]k8stypes.NamespacedName, 0, len(reqs))
	for _, req := range reqs {
		keys = append(keys, req.NamespacedName)
	}
	assert.Equal(t, []k8stypes.NamespacedName{{Namespace: wrsnTenantNS, Name: wrsnRule}}, keys)

	unrelated := wrsnKubeProvider(true)
	unrelated.Name = "unrelated"
	assert.Empty(t, r.kubeProviderToWatchRules(context.Background(), unrelated),
		"no GitTarget mirrors through it, so no WatchRule is requeued")
}
