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
	k8stypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
)

const (
	snbTenantNS = "tenant-acme"
	snbSourceNS = "repo-config"
	snbTarget   = "acme"
	snbRule     = "repo-config-rule"
	snbProvider = "workspaces"
	// snbProviderNS is the KubeProvider's namespace: another than the tenant's, so the tenant is
	// admitted through spec.accessFrom.
	snbProviderNS = "platform"
)

func snbGitTarget() *configv1alpha3.GitTarget {
	return &configv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: snbTarget, Namespace: snbTenantNS},
		Spec: configv1alpha3.GitTargetSpec{
			GitBranchRef:    meta.NamespacedObjectReference{Name: gitBranchNameForTest("git", "main")},
			KubeProviderRef: meta.NamespacedObjectReference{Namespace: snbProviderNS, Name: snbProvider},
			Path:            "tenants/acme",
		},
	}
}

func snbGitProvider() *configv1alpha3.GitProvider {
	return &configv1alpha3.GitProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "git", Namespace: snbTenantNS},
	}
}

// snbKubeProvider admits snbTenantNS to watch its own namespace, and every namespace when wide.
func snbKubeProvider(wide bool) *configv1alpha3.KubeProvider {
	grant := configv1alpha3.SourceGrant{
		From: configv1alpha3.GrantFromNames, Names: []configv1alpha3.GrantNamespace{snbTenantNS},
		SourceNamespaces: []configv1alpha3.ScopeNamespace{configv1alpha3.NamespaceVariable},
	}
	if wide {
		grant.SourceNamespaces = append(grant.SourceNamespaces, "*")
	}
	return &configv1alpha3.KubeProvider{
		ObjectMeta: metav1.ObjectMeta{Namespace: snbProviderNS, Name: snbProvider},
		Spec: configv1alpha3.KubeProviderSpec{
			InCluster:         &configv1alpha3.KubeProviderInCluster{},
			AllowedGitTargets: []configv1alpha3.SourceGrant{grant},
		},
	}
}

// snbWatchRule builds a rule with one item per given rules[].sourceNamespace ("" = omitted).
func snbWatchRule(sourceNamespaces ...string) *configv1alpha3.WatchRule {
	items := make([]configv1alpha3.ResourceRule, 0, len(sourceNamespaces))
	for _, ns := range sourceNamespaces {
		items = append(items, configv1alpha3.ResourceRule{
			Resources: []string{"configmaps"}, SourceNamespace: ns,
		})
	}
	return &configv1alpha3.WatchRule{
		ObjectMeta: metav1.ObjectMeta{Name: snbRule, Namespace: snbTenantNS},
		Spec: configv1alpha3.WatchRuleSpec{
			GitTargetRef: meta.LocalObjectReference{Name: snbTarget},
			Rules:        items,
		},
	}
}

func snbManager(t *testing.T, objects ...client.Object) *Manager {
	t.Helper()
	return &Manager{
		Client:    fake.NewClientBuilder().WithScheme(makeScheme(t)).WithObjects(objects...).Build(),
		Log:       logr.Discard(),
		RuleStore: rulestore.NewStore(),
	}
}

func snbCompiledNames(m *Manager) []string {
	names := []string{}
	for _, r := range m.RuleStore.SnapshotWatchRules() {
		names = append(names, r.Source.Name)
	}
	return names
}

// Bootstrap seeds the store BEFORE the first reconcile and then marks it ready, so a gate the
// reconciler alone enforced would be bypassed for the whole startup window — and that window
// reopens on EVERY operator restart, which is exactly when nobody is watching. This asserts the
// state at the moment MarkReady() returns, which is the only moment that proves it: a passing
// reconciler test suite actively hides this failure.
func TestBootstrap_DeniedSourceNamespaceIsNotCompiledOnRestart(t *testing.T) {
	m := snbManager(t,
		// The provider does NOT delegate, so the override is refused.
		snbGitTarget(), snbGitProvider(), snbKubeProvider(false),
		snbWatchRule(snbSourceNS),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: snbTenantNS}},
	)

	require.NoError(t, m.bootstrapRuleStore(context.Background(), logr.Discard()),
		"a refused rule is a refusal, not a startup failure")

	assert.Empty(t, snbCompiledNames(m),
		"a denied override must not be compiled at bootstrap; otherwise every restart reopens "+
			"the window the gate exists to close")
	assert.True(t, m.RuleStore.IsReady(),
		"the store must still be marked ready so one refused rule cannot wedge the data plane")
}

// The upgrade guarantee at the bootstrap call site: a rule that omits sourceNamespace must seed
// exactly as before, with no provider involvement at all.
func TestBootstrap_LegacyWatchRuleStillCompiles(t *testing.T) {
	m := snbManager(t,
		snbGitTarget(), snbGitProvider(), snbKubeProvider(false),
		snbWatchRule(""),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: snbTenantNS}},
	)

	require.NoError(t, m.bootstrapRuleStore(context.Background(), logr.Discard()))

	compiled := m.RuleStore.SnapshotWatchRules()
	require.Len(t, compiled, 1)
	assert.Equal(t, snbRule, compiled[0].Source.Name)
	assert.Equal(t, []string{snbTenantNS}, compiled[0].ResourceRules[0].SourceNamespaces,
		"a legacy item's source namespace is the rule's own namespace")
}

