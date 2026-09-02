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
)

func snbGitTarget() *configv1alpha3.GitTarget {
	return &configv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: snbTarget, Namespace: snbTenantNS},
		Spec: configv1alpha3.GitTargetSpec{
			ProviderRef:        configv1alpha3.GitProviderReference{Name: "git"},
			ClusterProviderRef: &configv1alpha3.ClusterProviderReference{Name: snbProvider},
			Branch:             "main",
			Path:               "tenants/acme",
		},
	}
}

func snbGitProvider() *configv1alpha3.GitProvider {
	return &configv1alpha3.GitProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "git", Namespace: snbTenantNS},
	}
}

func snbClusterProvider(delegate bool) *configv1alpha3.ClusterProvider {
	return &configv1alpha3.ClusterProvider{
		ObjectMeta: metav1.ObjectMeta{Name: snbProvider},
		Spec: configv1alpha3.ClusterProviderSpec{
			AccessFrom:              &configv1alpha3.NamespaceMatcher{Names: []string{snbTenantNS}},
			AllowAnySourceNamespace: delegate,
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
			TargetRef: configv1alpha3.LocalTargetReference{Name: snbTarget},
			Rules:     items,
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
		snbGitTarget(), snbGitProvider(), snbClusterProvider(false),
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
		snbGitTarget(), snbGitProvider(), snbClusterProvider(false),
		snbWatchRule(""),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: snbTenantNS}},
	)

	require.NoError(t, m.bootstrapRuleStore(context.Background(), logr.Discard()))

	compiled := m.RuleStore.SnapshotWatchRules()
	require.Len(t, compiled, 1)
	assert.Equal(t, snbRule, compiled[0].Source.Name)
	assert.Equal(t, []string{snbTenantNS}, compiled[0].ResourceRules[0].SourceNamespaces,
		"a legacy item's source namespace is the rule's own namespace")
	assert.Equal(t, "main", compiled[0].Branch)
}

// The admitted override path seeds the EFFECTIVE namespace, not the rule's own.
func TestBootstrap_AuthorizedOverrideCompilesWithItsSourceNamespace(t *testing.T) {
	m := snbManager(t,
		snbGitTarget(), snbGitProvider(), snbClusterProvider(true),
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

// "*" compiles to the EMPTY namespace, which is the cluster-wide cell — not to an enumerated set,
// and not to the rule's own namespace. This is the redefinition, asserted at the compile path that
// produces the store entry every stream and every resync scope is projected from.
func TestCompileWatchRule_WildcardCompilesToTheClusterWideCell(t *testing.T) {
	ctx := context.Background()
	m := snbManager(t,
		snbGitTarget(), snbGitProvider(), snbClusterProvider(true),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: snbTenantNS}},
	)

	resolved, err := CompileWatchRule(ctx, m.Client, m.RuleStore,
		*snbWatchRule(configv1alpha3.SourceNamespaceWildcard), *snbGitTarget(), *snbGitProvider())

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
		snbGitTarget(), snbGitProvider(), snbClusterProvider(false),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: snbTenantNS}},
	)

	resolved, err := CompileWatchRule(ctx, m.Client, m.RuleStore,
		*snbWatchRule(configv1alpha3.SourceNamespaceWildcard), *snbGitTarget(), *snbGitProvider())

	require.NoError(t, err)
	assert.False(t, resolved.Admitted())
	assert.Contains(t, resolved.Message, "allowAnySourceNamespace",
		"the refusal must name the flag a platform admin has to set")
	assert.Empty(t, m.RuleStore.SnapshotWatchRules())
}

// The REVOCATION contract at the shared compile path: a rule accepted earlier and then denied by a
// withdrawn delegation must have its compiled rule REMOVED, not merely reported unready. A gate
// that only writes a condition is not a gate.
func TestCompileWatchRule_TerminalRefusalRemovesAnAlreadyCompiledRule(t *testing.T) {
	ctx := context.Background()
	m := snbManager(t,
		snbGitTarget(), snbGitProvider(), snbClusterProvider(true),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: snbTenantNS}},
	)

	rule := *snbWatchRule(snbSourceNS)
	target := *snbGitTarget()
	provider := *snbGitProvider()

	resolved, err := CompileWatchRule(ctx, m.Client, m.RuleStore, rule, target, provider)
	require.NoError(t, err)
	require.True(t, resolved.Admitted())
	require.Len(t, m.RuleStore.SnapshotWatchRules(), 1, "precondition: the rule is compiled")

	// The platform admin withdraws the delegation.
	var stored configv1alpha3.ClusterProvider
	require.NoError(t, m.Client.Get(ctx, k8stypes.NamespacedName{Name: snbProvider}, &stored))
	stored.Spec.AllowAnySourceNamespace = false
	require.NoError(t, m.Client.Update(ctx, &stored))

	resolved, err = CompileWatchRule(ctx, m.Client, m.RuleStore, rule, target, provider)

	require.NoError(t, err)
	assert.False(t, resolved.Admitted())
	assert.Empty(t, m.RuleStore.SnapshotWatchRules(),
		"a revoked rule must be removed from the store, not left running with a bad condition")
}

