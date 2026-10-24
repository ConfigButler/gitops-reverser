// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"testing"

	meta "github.com/fluxcd/pkg/apis/meta"
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
// must apply the KubeProvider admission gate. A helper only the reconciler used would leave the
// whole startup window unguarded — long enough to compile a rule and plan a stream for a GitTarget
// the provider never admitted.

const (
	bootRuleName     = "mirror-everything"
	bootTargetName   = "prod-mirror"
	bootTargetNS     = "team-a"
	bootProviderName = "prod-eu-1"
	// bootProviderNS is the KubeProvider's namespace, not the GitTarget's, so whether it admits the
	// GitTarget is decided by a Names grant in its spec.allowedGitTargets.
	bootProviderNS = "platform"
)

func bootGitTarget() *configv1alpha3.GitTarget {
	return &configv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: bootTargetName, Namespace: bootTargetNS},
		Spec: configv1alpha3.GitTargetSpec{
			GitBranchRef:    meta.NamespacedObjectReference{Name: gitBranchNameForTest("git", "main")},
			KubeProviderRef: meta.NamespacedObjectReference{Namespace: bootProviderNS, Name: bootProviderName},
			Path:            "clusters/prod",
		},
	}
}

func bootGitProvider() *configv1alpha3.GitProvider {
	return &configv1alpha3.GitProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "git", Namespace: bootTargetNS},
	}
}

// mirrorGrants admits the named namespaces to mirror their own namespace and every cluster-scoped
// resource, the reach a provider admitting them had before grants were scoped.
func mirrorGrants(namespaces ...string) []configv1alpha3.KubeProviderGitTargetGrant {
	names := make([]configv1alpha3.GrantNamespace, 0, len(namespaces))
	for _, ns := range namespaces {
		names = append(names, configv1alpha3.GrantNamespace(ns))
	}
	return []configv1alpha3.KubeProviderGitTargetGrant{{
		ConsumerNamespaces: configv1alpha3.ConsumerNamespaces{From: configv1alpha3.GrantFromNames, Names: names},
		SourceNamespaces:   []configv1alpha3.ScopeNamespace{configv1alpha3.ConsumerNamespaceVariable},
		ClusterResources:   []configv1alpha3.ClusterResourceIdentity{"*"},
	}}
}

func bootKubeProvider(policy []configv1alpha3.KubeProviderGitTargetGrant) *configv1alpha3.KubeProvider {
	return &configv1alpha3.KubeProvider{
		ObjectMeta: metav1.ObjectMeta{Namespace: bootProviderNS, Name: bootProviderName},
		Spec: configv1alpha3.KubeProviderSpec{
			SecretRef:         &meta.SecretKeyReference{Name: "prod-eu-1-kubeconfig"},
			AllowedGitTargets: policy,
		},
	}
}

func bootClusterWatchRule() *configv1alpha3.ClusterWatchRule {
	return &configv1alpha3.ClusterWatchRule{
		ObjectMeta: metav1.ObjectMeta{Name: bootRuleName},
		Spec: configv1alpha3.ClusterWatchRuleSpec{
			GitTargetRef: meta.NamespacedObjectReference{Name: bootTargetName, Namespace: bootTargetNS},
			Rules: []configv1alpha3.ClusterResourceRule{{
				Resources: []string{"customresourcedefinitions"},
				APIGroups: []string{"apiextensions.k8s.io"},
			}},
		},
	}
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
		bootKubeProvider(mirrorGrants("some-other-namespace")),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: bootTargetNS}},
	)

	err := m.bootstrapClusterWatchRule(context.Background(), *bootClusterWatchRule())

	require.Error(t, err, "an unadmitted rule must be reported, not silently seeded")
	assert.Contains(t, err.Error(), "may not compile against GitTarget")
	assert.Empty(t, bootCompiledNames(m), "a refused rule must never reach the store")
}

// TestBootstrapClusterWatchRule_RefusesMissingKubeProvider covers the hard-gate half: an
// undeclared provider is not an implicit allow at startup either.
func TestBootstrapClusterWatchRule_RefusesMissingKubeProvider(t *testing.T) {
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
		bootKubeProvider(mirrorGrants(bootTargetNS)),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: bootTargetNS}},
	)

	require.NoError(t, m.bootstrapClusterWatchRule(context.Background(), *bootClusterWatchRule()))

	compiled := m.RuleStore.SnapshotClusterWatchRules()
	require.Len(t, compiled, 1)
	assert.Equal(t, bootRuleName, compiled[0].Source.Name)
	assert.Equal(t, bootTargetName, compiled[0].GitTargetRef)
	assert.Equal(t, bootTargetNS, compiled[0].GitTargetNamespace)
	assert.Equal(t, "clusters/prod", compiled[0].Path)
}

