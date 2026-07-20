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
	"github.com/ConfigButler/gitops-reverser/internal/authz"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
	internaltypes "github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/watch"
)

// cwaWatchManager is a WatchManagerInterface double that records replans. onReconcile lets a test
// observe the world at the exact moment the data plane is replanned, which is how the
// stop-before-status ordering contract is asserted.
type cwaWatchManager struct {
	replans     int
	replanErr   error
	onReconcile func()
}

func (m *cwaWatchManager) ReconcileForRuleChange(context.Context) error {
	m.replans++
	if m.onReconcile != nil {
		m.onReconcile()
	}
	return m.replanErr
}

func (m *cwaWatchManager) ResolveWatchRuleResources(
	context.Context, configbutleraiv1alpha3.WatchRule,
) (bool, string) {
	return true, "resolved"
}

func (m *cwaWatchManager) ResolveClusterWatchRuleResources(
	context.Context, configbutleraiv1alpha3.ClusterWatchRule,
) (bool, string) {
	return true, "resolved"
}

func (m *cwaWatchManager) StreamSummaryForGitTarget(internaltypes.ResourceReference) watch.StreamSummary {
	return watch.StreamSummary{}
}

func (m *cwaWatchManager) StreamSummaryForWatchRule(
	configbutleraiv1alpha3.WatchRule,
) watch.StreamSummary {
	return cwaRunningSummary()
}

func (m *cwaWatchManager) StreamSummaryForClusterWatchRule(
	configbutleraiv1alpha3.ClusterWatchRule,
) watch.StreamSummary {
	return cwaRunningSummary()
}

func cwaRunningSummary() watch.StreamSummary {
	return watch.StreamSummary{
		Total: 1, Ready: 1, Reason: "Streaming", Message: "1/1 streams running",
	}
}

// --- fixtures ---------------------------------------------------------------------------------

const (
	cwaRuleName     = "mirror-everything"
	cwaTargetName   = "prod-mirror"
	cwaTargetNS     = "team-a"
	cwaProviderName = "prod-eu-1"
)

func cwaGitTarget() *configbutleraiv1alpha3.GitTarget {
	return &configbutleraiv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: cwaTargetName, Namespace: cwaTargetNS},
		Spec: configbutleraiv1alpha3.GitTargetSpec{
			ProviderRef:        configbutleraiv1alpha3.GitProviderReference{Name: "git"},
			ClusterProviderRef: &configbutleraiv1alpha3.ClusterProviderReference{Name: cwaProviderName},
			Branch:             "main",
			Path:               "clusters/prod",
		},
	}
}

func cwaGitProvider() *configbutleraiv1alpha3.GitProvider {
	return &configbutleraiv1alpha3.GitProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "git", Namespace: cwaTargetNS},
	}
}

func cwaClusterProvider(policy *configbutleraiv1alpha3.AllowedNamespaces) *configbutleraiv1alpha3.ClusterProvider {
	return &configbutleraiv1alpha3.ClusterProvider{
		ObjectMeta: metav1.ObjectMeta{Name: cwaProviderName},
		Spec:       configbutleraiv1alpha3.ClusterProviderSpec{AllowedNamespaces: policy},
	}
}

func cwaClusterWatchRule() *configbutleraiv1alpha3.ClusterWatchRule {
	return &configbutleraiv1alpha3.ClusterWatchRule{
		ObjectMeta: metav1.ObjectMeta{Name: cwaRuleName, Generation: 1},
		Spec: configbutleraiv1alpha3.ClusterWatchRuleSpec{
			TargetRef: configbutleraiv1alpha3.NamespacedTargetReference{
				Kind: "GitTarget", Name: cwaTargetName, Namespace: cwaTargetNS,
			},
			Rules: []configbutleraiv1alpha3.ClusterResourceRule{{
				Scope:     configbutleraiv1alpha3.ResourceScopeNamespaced,
				Resources: []string{"configmaps"},
			}},
		},
	}
}

// cwaAdmitting is the policy that admits cwaTargetNS; cwaDenying admits some other namespace.
func cwaAdmitting() *configbutleraiv1alpha3.AllowedNamespaces {
	return &configbutleraiv1alpha3.AllowedNamespaces{Names: []string{cwaTargetNS}}
}

