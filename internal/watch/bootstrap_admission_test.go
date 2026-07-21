// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
)

// Bootstrap runs BEFORE the first reconcile on every restart, so it is the second call site that
// must apply the ClusterProvider admission gate. A helper only the reconciler used would leave the
// whole startup window unguarded — long enough to compile a rule and plan a stream for a GitTarget
// the provider never admitted.

const (
	bootRuleName     = "mirror-everything"
	bootTargetName   = "prod-mirror"
	bootTargetNS     = "team-a"
	bootProviderName = "prod-eu-1"
)

func bootGitTarget() *configv1alpha3.GitTarget {
	return &configv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: bootTargetName, Namespace: bootTargetNS},
		Spec: configv1alpha3.GitTargetSpec{
			ProviderRef:        configv1alpha3.GitProviderReference{Name: "git"},
			ClusterProviderRef: &configv1alpha3.ClusterProviderReference{Name: bootProviderName},
			Branch:             "main",
			Path:               "clusters/prod",
		},
	}
}

func bootGitProvider() *configv1alpha3.GitProvider {
	return &configv1alpha3.GitProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "git", Namespace: bootTargetNS},
	}
}

func bootClusterProvider(policy *configv1alpha3.NamespaceMatcher) *configv1alpha3.ClusterProvider {
	return &configv1alpha3.ClusterProvider{
		ObjectMeta: metav1.ObjectMeta{Name: bootProviderName},
		Spec:       configv1alpha3.ClusterProviderSpec{AllowedNamespaces: policy},
	}
}

func bootClusterWatchRule() *configv1alpha3.ClusterWatchRule {
	return &configv1alpha3.ClusterWatchRule{
		ObjectMeta: metav1.ObjectMeta{Name: bootRuleName},
		Spec: configv1alpha3.ClusterWatchRuleSpec{
			TargetRef: configv1alpha3.NamespacedTargetReference{
				Kind: "GitTarget", Name: bootTargetName, Namespace: bootTargetNS,
			},
			Rules: []configv1alpha3.ClusterResourceRule{{
				Resources: []string{"customresourcedefinitions"},
				APIGroups: []string{"apiextensions.k8s.io"},
			}},
		},
	}
}

// bootNamespacedClusterWatchRule is a STORED pre-release object: `scope: Namespaced` is rejected at
// admission from this release on, but etcd still holds objects written before it.
func bootNamespacedClusterWatchRule() *configv1alpha3.ClusterWatchRule {
	rule := bootClusterWatchRule()
	rule.Spec.Rules = []configv1alpha3.ClusterResourceRule{
		{Resources: []string{"customresourcedefinitions"}, APIGroups: []string{"apiextensions.k8s.io"}},
		{Resources: []string{"configmaps"}, Scope: configv1alpha3.ResourceScopeNamespaced},
	}
	return rule
}

func bootManager(t *testing.T, objects ...client.Object) *Manager {
	t.Helper()
	return &Manager{
		Client:    fake.NewClientBuilder().WithScheme(makeScheme(t)).WithObjects(objects...).Build(),
		Log:       logr.Discard(),
		RuleStore: rulestore.NewStore(),
	}
}

func bootCompiledNames(m *Manager) []string {
	names := []string{}
	for _, r := range m.RuleStore.SnapshotClusterWatchRules() {
		names = append(names, r.Source.Name)
	}
	return names
}

// TestBootstrapClusterWatchRule_RefusesUnauthorizedGitTargetNamespace is the bootstrap half of the
// direct-refusal test: the rule must not be seeded at all.
func TestBootstrapClusterWatchRule_RefusesUnauthorizedGitTargetNamespace(t *testing.T) {
	m := bootManager(t,
		bootGitTarget(), bootGitProvider(),
		bootClusterProvider(&configv1alpha3.NamespaceMatcher{Names: []string{"some-other-namespace"}}),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: bootTargetNS}},
	)

	err := m.bootstrapClusterWatchRule(context.Background(), *bootClusterWatchRule())

	require.Error(t, err, "an unadmitted rule must be reported, not silently seeded")
	assert.Contains(t, err.Error(), "may not compile against GitTarget")
	assert.Empty(t, bootCompiledNames(m), "a refused rule must never reach the store")
}