// The admitted override path seeds the EFFECTIVE namespace, not the rule's own.
func TestBootstrap_AuthorizedOverrideCompilesWithItsSourceNamespace(t *testing.T) {
	m := snbManager(t,
		snbGitTarget(), snbGitProvider(), snbKubeProvider(true),
		snbWatchRule(snbSourceNS),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: snbTenantNS}},
	)

	require.NoError(t, m.bootstrapRuleStore(context.Background(), logr.Discard()))

	compiled := m.RuleStore.SnapshotWatchRules()
	require.Len(t, compiled, 1)
	assert.Equal(t, []string{snbSourceNS}, compiled[0].ResourceRules[0].SourceNamespaces)
	assert.Equal(t, snbTenantNS, compiled[0].Source.Namespace,
		"Source still names the WatchRule object in the control plane")
}

// "*" compiles to the EMPTY namespace, which is the cluster-wide collection — not to an enumerated set,
// and not to the rule's own namespace. This is the redefinition, asserted at the compile path that
// produces the store entry every stream and every resync scope is projected from.
func TestCompileWatchRule_WildcardCompilesToTheClusterWideCollection(t *testing.T) {
	ctx := context.Background()
	m := snbManager(t,
		snbGitTarget(), snbGitProvider(), snbKubeProvider(true),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: snbTenantNS}},
	)

	resolved, err := CompileWatchRule(ctx, m.Client, m.RuleStore,
		*snbWatchRule(configv1alpha3.SourceNamespaceWildcard), *snbGitTarget())

	require.NoError(t, err)
	require.True(t, resolved.Admitted())
	compiled := m.RuleStore.SnapshotWatchRules()
	require.Len(t, compiled, 1)
	assert.Equal(t, []string{""}, compiled[0].ResourceRules[0].SourceNamespaces,
		`"*" is one cluster-wide list and watch, which is the empty namespace`)
}

// "*" is refused outright while the provider does not delegate. It is the widest request in the
// API, so it must not be the one that slips through a provider that grants nothing.
func TestCompileWatchRule_WildcardIsRefusedWithoutDelegation(t *testing.T) {
	ctx := context.Background()
	m := snbManager(t,
		snbGitTarget(), snbGitProvider(), snbKubeProvider(false),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: snbTenantNS}},
	)

	resolved, err := CompileWatchRule(ctx, m.Client, m.RuleStore,
		*snbWatchRule(configv1alpha3.SourceNamespaceWildcard), *snbGitTarget())

	require.NoError(t, err)
	assert.False(t, resolved.Admitted())
	assert.Contains(t, resolved.Message, "spec.allowedGitTargets[].sourceNamespaces",
		"the refusal must name the grant a platform admin has to widen")
	assert.Empty(t, m.RuleStore.SnapshotWatchRules())
}

// The REVOCATION contract at the shared compile path: a rule accepted earlier and then denied by a
// withdrawn delegation must have its compiled rule REMOVED, not merely reported unready. A gate
// that only writes a condition is not a gate.
func TestCompileWatchRule_TerminalRefusalRemovesAnAlreadyCompiledRule(t *testing.T) {
	ctx := context.Background()
	m := snbManager(t,
		snbGitTarget(), snbGitProvider(), snbKubeProvider(true),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: snbTenantNS}},
	)

	rule := *snbWatchRule(snbSourceNS)
	target := *snbGitTarget()

	resolved, err := CompileWatchRule(ctx, m.Client, m.RuleStore, rule, target)
	require.NoError(t, err)
	require.True(t, resolved.Admitted())
	require.Len(t, m.RuleStore.SnapshotWatchRules(), 1, "precondition: the rule is compiled")

	// The platform admin narrows the grant to the tenant's own namespace.
	var stored configv1alpha3.KubeProvider
	providerKey := k8stypes.NamespacedName{Namespace: snbProviderNS, Name: snbProvider}
	require.NoError(t, m.Client.Get(ctx, providerKey, &stored))
	stored.Spec.AllowedGitTargets[0].SourceNamespaces = []configv1alpha3.ScopeNamespace{
		configv1alpha3.NamespaceVariable}
	require.NoError(t, m.Client.Update(ctx, &stored))

	resolved, err = CompileWatchRule(ctx, m.Client, m.RuleStore, rule, target)

	require.NoError(t, err)
	assert.False(t, resolved.Admitted())
	assert.Empty(t, m.RuleStore.SnapshotWatchRules(),
		"a revoked rule must be removed from the store, not left running with a bad condition")
}
