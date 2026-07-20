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
	"github.com/ConfigButler/gitops-reverser/internal/authz"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
)

const (
	snbTenantNS = "tenant-acme"
	snbSourceNS = "repo-config"
	snbTarget   = "acme"
	snbRule     = "repo-config-rule"
	snbProvider = "workspaces"
)

func snbGitTarget(policy *configv1alpha3.NamespaceMatcher) *configv1alpha3.GitTarget {
	return &configv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: snbTarget, Namespace: snbTenantNS},
		Spec: configv1alpha3.GitTargetSpec{
			ProviderRef:             configv1alpha3.GitProviderReference{Name: "git"},
			ClusterProviderRef:      &configv1alpha3.ClusterProviderReference{Name: snbProvider},
			Branch:                  "main",
			Path:                    "tenants/acme",
			AllowedSourceNamespaces: policy,
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
			AllowedNamespaces:                     &configv1alpha3.NamespaceMatcher{Names: []string{snbTenantNS}},
			AllowWatchRuleSourceNamespaceOverride: delegate,
		},
	}
}

func snbWatchRule(sourceNamespace string) *configv1alpha3.WatchRule {
	return &configv1alpha3.WatchRule{
		ObjectMeta: metav1.ObjectMeta{Name: snbRule, Namespace: snbTenantNS},
		Spec: configv1alpha3.WatchRuleSpec{
			TargetRef:       configv1alpha3.LocalTargetReference{Name: snbTarget},
			SourceNamespace: sourceNamespace,
			Rules:           []configv1alpha3.ResourceRule{{Resources: []string{"configmaps"}}},
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

// TestBootstrap_DeniedSourceNamespaceIsNotCompiledOnRestart is the second must-have test.
//
// Bootstrap seeds the store BEFORE the first reconcile and then marks it ready, so a gate the
// reconciler alone enforced would be bypassed for the whole startup window — and that window
// reopens on EVERY operator restart, which is exactly when nobody is watching. This asserts the
// state at the moment MarkReady() returns, which is the only moment that proves it: a passing
// reconciler test suite actively hides this failure.
func TestBootstrap_DeniedSourceNamespaceIsNotCompiledOnRestart(t *testing.T) {
	m := snbManager(t,
		// The provider does NOT delegate, so the override is refused.
		snbGitTarget(&configv1alpha3.NamespaceMatcher{Names: []string{snbSourceNS}}),
		snbGitProvider(),
		snbClusterProvider(false),
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

// TestBootstrap_LegacyWatchRuleStillCompiles is the upgrade guarantee at the bootstrap call site:
// a rule that omits sourceNamespace against a target with no policy must seed exactly as before.
func TestBootstrap_LegacyWatchRuleStillCompiles(t *testing.T) {
	m := snbManager(t,
		snbGitTarget(nil), snbGitProvider(), snbClusterProvider(false),
		snbWatchRule(""),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: snbTenantNS}},
	)

	require.NoError(t, m.bootstrapRuleStore(context.Background(), logr.Discard()))

	compiled := m.RuleStore.SnapshotWatchRules()
	require.Len(t, compiled, 1)
	assert.Equal(t, snbRule, compiled[0].Source.Name)
	assert.Equal(t, snbTenantNS, compiled[0].SourceNamespace,
		"a legacy rule's source namespace is its own namespace")
	assert.Equal(t, "main", compiled[0].Branch)
}

// TestBootstrap_AuthorizedOverrideCompilesWithItsSourceNamespace proves the admitted override path
// seeds the EFFECTIVE namespace, not the rule's own.
func TestBootstrap_AuthorizedOverrideCompilesWithItsSourceNamespace(t *testing.T) {
	m := snbManager(t,
		snbGitTarget(&configv1alpha3.NamespaceMatcher{Names: []string{snbSourceNS}}),
		snbGitProvider(), snbClusterProvider(true),
		snbWatchRule(snbSourceNS),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: snbTenantNS}},
	)

	require.NoError(t, m.bootstrapRuleStore(context.Background(), logr.Discard()))

	compiled := m.RuleStore.SnapshotWatchRules()
	require.Len(t, compiled, 1)
	assert.Equal(t, snbSourceNS, compiled[0].SourceNamespace)
	assert.Equal(t, snbTenantNS, compiled[0].Source.Namespace,
		"Source still names the WatchRule object in the control plane")
}

// TestCompileWatchRule_TerminalRefusalRemovesAnAlreadyCompiledRule is the REVOCATION contract at
// the shared compile path: a rule accepted earlier and then denied by a tightened policy must have
// its compiled rule REMOVED, not merely reported unready. A gate that only writes a condition is
// not a gate.
func TestCompileWatchRule_TerminalRefusalRemovesAnAlreadyCompiledRule(t *testing.T) {
	ctx := context.Background()
	m := snbManager(t,
		snbGitTarget(&configv1alpha3.NamespaceMatcher{Names: []string{snbSourceNS}}),
		snbGitProvider(), snbClusterProvider(true),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: snbTenantNS}},
	)

	rule := *snbWatchRule(snbSourceNS)
	target := *snbGitTarget(&configv1alpha3.NamespaceMatcher{Names: []string{snbSourceNS}})
	provider := *snbGitProvider()

	decision, err := CompileWatchRule(ctx, m.Client, m.RuleStore, m, rule, target, provider)
	require.NoError(t, err)
	require.True(t, decision.Admitted())
	require.Len(t, m.RuleStore.SnapshotWatchRules(), 1, "precondition: the rule is compiled")

	// The target owner tightens the policy so it no longer admits the namespace.
	tightened := *snbGitTarget(&configv1alpha3.NamespaceMatcher{Names: []string{"something-else"}})

	decision, err = CompileWatchRule(ctx, m.Client, m.RuleStore, m, rule, tightened, provider)

	require.NoError(t, err)
	assert.Equal(t, authz.SourceScopeDenied, decision.Verdict)
	assert.Empty(t, m.RuleStore.SnapshotWatchRules(),
		"a revoked rule must be removed from the store, not left running with a bad condition")
}

