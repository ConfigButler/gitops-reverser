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

func bootClusterProvider(policy *configv1alpha3.AllowedNamespaces) *configv1alpha3.ClusterProvider {
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
				Scope:     configv1alpha3.ResourceScopeNamespaced,
				Resources: []string{"configmaps"},
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
		bootClusterProvider(&configv1alpha3.AllowedNamespaces{Names: []string{"some-other-namespace"}}),
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
		bootClusterProvider(&configv1alpha3.AllowedNamespaces{Names: []string{bootTargetNS}}),
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
		bootClusterProvider(&configv1alpha3.AllowedNamespaces{Names: []string{"team-ok"}}),
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