// TestBootstrapClusterWatchRule_RefusesMissingClusterProvider covers the hard-gate half: an
// undeclared provider is not an implicit allow at startup either.
func TestBootstrapClusterWatchRule_RefusesMissingClusterProvider(t *testing.T) {
	m := bootManager(t,
		bootGitTarget(), bootGitProvider(),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: bootTargetNS}},
	)

	err := m.bootstrapClusterWatchRule(context.Background(), *bootClusterWatchRule())

	require.Error(t, err)
	assert.Empty(t, bootCompiledNames(m))
}

// TestBootstrapClusterWatchRule_SeedsAdmittedRule is the regression guard: the admitted startup
// path is unchanged, and the compiled rule still carries its resolved GitTarget/GitProvider values.
func TestBootstrapClusterWatchRule_SeedsAdmittedRule(t *testing.T) {
	m := bootManager(t,
		bootGitTarget(), bootGitProvider(),
		bootClusterProvider(&configv1alpha3.NamespaceMatcher{Names: []string{bootTargetNS}}),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: bootTargetNS}},
	)

	require.NoError(t, m.bootstrapClusterWatchRule(context.Background(), *bootClusterWatchRule()))

	compiled := m.RuleStore.SnapshotClusterWatchRules()
	require.Len(t, compiled, 1)
	assert.Equal(t, bootRuleName, compiled[0].Source.Name)
	assert.Equal(t, bootTargetName, compiled[0].GitTargetRef)
	assert.Equal(t, bootTargetNS, compiled[0].GitTargetNamespace)
	assert.Equal(t, "main", compiled[0].Branch)
	assert.Equal(t, "clusters/prod", compiled[0].Path)
}

// TestBootstrapRuleStore_SkipsUnauthorizedRuleButStillReady proves the denial is contained: one
// refused rule must not abort startup or block the admitted rules beside it, and the store must
// still be marked ready — otherwise a single unauthorized rule would wedge the whole data plane.
func TestBootstrapRuleStore_SkipsUnauthorizedRuleButStillReady(t *testing.T) {
	admittedTarget := &configv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "ok-mirror", Namespace: "team-ok"},
		Spec: configv1alpha3.GitTargetSpec{
			ProviderRef:        configv1alpha3.GitProviderReference{Name: "git"},
			ClusterProviderRef: &configv1alpha3.ClusterProviderReference{Name: bootProviderName},
			Branch:             "main",
			Path:               "clusters/ok",
		},
	}
	admittedRule := bootClusterWatchRule()
	admittedRule.Name = "admitted-rule"
	admittedRule.Spec.TargetRef.Name = "ok-mirror"
	admittedRule.Spec.TargetRef.Namespace = "team-ok"

	m := bootManager(t,
		bootGitTarget(), bootGitProvider(), bootClusterWatchRule(),
		admittedTarget,
		&configv1alpha3.GitProvider{ObjectMeta: metav1.ObjectMeta{Name: "git", Namespace: "team-ok"}},
		admittedRule,
		// Admits team-ok only, so the team-a rule is refused.
		bootClusterProvider(&configv1alpha3.NamespaceMatcher{Names: []string{"team-ok"}}),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: bootTargetNS}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-ok"}},
	)

	require.NoError(t, m.bootstrapRuleStore(context.Background(), logr.Discard()),
		"one refused rule must not abort startup")

	assert.Equal(t, []string{"admitted-rule"}, bootCompiledNames(m),
		"only the admitted rule may be seeded")
	assert.True(t, m.RuleStore.IsReady(),
		"the store must still be marked ready: a refused rule is a refusal, not a startup failure")
}