// TestCompileWatchRule_RetainsScopeWhenPolicyBecomesUnevaluatable is the MAINTAINING half of the
// establishing/maintaining contract, and the one that protects a tenant's Git content.
//
// A rule that already holds a resolved scope must keep it — and keep running — when its policy
// becomes unevaluatable. Narrowing to nothing there would feed an empty set into a resync sweep and
// DELETE the tenant's manifests over a transient source-cluster outage.
func TestCompileWatchRule_RetainsScopeWhenPolicyBecomesUnevaluatable(t *testing.T) {
	ctx := context.Background()
	m := snbManager(t,
		snbGitTarget(nil), snbGitProvider(), snbClusterProvider(true),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: snbTenantNS}},
	)

	rule := *snbWatchRule(snbSourceNS)
	provider := *snbGitProvider()
	named := *snbGitTarget(&configv1alpha3.NamespaceMatcher{Names: []string{snbSourceNS}})

	// Establish the grant through an exact name (no source-cluster access needed).
	decision, err := CompileWatchRule(ctx, m.Client, m.RuleStore, m, rule, named, provider)
	require.NoError(t, err)
	require.True(t, decision.Admitted())
	require.Len(t, m.RuleStore.SnapshotWatchRules(), 1)

	// The owner swaps it for a selector, and the source cluster's Namespace list is forbidden.
	selector := *snbGitTarget(&configv1alpha3.NamespaceMatcher{
		Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"mirrorable": "true"}},
	})
	m.sourceScope().store(configPlaneClusterID, namespaceSnapshot{forbidden: true})

	decision, err = CompileWatchRule(ctx, m.Client, m.RuleStore, m, rule, selector, provider)

	require.NoError(t, err)
	assert.Equal(t, authz.SourceScopeUnknown, decision.Verdict,
		"a retained scope is Unknown, never a terminal failure")
	assert.Len(t, m.RuleStore.SnapshotWatchRules(), 1,
		"the last known-good scope keeps running: no narrowing, no sweep")
}

// TestCompileWatchRule_UnevaluatablePolicyEstablishesNothing is the ESTABLISHING half. With no
// prior grant, the same unevaluatable policy must compile NOTHING — the grant is not established,
// so nothing runs and nothing is swept.
func TestCompileWatchRule_UnevaluatablePolicyEstablishesNothing(t *testing.T) {
	ctx := context.Background()
	m := snbManager(t,
		snbGitTarget(nil), snbGitProvider(), snbClusterProvider(true),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: snbTenantNS}},
	)
	m.sourceScope().store(configPlaneClusterID, namespaceSnapshot{forbidden: true})

	selector := *snbGitTarget(&configv1alpha3.NamespaceMatcher{
		Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"mirrorable": "true"}},
	})

	decision, err := CompileWatchRule(
		ctx, m.Client, m.RuleStore, m, *snbWatchRule(snbSourceNS), selector, *snbGitProvider())

	require.NoError(t, err)
	assert.Equal(t, authz.SourceScopeUnavailable, decision.Verdict,
		"with no scope ever resolved this is terminal, not a retained scope")
	assert.Empty(t, m.RuleStore.SnapshotWatchRules())
}

// TestCompileWatchRule_RetentionIsNamespaceSpecific: a rule that EDITS spec.sourceNamespace is
// establishing a NEW grant, so a stale grant for the previous namespace must not let an
// unevaluatable policy through.
func TestCompileWatchRule_RetentionIsNamespaceSpecific(t *testing.T) {
	ctx := context.Background()
	m := snbManager(t,
		snbGitTarget(nil), snbGitProvider(), snbClusterProvider(true),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: snbTenantNS}},
	)
	m.RecordSourceNamespaceGrant(
		k8stypes.NamespacedName{Name: snbRule, Namespace: snbTenantNS}, snbSourceNS)
	m.sourceScope().store(configPlaneClusterID, namespaceSnapshot{forbidden: true})

	selector := *snbGitTarget(&configv1alpha3.NamespaceMatcher{
		Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"mirrorable": "true"}},
	})

	// The rule now asks for a DIFFERENT namespace than the one it holds a grant for.
	decision, err := CompileWatchRule(
		ctx, m.Client, m.RuleStore, m, *snbWatchRule("some-other-namespace"), selector, *snbGitProvider())

	require.NoError(t, err)
	assert.Equal(t, authz.SourceScopeUnavailable, decision.Verdict,
		"a grant for a different namespace must not be retained across an edit")
}