func cwaDenying() *configbutleraiv1alpha3.AllowedNamespaces {
	return &configbutleraiv1alpha3.AllowedNamespaces{Names: []string{"some-other-namespace"}}
}

type cwaFixture struct {
	reconciler *ClusterWatchRuleReconciler
	store      *rulestore.RuleStore
	wm         *cwaWatchManager
	client     client.Client
}

func newCWAFixture(
	t *testing.T,
	objects []client.Object,
	interceptors interceptor.Funcs,
) *cwaFixture {
	t.Helper()

	cl := fake.NewClientBuilder().
		WithScheme(scScheme(t)).
		WithObjects(objects...).
		WithStatusSubresource(&configbutleraiv1alpha3.ClusterWatchRule{}).
		WithInterceptorFuncs(interceptors).
		Build()

	store := rulestore.NewStore()
	wm := &cwaWatchManager{}

	return &cwaFixture{
		reconciler: &ClusterWatchRuleReconciler{
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

func (f *cwaFixture) reconcile(ctx context.Context) (ctrl.Result, error) {
	return f.reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: k8stypes.NamespacedName{Name: cwaRuleName},
	})
}

func (f *cwaFixture) compiledNames() []string {
	names := []string{}
	for _, r := range f.store.SnapshotClusterWatchRules() {
		names = append(names, r.Source.Name)
	}
	return names
}

func (f *cwaFixture) reloadRule(ctx context.Context, t *testing.T) *configbutleraiv1alpha3.ClusterWatchRule {
	t.Helper()
	var rule configbutleraiv1alpha3.ClusterWatchRule
	require.NoError(t, f.client.Get(ctx, k8stypes.NamespacedName{Name: cwaRuleName}, &rule))
	return &rule
}

// assertTerminalRefusal pins the whole published verdict: the kstatus trio plus the two projected
// conditions, all under the one reason an operator greps for.
func assertTerminalRefusal(t *testing.T, rule *configbutleraiv1alpha3.ClusterWatchRule) {
	t.Helper()

	for _, want := range []struct {
		conditionType string
		status        metav1.ConditionStatus
	}{
		{ConditionTypeGitTargetReady, metav1.ConditionFalse},
		{ConditionTypeStreamsRunning, metav1.ConditionFalse},
		{ConditionTypeReady, metav1.ConditionFalse},
		{ConditionTypeReconciling, metav1.ConditionFalse},
		{ConditionTypeStalled, metav1.ConditionTrue},
	} {
		cond := apimeta.FindStatusCondition(rule.Status.Conditions, want.conditionType)
		require.NotNil(t, cond, "condition %s must be published", want.conditionType)
		assert.Equal(t, want.status, cond.Status, "condition %s status", want.conditionType)
		assert.Equal(t, ClusterWatchRuleReasonGitTargetNamespaceNotAuthorized, cond.Reason,
			"condition %s reason", want.conditionType)
	}
}

// --- tests ------------------------------------------------------------------------------------

// TestReconcile_ClusterWatchRuleRefusedWhenTargetNamespaceUnauthorized is the direct-refusal case:
// a ClusterWatchRule may not compile against a GitTarget whose namespace the ClusterProvider never
// admitted, even though the rule itself is cluster-scoped and names the target freely.
func TestReconcile_ClusterWatchRuleRefusedWhenTargetNamespaceUnauthorized(t *testing.T) {
	ctx := context.Background()
	f := newCWAFixture(t, []client.Object{
		cwaClusterWatchRule(), cwaGitTarget(), cwaGitProvider(),
		cwaClusterProvider(cwaDenying()),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cwaTargetNS}},
	}, interceptor.Funcs{})

	_, err := f.reconcile(ctx)
	require.NoError(t, err, "a refusal is terminal, not an error to retry")

	assert.Empty(t, f.compiledNames(), "a refused rule must leave no compiled rule behind")
	assert.Positive(t, f.wm.replans, "the watch manager must be replanned so no stream survives")
	assertTerminalRefusal(t, f.reloadRule(ctx, t))
}