// A GitTarget still carrying spec.allowedSourceNamespaces compiles NOTHING, and the check is here
// rather than only on the GitTarget's Validated gate because BOOTSTRAP seeds the store before the
// first reconcile, on every restart. A gate the reconciler alone enforced would be bypassed for
// that whole window — the same argument that put the source-namespace gate on this path.
//
// The hazard it closes is specific: the removed field is inert, so a target still carrying it reads
// like a bound on which source namespaces reach its folder while enforcing nothing. The moment its
// ClusterProvider is migrated, a "*" rule under it widens from that declared set to every namespace
// the credential can read.
func TestBootstrap_LegacySourceNamespacePolicyIsNotCompiledOnRestart(t *testing.T) {
	target := snbGitTarget()
	//nolint:staticcheck // setting the removed field is the point: it must refuse, not be ignored.
	target.Spec.AllowedSourceNamespaces = &configv1alpha3.NamespaceMatcher{
		Names: []string{snbSourceNS},
	}
	m := snbManager(t,
		target, snbGitProvider(), snbClusterProvider(true),
		snbWatchRule(configv1alpha3.SourceNamespaceWildcard),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: snbTenantNS}},
	)

	require.NoError(t, m.bootstrapRuleStore(context.Background(), logr.Discard()),
		"an unmigrated target is a refusal, not a startup failure")

	assert.Empty(t, snbCompiledNames(m),
		"a wildcard rule must not widen to cluster-wide against a target nobody has migrated")
	assert.True(t, m.RuleStore.IsReady(),
		"the store must still be marked ready so one unmigrated target cannot wedge the data plane")
}

// The same refusal at the compile path, with the message an operator reads, and the revocation
// contract: a target that was compiled and then found to carry the field has its rule REMOVED.
func TestCompileWatchRule_LegacySourceNamespacePolicyRemovesTheCompiledRule(t *testing.T) {
	ctx := context.Background()
	m := snbManager(t,
		snbGitTarget(), snbGitProvider(), snbClusterProvider(true),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: snbTenantNS}},
	)
	rule := *snbWatchRule(snbSourceNS)

	resolved, err := CompileWatchRule(ctx, m.Client, m.RuleStore, rule, *snbGitTarget(), *snbGitProvider())
	require.NoError(t, err)
	require.True(t, resolved.Admitted(), "precondition: the rule compiles against a migrated target")
	require.Len(t, m.RuleStore.SnapshotWatchRules(), 1)

	legacy := *snbGitTarget()
	//nolint:staticcheck // setting the removed field is the point.
	legacy.Spec.AllowedSourceNamespaces = &configv1alpha3.NamespaceMatcher{Names: []string{snbSourceNS}}

	resolved, err = CompileWatchRule(ctx, m.Client, m.RuleStore, rule, legacy, *snbGitProvider())

	require.NoError(t, err)
	assert.False(t, resolved.Admitted())
	assert.Contains(t, resolved.Message, "spec.allowedSourceNamespaces",
		"the refusal must name the field to delete")
	assert.Empty(t, m.RuleStore.SnapshotWatchRules(),
		"a gate that only reports is not a gate: the compiled rule must be gone")
}

// A ClusterWatchRule selects cluster-scoped objects, which the removed field never bounded, so this
// refusal does not change what it mirrors. It still refuses, so the operator fixes one object
// rather than discovering the migration kind by kind.
func TestCompileClusterWatchRule_LegacySourceNamespacePolicyRefuses(t *testing.T) {
	ctx := context.Background()
	m := snbManager(t,
		snbGitTarget(), snbGitProvider(), snbClusterProvider(true),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: snbTenantNS}},
	)
	legacy := *snbGitTarget()
	//nolint:staticcheck // setting the removed field is the point.
	legacy.Spec.AllowedSourceNamespaces = &configv1alpha3.NamespaceMatcher{Names: []string{snbSourceNS}}

	rule := configv1alpha3.ClusterWatchRule{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-rule"},
		Spec: configv1alpha3.ClusterWatchRuleSpec{
			TargetRef: configv1alpha3.NamespacedTargetReference{Name: snbTarget, Namespace: snbTenantNS},
			Rules:     []configv1alpha3.ClusterResourceRule{{Resources: []string{"namespaces"}}},
		},
	}

	decision, err := CompileClusterWatchRule(ctx, m.Client, m.RuleStore, rule, legacy, *snbGitProvider())

	require.NoError(t, err)
	assert.False(t, decision.Admitted)
	assert.Contains(t, decision.Message, "spec.allowedSourceNamespaces")
}