// TestResolveSourceNamespace_ThreeValuedResults pins the source-scope service's own contract: an
// unsynced cache is "cannot say yet", a Forbidden list is terminal, and a synced cache gives a real
// yes/no. Collapsing any of these into another is how a transient outage becomes a stopped stream.
func TestResolveSourceNamespace_ThreeValuedResults(t *testing.T) {
	ctx := context.Background()
	target := snbGitTarget(&configv1alpha3.NamespaceMatcher{
		Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"mirrorable": "true"}},
	})

	t.Run("unsynced cache is Unknown, never Denied", func(t *testing.T) {
		m := snbManager(t)
		result := m.ResolveSourceNamespace(ctx, target, snbSourceNS)
		assert.Equal(t, authz.SourceScopeUnknown, result.Verdict)
	})

	t.Run("forbidden Namespace list is terminal", func(t *testing.T) {
		m := snbManager(t)
		m.sourceScope().store(configPlaneClusterID, namespaceSnapshot{forbidden: true})
		result := m.ResolveSourceNamespace(ctx, target, snbSourceNS)
		assert.Equal(t, authz.SourceScopeUnavailable, result.Verdict)
		assert.Contains(t, result.Message, "use exact names")
	})

	t.Run("synced cache with matching labels admits", func(t *testing.T) {
		m := snbManager(t)
		m.sourceScope().store(configPlaneClusterID, namespaceSnapshot{
			synced: true,
			labels: map[string]map[string]string{snbSourceNS: {"mirrorable": "true"}},
		})
		result := m.ResolveSourceNamespace(ctx, target, snbSourceNS)
		assert.Equal(t, authz.SourceScopeAdmitted, result.Verdict)
	})

	t.Run("synced cache with non-matching labels denies", func(t *testing.T) {
		m := snbManager(t)
		m.sourceScope().store(configPlaneClusterID, namespaceSnapshot{
			synced: true,
			labels: map[string]map[string]string{snbSourceNS: {"mirrorable": "false"}},
		})
		result := m.ResolveSourceNamespace(ctx, target, snbSourceNS)
		assert.Equal(t, authz.SourceScopeDenied, result.Verdict)
	})

	t.Run("synced cache missing the namespace denies with a legible cause", func(t *testing.T) {
		m := snbManager(t)
		m.sourceScope().store(configPlaneClusterID, namespaceSnapshot{
			synced: true,
			labels: map[string]map[string]string{"elsewhere": {"mirrorable": "true"}},
		})
		result := m.ResolveSourceNamespace(ctx, target, snbSourceNS)
		assert.Equal(t, authz.SourceScopeDenied, result.Verdict)
		assert.Contains(t, result.Message, "does not exist")
	})
}

// TestSourceNamespaceSnapshot_StoreDetectsObservableChange pins the ENQUEUE trigger. Only a real
// change may enqueue — otherwise every 30s refresh re-reconciles every rule — but a LABEL EDIT
// must, or a revocation goes stale in the cache and never lands.
func TestSourceNamespaceSnapshot_StoreDetectsObservableChange(t *testing.T) {
	scope := &sourceNamespaceScope{
		wanted:    map[string]struct{}{},
		snapshots: map[string]namespaceSnapshot{},
		grants:    map[k8stypes.NamespacedName]string{},
	}
	synced := func(labels map[string]string) namespaceSnapshot {
		return namespaceSnapshot{synced: true, labels: map[string]map[string]string{snbSourceNS: labels}}
	}

	assert.True(t, scope.store("c", synced(map[string]string{"a": "1"})), "the first snapshot is a change")
	assert.False(t, scope.store("c", synced(map[string]string{"a": "1"})), "an identical refresh is not")
	assert.True(t, scope.store("c", synced(map[string]string{"a": "2"})), "a label edit is a change")
	assert.True(t, scope.store("c", namespaceSnapshot{forbidden: true}), "losing access is a change")
	assert.False(t, scope.store("c", namespaceSnapshot{forbidden: true}), "still forbidden is not")
}

// TestRetainOnRetryableError keeps a synced snapshot usable across a blip: a momentary list failure
// must not revoke anything, because the answers it already holds are still the best available.
func TestRetainOnRetryableError(t *testing.T) {
	previous := namespaceSnapshot{
		synced: true,
		labels: map[string]map[string]string{snbSourceNS: {"mirrorable": "true"}},
	}

	next := retainOnRetryableError(previous, assert.AnError)

	assert.True(t, next.synced, "a transient failure must not un-sync a working cache")
	assert.False(t, next.forbidden, "a transient failure is not the terminal Forbidden case")
	assert.Equal(t, previous.labels, next.labels)
}