// TestReconcile_ClusterWatchRuleRefusedWhenClusterProviderMissing covers the other half of the
// shared gate: an undeclared provider is a hard denial, so an operator cannot sidestep
// allowedNamespaces by simply never creating the provider.
func TestReconcile_ClusterWatchRuleRefusedWhenClusterProviderMissing(t *testing.T) {
	ctx := context.Background()
	f := newCWAFixture(t, []client.Object{
		cwaClusterWatchRule(), cwaGitTarget(), cwaGitProvider(),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cwaTargetNS}},
	}, interceptor.Funcs{})

	_, err := f.reconcile(ctx)
	require.NoError(t, err)

	assert.Empty(t, f.compiledNames())
	rule := f.reloadRule(ctx, t)
	assertTerminalRefusal(t, rule)
	cond := apimeta.FindStatusCondition(rule.Status.Conditions, ConditionTypeGitTargetReady)
	assert.Contains(t, cond.Message, "was not found",
		"the message must name which of the two provider-side causes it was")
}

// TestReconcile_RefusalStopsDataPlaneBeforePublishingStatus is the ordering guard the design calls
// for: a gate that only writes a condition is not a gate. It fails a status-only implementation and
// it fails an implementation that publishes the refusal while the stream is still planned.
func TestReconcile_RefusalStopsDataPlaneBeforePublishingStatus(t *testing.T) {
	ctx := context.Background()

	var (
		replannedAt           int
		statusWrittenAt       int
		seq                   int
		compiledAtStatusWrite int
	)

	var f *cwaFixture
	f = newCWAFixture(t, []client.Object{
		cwaClusterWatchRule(), cwaGitTarget(), cwaGitProvider(),
		cwaClusterProvider(cwaDenying()),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cwaTargetNS}},
	}, interceptor.Funcs{
		SubResourceUpdate: func(
			ctx context.Context, c client.Client, _ string,
			obj client.Object, opts ...client.SubResourceUpdateOption,
		) error {
			seq++
			statusWrittenAt = seq
			compiledAtStatusWrite = len(f.store.SnapshotClusterWatchRules())
			return c.Status().Update(ctx, obj, opts...)
		},
	})

	// Pre-seed the store as an earlier, admitted reconcile would have left it, so "no compiled
	// rule at the end" cannot pass vacuously.
	f.store.AddOrUpdateClusterWatchRule(
		*cwaClusterWatchRule(), cwaTargetName, cwaTargetNS, "git", cwaTargetNS, "main", "clusters/prod")
	require.Len(t, f.compiledNames(), 1, "precondition: the rule starts out compiled")

	f.wm.onReconcile = func() {
		seq++
		replannedAt = seq
	}

	_, err := f.reconcile(ctx)
	require.NoError(t, err)

	require.Positive(t, replannedAt, "the watch manager must be replanned on refusal")
	require.Positive(t, statusWrittenAt, "the refusal must be published")
	assert.Less(t, replannedAt, statusWrittenAt,
		"the data plane must be stopped BEFORE the refusal is published")
	assert.Zero(t, compiledAtStatusWrite,
		"the compiled rule must already be gone when the terminal condition becomes observable")
}

// TestReconcile_AdmittedClusterWatchRuleStillCompiles is the regression guard for a helper that is
// accidentally too strict: the admitted path must be untouched.
func TestReconcile_AdmittedClusterWatchRuleStillCompiles(t *testing.T) {
	ctx := context.Background()
	f := newCWAFixture(t, []client.Object{
		cwaClusterWatchRule(), cwaGitTarget(), cwaGitProvider(),
		cwaClusterProvider(cwaAdmitting()),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cwaTargetNS}},
	}, interceptor.Funcs{})

	_, err := f.reconcile(ctx)
	require.NoError(t, err)

	assert.Equal(t, []string{cwaRuleName}, f.compiledNames(), "an admitted rule must compile")

	rule := f.reloadRule(ctx, t)
	stalled := apimeta.FindStatusCondition(rule.Status.Conditions, ConditionTypeStalled)
	require.NotNil(t, stalled)
	assert.Equal(t, metav1.ConditionFalse, stalled.Status, "an admitted rule must not be stalled")
	assert.NotEqual(t, ClusterWatchRuleReasonGitTargetNamespaceNotAuthorized, stalled.Reason)
}