// TestBootstrap_PreExistingNamespacedClusterRuleIsRefused is THE cluster-scope-only test.
//
// A ClusterWatchRule stored with `scope: Namespaced` before this release keeps that value in etcd,
// and bootstrap seeds the store BEFORE the first reconcile can publish any status. So the refusal
// has to live in the shared compile path: a reconciler-only check would let every restart open a
// cluster-wide namespaced watch for the whole startup window. This asserts the state at the moment
// MarkReady() returns — the only moment that proves it.
func TestBootstrap_PreExistingNamespacedClusterRuleIsRefused(t *testing.T) {
	m := bootManager(t,
		bootGitTarget(), bootGitProvider(),
		bootClusterProvider(&configv1alpha3.NamespaceMatcher{Names: []string{bootTargetNS}}),
		bootNamespacedClusterWatchRule(),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: bootTargetNS}},
	)

	require.NoError(t, m.bootstrapRuleStore(context.Background(), logr.Discard()),
		"a refused rule is a refusal, not a startup failure")

	assert.Empty(t, bootCompiledNames(m),
		"a stored namespaced ClusterWatchRule must compile NO stream before status can be published")
	assert.True(t, m.RuleStore.IsReady())
}

// TestBootstrapClusterWatchRule_WildcardStillResolvesItsClusterScopedTypes: the refusal keys on the
// STORED scope, not on what the selector happens to resolve. `resources: ["*"]` legitimately
// resolves cluster-scoped records — inferring the refusal from the resolution would break exactly
// the rule that the restart fixture exists to protect.
func TestBootstrapClusterWatchRule_WildcardStillResolvesItsClusterScopedTypes(t *testing.T) {
	rule := bootClusterWatchRule()
	rule.Spec.Rules = []configv1alpha3.ClusterResourceRule{{
		Resources: []string{"*"}, APIVersions: []string{"*"},
	}}

	m := bootManager(t,
		bootGitTarget(), bootGitProvider(),
		bootClusterProvider(&configv1alpha3.NamespaceMatcher{Names: []string{bootTargetNS}}),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: bootTargetNS}},
	)

	require.NoError(t, m.bootstrapClusterWatchRule(context.Background(), *rule))

	assert.Equal(t, []string{bootRuleName}, bootCompiledNames(m),
		"a wildcard selector is not itself a namespaced scope declaration")
}

// TestCompileClusterWatchRule_RefusalRemovesAnAlreadyCompiledRule is the REVOCATION contract for the
// cluster kind: a rule accepted earlier and then refused must have its compiled rule REMOVED, not
// merely reported unready.
func TestCompileClusterWatchRule_RefusalRemovesAnAlreadyCompiledRule(t *testing.T) {
	ctx := context.Background()
	m := bootManager(t,
		bootGitTarget(), bootGitProvider(),
		bootClusterProvider(&configv1alpha3.NamespaceMatcher{Names: []string{bootTargetNS}}),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: bootTargetNS}},
	)

	decision, err := CompileClusterWatchRule(
		ctx, m.Client, m.RuleStore, *bootClusterWatchRule(), *bootGitTarget(), *bootGitProvider())
	require.NoError(t, err)
	require.True(t, decision.Admitted)
	require.Len(t, bootCompiledNames(m), 1, "precondition: the rule is compiled")

	// Somebody re-applies the pre-release manifest (or an old object is re-observed).
	decision, err = CompileClusterWatchRule(
		ctx, m.Client, m.RuleStore, *bootNamespacedClusterWatchRule(), *bootGitTarget(), *bootGitProvider())

	require.NoError(t, err)
	assert.False(t, decision.Admitted)
	assert.Equal(t, ClusterWatchRuleReasonScopeNotSupported, decision.Reason)
	assert.Contains(t, decision.Message, "rules[].sourceNamespace",
		"the refusal must name the replacement, because the migration is cross-kind")
	assert.Empty(t, bootCompiledNames(m),
		"a refused rule must be removed from the store, not left running with a bad condition")
}