// TestBootstrapRuleStore_SkipsUnauthorizedRuleButStillReady proves the denial is contained: one
// refused rule must not abort startup or block the admitted rules beside it, and the store must
// still be marked ready — otherwise a single unauthorized rule would wedge the whole data plane.
func TestBootstrapRuleStore_SkipsUnauthorizedRuleButStillReady(t *testing.T) {
	admittedTarget := &configv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "ok-mirror", Namespace: "team-ok"},
		Spec: configv1alpha3.GitTargetSpec{
			GitBranchRef:    meta.NamespacedObjectReference{Name: gitBranchNameForTest("git", "main")},
			KubeProviderRef: meta.NamespacedObjectReference{Namespace: bootProviderNS, Name: bootProviderName},
			Path:            "clusters/ok",
		},
	}
	admittedRule := bootClusterWatchRule()
	admittedRule.Name = "admitted-rule"
	admittedRule.Spec.GitTargetRef.Name = "ok-mirror"
	admittedRule.Spec.GitTargetRef.Namespace = "team-ok"

	m := bootManager(t,
		bootGitTarget(), bootGitProvider(), bootClusterWatchRule(),
		admittedTarget,
		&configv1alpha3.GitProvider{ObjectMeta: metav1.ObjectMeta{Name: "git", Namespace: "team-ok"}},
		admittedRule,
		// Admits team-ok only, so the team-a rule is refused.
		bootKubeProvider(mirrorGrants("team-ok")),
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

// TestBootstrapRuleStore_SeedsNoClusterWatchRuleAnotherInstanceServes: under --watch-namespaces a
// ClusterWatchRule naming a GitTarget outside the served namespaces belongs to another instance,
// and is not seeded even though its provider would admit it.
func TestBootstrapRuleStore_SeedsNoClusterWatchRuleAnotherInstanceServes(t *testing.T) {
	servedTarget := bootGitTarget()
	servedTarget.Name, servedTarget.Namespace = "ok-mirror", "team-ok"
	servedRule := bootClusterWatchRule()
	servedRule.Name = "served-rule"
	servedRule.Spec.GitTargetRef = meta.NamespacedObjectReference{Name: "ok-mirror", Namespace: "team-ok"}

	m := bootManager(t,
		bootGitTarget(), bootGitProvider(), bootClusterWatchRule(),
		servedTarget,
		&configv1alpha3.GitProvider{ObjectMeta: metav1.ObjectMeta{Name: "git", Namespace: "team-ok"}},
		servedRule,
		bootKubeProvider(mirrorGrants(bootTargetNS, "team-ok")),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: bootTargetNS}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-ok"}},
	)
	var err error
	m.ServedNamespaces, err = ParseServedNamespaces("team-ok," + bootProviderNS)
	require.NoError(t, err)

	require.NoError(t, m.bootstrapRuleStore(context.Background(), logr.Discard()))

	assert.Equal(t, []string{"served-rule"}, bootCompiledNames(m))
	assert.True(t, m.RuleStore.IsReady())
}

// TestBootstrapClusterWatchRule_WildcardStillResolvesItsClusterScopedTypes pins what a wildcard
// selector means on the cluster kind. Which scope a rule watches is decided by which KIND you
// write, never by a field, so `resources: ["*"]` on a ClusterWatchRule resolves the cluster-scoped
// records and nothing else.
func TestBootstrapClusterWatchRule_WildcardStillResolvesItsClusterScopedTypes(t *testing.T) {
	rule := bootClusterWatchRule()
	rule.Spec.Rules = []configv1alpha3.ClusterResourceRule{{
		Resources: []string{"*"}, APIVersions: []string{"*"},
	}}

	m := bootManager(t,
		bootGitTarget(), bootGitProvider(),
		bootKubeProvider(mirrorGrants(bootTargetNS)),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: bootTargetNS}},
	)

	require.NoError(t, m.bootstrapClusterWatchRule(context.Background(), *rule))

	assert.Equal(t, []string{bootRuleName}, bootCompiledNames(m),
		"a wildcard selector is not itself a namespaced scope declaration")
}

// TestCompileClusterWatchRule_RefusalRemovesAnAlreadyCompiledRule is the REVOCATION contract for the
// cluster kind: a rule accepted earlier and then refused must have its compiled rule REMOVED, not
// merely reported unready. The refusal used here is the KubeProvider withdrawing the target's
// namespace, which is the one refusal the compile path can reach at runtime.
func TestCompileClusterWatchRule_RefusalRemovesAnAlreadyCompiledRule(t *testing.T) {
	ctx := context.Background()
	m := bootManager(t,
		bootGitTarget(), bootGitProvider(),
		bootKubeProvider(mirrorGrants(bootTargetNS)),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: bootTargetNS}},
	)

	decision, err := CompileClusterWatchRule(
		ctx, m.Client, m.RuleStore, *bootClusterWatchRule(), *bootGitTarget(), nil)
	require.NoError(t, err)
	require.True(t, decision.Admitted)
	require.Len(t, bootCompiledNames(m), 1, "precondition: the rule is compiled")

	// The platform admin narrows the KubeProvider so it no longer admits the target's namespace.
	var provider configv1alpha3.KubeProvider
	providerKey := client.ObjectKey{Namespace: bootProviderNS, Name: bootProviderName}
	require.NoError(t, m.Client.Get(ctx, providerKey, &provider))
	provider.Spec.AllowedGitTargets = mirrorGrants("someone-else")
	require.NoError(t, m.Client.Update(ctx, &provider))

	decision, err = CompileClusterWatchRule(
		ctx, m.Client, m.RuleStore, *bootClusterWatchRule(), *bootGitTarget(), nil)

	require.NoError(t, err)
	assert.False(t, decision.Admitted)
	assert.Equal(t, ClusterWatchRuleReasonGitTargetNamespaceNotAuthorized, decision.Reason)
	assert.Empty(t, bootCompiledNames(m),
		"a refused rule must be removed from the store, not left running with a bad condition")
}