// TestReconcile_AdmittedBySelectorOnNamespaceLabels proves the gate reads the live Namespace's
// labels, not just the names list — the path the Namespace watch exists to re-trigger.
func TestReconcile_AdmittedBySelectorOnNamespaceLabels(t *testing.T) {
	ctx := context.Background()
	f := newCWAFixture(t, []client.Object{
		cwaClusterWatchRule(), cwaGitTarget(), cwaGitProvider(),
		cwaClusterProvider(&configbutleraiv1alpha3.AllowedNamespaces{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"mirror": "yes"}},
		}),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: cwaTargetNS, Labels: map[string]string{"mirror": "yes"},
		}},
	}, interceptor.Funcs{})

	_, err := f.reconcile(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{cwaRuleName}, f.compiledNames())
}

// TestReconcile_RevocationRemovesCompiledRule is the revocation case: a rule that was admitted and
// running must be torn down when the provider's allowedNamespaces stops admitting its target's
// namespace. Same terminal status as an initial denial.
func TestReconcile_RevocationRemovesCompiledRule(t *testing.T) {
	ctx := context.Background()
	f := newCWAFixture(t, []client.Object{
		cwaClusterWatchRule(), cwaGitTarget(), cwaGitProvider(),
		cwaClusterProvider(cwaAdmitting()),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cwaTargetNS}},
	}, interceptor.Funcs{})

	// Round 1: admitted and compiled.
	_, err := f.reconcile(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{cwaRuleName}, f.compiledNames(), "precondition: the rule is running")
	replansAfterAdmission := f.wm.replans

	// Revoke: the namespace leaves allowedNamespaces.
	var provider configbutleraiv1alpha3.ClusterProvider
	require.NoError(t, f.client.Get(ctx, k8stypes.NamespacedName{Name: cwaProviderName}, &provider))
	provider.Spec.AllowedNamespaces = cwaDenying()
	require.NoError(t, f.client.Update(ctx, &provider))

	// Round 2: the same reconcile now refuses.
	_, err = f.reconcile(ctx)
	require.NoError(t, err)

	assert.Empty(t, f.compiledNames(), "revocation must remove the compiled rule")
	assert.Greater(t, f.wm.replans, replansAfterAdmission,
		"revocation must replan the watch manager so the stream stops")
	assertTerminalRefusal(t, f.reloadRule(ctx, t))
}

// TestReconcile_AdmissionReadErrorRequeuesAndKeepsCompiledRule pins the fail-safe direction. A
// transient apiserver failure is NOT a denial: tearing the stream down on it would turn every
// blip into an outage, so the rule stays compiled and the error requeues.
func TestReconcile_AdmissionReadErrorRequeuesAndKeepsCompiledRule(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("apiserver unavailable")

	f := newCWAFixture(t, []client.Object{
		cwaClusterWatchRule(), cwaGitTarget(), cwaGitProvider(),
		cwaClusterProvider(cwaAdmitting()),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cwaTargetNS}},
	}, interceptor.Funcs{
		Get: func(
			ctx context.Context, c client.WithWatch, key client.ObjectKey,
			obj client.Object, opts ...client.GetOption,
		) error {
			if _, ok := obj.(*configbutleraiv1alpha3.ClusterProvider); ok {
				return boom
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})

	f.store.AddOrUpdateClusterWatchRule(
		*cwaClusterWatchRule(), cwaTargetName, cwaTargetNS, "git", cwaTargetNS, "main", "clusters/prod")

	_, err := f.reconcile(ctx)

	require.Error(t, err, "a read failure must requeue, not silently pass or silently refuse")
	require.ErrorIs(t, err, boom)
	assert.Equal(t, []string{cwaRuleName}, f.compiledNames(),
		"a transient read failure must NOT tear down a running rule")
}

// --- mapper tests -----------------------------------------------------------------------------

// TestClusterProviderToClusterWatchRules proves a provider policy change requeues ClusterWatchRules
// and not only GitTargets. Without it a revocation converges only on the ~10m periodic reconcile,
// because the GitTarget's resulting status flip is dropped by GenerationChangedPredicate.
func TestClusterProviderToClusterWatchRules(t *testing.T) {
	mirrored := cwaGitTarget()
	elsewhere := &configbutleraiv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "other-mirror", Namespace: "team-b"},
		Spec: configbutleraiv1alpha3.GitTargetSpec{
			ClusterProviderRef: &configbutleraiv1alpha3.ClusterProviderReference{Name: "prod-us-1"},
		},
	}

	ruleFor := func(name, targetName, targetNS string) *configbutleraiv1alpha3.ClusterWatchRule {
		r := cwaClusterWatchRule()
		r.Name = name
		r.Spec.TargetRef.Name = targetName
		r.Spec.TargetRef.Namespace = targetNS
		return r
	}

	cl := fake.NewClientBuilder().WithScheme(scScheme(t)).WithObjects(
		mirrored, elsewhere,
		ruleFor("rule-affected", cwaTargetName, cwaTargetNS),
		ruleFor("rule-other-provider", "other-mirror", "team-b"),
		ruleFor("rule-dangling", "no-such-target", cwaTargetNS),
	).Build()
	r := &ClusterWatchRuleReconciler{Client: cl}

	reqs := r.clusterProviderToClusterWatchRules(context.Background(), cwaClusterProvider(cwaAdmitting()))

	names := []string{}
	for _, req := range reqs {
		assert.Empty(t, req.Namespace, "ClusterWatchRule is cluster-scoped: requests carry a name only")
		names = append(names, req.Name)
	}
	assert.ElementsMatch(t, []string{"rule-affected"}, names,
		"only rules whose target mirrors through THIS provider are requeued")
}

// TestNamespaceToClusterWatchRules covers the selector half: a label change on a GitTarget's
// namespace can grant or revoke every ClusterWatchRule pointing at a target in it.
func TestNamespaceToClusterWatchRules(t *testing.T) {
	inNS := cwaGitTarget()
	otherNS := &configbutleraiv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "other-mirror", Namespace: "team-b"},
	}

	ruleFor := func(name, targetName, targetNS string) *configbutleraiv1alpha3.ClusterWatchRule {
		r := cwaClusterWatchRule()
		r.Name = name
		r.Spec.TargetRef.Name = targetName
		r.Spec.TargetRef.Namespace = targetNS
		return r
	}

	cl := fake.NewClientBuilder().WithScheme(scScheme(t)).WithObjects(
		inNS, otherNS,
		ruleFor("rule-in-ns", cwaTargetName, cwaTargetNS),
		ruleFor("rule-other-ns", "other-mirror", "team-b"),
	).Build()
	r := &ClusterWatchRuleReconciler{Client: cl}

	reqs := r.namespaceToClusterWatchRules(context.Background(),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cwaTargetNS}})

	names := []string{}
	for _, req := range reqs {
		names = append(names, req.Name)
	}
	assert.ElementsMatch(t, []string{"rule-in-ns"}, names)
}

// TestClusterWatchRulesTargeting_NoMatchesEnqueuesNothing keeps the mappers from degenerating into
// "re-enqueue everything", which would hide a broken match behind a passing convergence test.
func TestClusterWatchRulesTargeting_NoMatchesEnqueuesNothing(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(scScheme(t)).WithObjects(cwaClusterWatchRule()).Build()
	r := &ClusterWatchRuleReconciler{Client: cl}

	assert.Empty(t, r.clusterProviderToClusterWatchRules(context.Background(),
		cwaClusterProvider(cwaAdmitting())), "no GitTargets mirror through this provider")
	assert.Empty(t, r.namespaceToClusterWatchRules(context.Background(),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "empty-ns"}}))
}

// TestRefusalReasonIsStableForOperators pins the reason string: it is a documented, greppable
// contract, so renaming it must break a test rather than silently break an operator's alert.
func TestRefusalReasonIsStableForOperators(t *testing.T) {
	assert.Equal(t, "GitTargetNamespaceNotAuthorized",
		ClusterWatchRuleReasonGitTargetNamespaceNotAuthorized)
	assert.Equal(t, "NamespaceNotAuthorized", authz.ReasonNamespaceNotAuthorized)
	assert.Equal(t, "ClusterProviderNotFound", authz.ReasonClusterProviderNotFound)
}
