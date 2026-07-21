// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
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
			AllowedNamespaces:            &configv1alpha3.NamespaceMatcher{Names: []string{snbTenantNS}},
			AllowSourceNamespaceOverride: delegate,
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
	assert.Equal(t, []string{snbTenantNS}, compiled[0].ResourceRules[0].SourceNamespaces,
		"a legacy item's source namespace is the rule's own namespace")
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
	assert.Equal(t, []string{snbSourceNS}, compiled[0].ResourceRules[0].SourceNamespaces)
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

	resolved, err := CompileWatchRule(ctx, m.Client, m.RuleStore, m, rule, target, provider)
	require.NoError(t, err)
	require.True(t, resolved.Admitted())
	require.Len(t, m.RuleStore.SnapshotWatchRules(), 1, "precondition: the rule is compiled")

	// The target owner tightens the policy so it no longer admits the namespace.
	tightened := *snbGitTarget(&configv1alpha3.NamespaceMatcher{Names: []string{"something-else"}})

	resolved, err = CompileWatchRule(ctx, m.Client, m.RuleStore, m, rule, tightened, provider)

	require.NoError(t, err)
	assert.Equal(t, authz.SourceScopeDenied, resolved.Verdict)
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
	resolved, err := CompileWatchRule(ctx, m.Client, m.RuleStore, m, rule, named, provider)
	require.NoError(t, err)
	require.True(t, resolved.Admitted())
	require.Len(t, m.RuleStore.SnapshotWatchRules(), 1)

	// The owner swaps it for a selector, and the source cluster's Namespace list is forbidden.
	selector := *snbGitTarget(&configv1alpha3.NamespaceMatcher{
		Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"mirrorable": "true"}},
	})
	m.sourceScope().store(snbProvider, namespaceSnapshot{forbidden: true})

	resolved, err = CompileWatchRule(ctx, m.Client, m.RuleStore, m, rule, selector, provider)

	require.NoError(t, err)
	assert.Equal(t, authz.SourceScopeUnknown, resolved.Verdict,
		"a retained scope is Unknown, never a terminal failure")

	// The ABSENCE of the sweep is the assertion that matters, and it is a property of the RESOLVED
	// SCOPE, not of the condition: the watched-type table (and therefore every resync scope) is
	// projected from the compiled rule's SourceNamespaces. A narrowing to the empty set would leave
	// the rule present and the condition Unknown while quietly emptying the desired set — which is
	// what a mark-and-sweep resync turns into a deletion of the tenant's manifests.
	compiled := m.RuleStore.SnapshotWatchRules()
	require.Len(t, compiled, 1,
		"the last known-good scope keeps running: no narrowing, no sweep")
	assert.Equal(t, []string{snbSourceNS}, compiled[0].ResourceRules[0].SourceNamespaces,
		"the retained scope must be the LAST KNOWN-GOOD set, not narrowed and not widened")

	// And the resolved scope stays recorded under the same spec, so a further unevaluatable
	// reconcile keeps retaining rather than flipping terminal on the second pass.
	retained, ok := m.RetainedSourceScope(
		k8stypes.NamespacedName{Name: snbRule, Namespace: snbTenantNS}, SourceScopeSpecHash(&rule))
	require.True(t, ok, "'cannot say' must never forget the grant")
	assert.Equal(t, [][]string{{snbSourceNS}}, retained)
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
	m.sourceScope().store(snbProvider, namespaceSnapshot{forbidden: true})

	selector := *snbGitTarget(&configv1alpha3.NamespaceMatcher{
		Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"mirrorable": "true"}},
	})

	resolved, err := CompileWatchRule(
		ctx, m.Client, m.RuleStore, m, *snbWatchRule(snbSourceNS), selector, *snbGitProvider())

	require.NoError(t, err)
	assert.Equal(t, authz.SourceScopeUnavailable, resolved.Verdict,
		"with no scope ever resolved this is terminal, not a retained scope")
	assert.Empty(t, m.RuleStore.SnapshotWatchRules())
}

// TestCompileWatchRule_RetentionIsSpecSpecific: a rule that EDITS its items is establishing a NEW
// scope, so a stale grant recorded under the previous spec must not let an unevaluatable policy
// through. Keying the memory by spec hash rather than by item index is also what stops a REORDER
// from making one item inherit another item's grant.
func TestCompileWatchRule_RetentionIsSpecSpecific(t *testing.T) {
	ctx := context.Background()
	m := snbManager(t,
		snbGitTarget(nil), snbGitProvider(), snbClusterProvider(true),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: snbTenantNS}},
	)
	granted := snbWatchRule(snbSourceNS)
	grantKey := k8stypes.NamespacedName{Name: snbRule, Namespace: snbTenantNS}
	m.RecordSourceScopeGrant(grantKey, SourceScopeSpecHash(granted), [][]string{{snbSourceNS}})
	m.sourceScope().store(snbProvider, namespaceSnapshot{forbidden: true})

	selector := *snbGitTarget(&configv1alpha3.NamespaceMatcher{
		Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"mirrorable": "true"}},
	})

	// The SAME spec retains its grant — that is the maintaining case.
	resolved, err := CompileWatchRule(
		ctx, m.Client, m.RuleStore, m, *granted, selector, *snbGitProvider())
	require.NoError(t, err)
	assert.Equal(t, authz.SourceScopeUnknown, resolved.Verdict,
		"the spec that established the grant keeps it, and reports Unknown rather than Failed")

	// An EDITED spec is establishing a new scope, so the stale grant must not let it through.
	resolved, err = CompileWatchRule(
		ctx, m.Client, m.RuleStore, m, *snbWatchRule("some-other-namespace"), selector, *snbGitProvider())

	require.NoError(t, err)
	assert.Equal(t, authz.SourceScopeUnavailable, resolved.Verdict,
		"a grant recorded under a different spec must not be retained across an edit")

	_, retained := m.RetainedSourceScope(grantKey, SourceScopeSpecHash(granted))
	assert.False(t, retained, "the terminal refusal drops the grant so nothing stale survives it")
}

// TestSourceScopeSpecHash_MovesWithEveryScopeInput pins what "the same spec" means for retention: any
// change that could move the resolved scope must discard the grant, and a reorder must not look
// like no change at all.
func TestSourceScopeSpecHash_MovesWithEveryScopeInput(t *testing.T) {
	base := snbWatchRule("", snbSourceNS)

	assert.Equal(t, SourceScopeSpecHash(base), SourceScopeSpecHash(snbWatchRule("", snbSourceNS)),
		"an unchanged spec must hash identically, or nothing is ever retained")
	assert.NotEqual(t, SourceScopeSpecHash(base), SourceScopeSpecHash(snbWatchRule(snbSourceNS, "")),
		"a REORDER changes which item holds which grant, so it must discard the memory")
	assert.NotEqual(t, SourceScopeSpecHash(base), SourceScopeSpecHash(snbWatchRule("")),
		"dropping an item changes the spec")
	assert.NotEqual(t, SourceScopeSpecHash(base),
		SourceScopeSpecHash(snbWatchRule("", configv1alpha3.SourceNamespaceWildcard)),
		"changing an item's requested namespace changes the spec")

	moved := snbWatchRule("", snbSourceNS)
	moved.Namespace = "tenant-zen"
	assert.NotEqual(t, SourceScopeSpecHash(base), SourceScopeSpecHash(moved),
		"the rule's own namespace is what an omitted item resolves to, so it is part of the spec")
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
		m.sourceScope().store(snbProvider, namespaceSnapshot{forbidden: true})
		result := m.ResolveSourceNamespace(ctx, target, snbSourceNS)
		assert.Equal(t, authz.SourceScopeUnavailable, result.Verdict)
		assert.Contains(t, result.Message, "use exact names")
	})

	t.Run("synced cache with matching labels admits", func(t *testing.T) {
		m := snbManager(t)
		m.sourceScope().store(snbProvider, namespaceSnapshot{
			synced: true,
			labels: map[string]map[string]string{snbSourceNS: {"mirrorable": "true"}},
		})
		result := m.ResolveSourceNamespace(ctx, target, snbSourceNS)
		assert.Equal(t, authz.SourceScopeAdmitted, result.Verdict)
	})

	t.Run("synced cache with non-matching labels denies", func(t *testing.T) {
		m := snbManager(t)
		m.sourceScope().store(snbProvider, namespaceSnapshot{
			synced: true,
			labels: map[string]map[string]string{snbSourceNS: {"mirrorable": "false"}},
		})
		result := m.ResolveSourceNamespace(ctx, target, snbSourceNS)
		assert.Equal(t, authz.SourceScopeDenied, result.Verdict)
	})

	t.Run("synced cache missing the namespace denies with a legible cause", func(t *testing.T) {
		m := snbManager(t)
		m.sourceScope().store(snbProvider, namespaceSnapshot{
			synced: true,
			labels: map[string]map[string]string{"elsewhere": {"mirrorable": "true"}},
		})
		result := m.ResolveSourceNamespace(ctx, target, snbSourceNS)
		assert.Equal(t, authz.SourceScopeDenied, result.Verdict)
		assert.Contains(t, result.Message, "does not exist")
	})
}

// TestResolveSourceNamespace_ReadsTheGitTargetsOwnCluster is the divergent-labels test, and the
// divergence is the entire point: with both clusters labelled the same way, this passes against a
// resolver reading either one.
//
// A GitTarget's policy is a statement about ITS source cluster. Resolving it through the
// Declare-time cache — which defaults an undeclared GitTarget to the config plane — means a remote
// target's selector is answered from config-plane Namespace labels during the window between the
// WatchRule reconcile and the GitTarget controller's Declare. Those two controllers run
// concurrently after a restart, so the window is ordinary operation, not a corner case. Here the
// config plane would admit and the real source cluster would not; admitting is the bug.
func TestResolveSourceNamespace_ReadsTheGitTargetsOwnCluster(t *testing.T) {
	ctx := context.Background()
	target := snbGitTarget(&configv1alpha3.NamespaceMatcher{
		Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"mirrorable": "true"}},
	})
	m := snbManager(t)

	// The config plane carries a same-named namespace that DOES match, plus one the source cluster
	// has never heard of. Neither may reach a decision about this target.
	m.sourceScope().store(configPlaneClusterID, namespaceSnapshot{
		synced: true,
		labels: map[string]map[string]string{
			snbSourceNS:    {"mirrorable": "true"},
			"only-up-here": {"mirrorable": "true"},
		},
	})
	m.sourceScope().store(snbProvider, namespaceSnapshot{
		synced: true,
		labels: map[string]map[string]string{snbSourceNS: {"mirrorable": "false"}},
	})

	result := m.ResolveSourceNamespace(ctx, target, snbSourceNS)
	assert.Equal(t, authz.SourceScopeDenied, result.Verdict,
		"the source cluster's labels decide, not a same-named namespace on the config plane")

	names, enumeration := m.EnumerateSourceNamespaces(ctx, target)
	require.Equal(t, authz.SourceScopeAdmitted, enumeration.Verdict)
	assert.Empty(t, names,
		"a wildcard expands over the SOURCE cluster's namespaces; the config plane's must not leak in")

	// The refresh loop must be armed for the same cluster the answer came from — otherwise the
	// snapshot that gets refreshed and the snapshot that gets read are two different clusters, and
	// the enqueue that carries a revocation matches no GitTarget at all.
	assert.Equal(t, []string{snbProvider}, m.sourceScope().wantedClusters())
}

// stubNamespaceLister is a dynamic.Interface serving one canned Namespace list, which reports the
// context its List was given. Only List is implemented: the embedded interfaces are nil, so any
// other call panics loudly instead of passing silently.
type stubNamespaceLister struct {
	dynamic.Interface

	onList func(ctx context.Context)
}

func (s stubNamespaceLister) Resource(schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	return stubNamespaceResource{onList: s.onList}
}

type stubNamespaceResource struct {
	dynamic.NamespaceableResourceInterface

	onList func(ctx context.Context)
}

func (s stubNamespaceResource) List(
	ctx context.Context, _ metav1.ListOptions,
) (*unstructured.UnstructuredList, error) {
	s.onList(ctx)
	return &unstructured.UnstructuredList{}, nil
}

// armStubCluster makes clusterID a cluster the refresh loop wants, backed by a stub client.
func armStubCluster(m *Manager, clusterID string, onList func(context.Context)) {
	m.sourceScope().want(clusterID)
	m.cluster(clusterID).dynamicClient = stubNamespaceLister{onList: onList}
}

// TestRefreshSourceNamespaceScopes_BoundsEveryClustersList pins the deadline.
//
// A source cluster's REST config deliberately carries no request timeout — its watches must stay
// open — and only its dial is bounded, so a cluster that accepts the connection and then never
// answers would block this refresh forever. Because the refresh runs inside
// ReconcileForRuleChange, "forever" also means the watched-type tables and target watches after it
// never refresh again, for every tenant.
func TestRefreshSourceNamespaceScopes_BoundsEveryClustersList(t *testing.T) {
	m := snbManager(t)

	var mu sync.Mutex
	remaining := map[string]time.Duration{}
	unbounded := []string{}

	for _, id := range []string{"cluster-a", "cluster-b"} {
		armStubCluster(m, id, func(ctx context.Context) {
			mu.Lock()
			defer mu.Unlock()
			deadline, ok := ctx.Deadline()
			if !ok {
				unbounded = append(unbounded, id)
				return
			}
			remaining[id] = time.Until(deadline)
		})
	}

	m.refreshSourceNamespaceScopes(context.Background())

	assert.Empty(t, unbounded, "every cluster's list must run under its own deadline")
	require.Len(t, remaining, 2, "every wanted cluster must be listed")
	for id, left := range remaining {
		assert.Positive(t, left, "%s got an already-expired deadline", id)
		assert.LessOrEqual(t, left, sourceNamespaceListTimeout,
			"%s got a deadline longer than the bound", id)
	}
}

// TestRefreshSourceNamespaceScopes_OneWedgedClusterCannotStarveTheOthers pins the fan-out.
//
// Serially, total latency grows as clusterCount × the slowest cluster, so ONE tenant's unreachable
// source cluster delays every other tenant's grants and revocations — the same failure the catalog
// refresh already had, and fixed, one file over. The barrier is what makes this a real test: it
// only falls through once every cluster is inside its list at the same moment, which a serial loop
// can never achieve.
func TestRefreshSourceNamespaceScopes_OneWedgedClusterCannotStarveTheOthers(t *testing.T) {
	const clusters = 3

	m := snbManager(t)
	entered := make(chan struct{}, clusters)
	release := make(chan struct{})
	var concurrent atomic.Bool
	concurrent.Store(true)

	for i := range clusters {
		armStubCluster(m, fmt.Sprintf("cluster-%d", i), func(context.Context) {
			entered <- struct{}{}
			select {
			case <-release:
			case <-time.After(2 * time.Second):
				concurrent.Store(false)
			}
		})
	}

	go func() {
		for range clusters {
			<-entered
		}
		close(release)
	}()

	m.refreshSourceNamespaceScopes(context.Background())

	assert.True(t, concurrent.Load(),
		"every wanted cluster must be listed concurrently, so one wedged cluster blocks only itself")
	for i := range clusters {
		_, ok := m.sourceScope().snapshot(fmt.Sprintf("cluster-%d", i))
		assert.True(t, ok, "cluster-%d was never listed", i)
	}
}

// TestSourceNamespaceSnapshot_StoreDetectsObservableChange pins the ENQUEUE trigger. Only a real
// change may enqueue — otherwise every 30s refresh re-reconciles every rule — but a LABEL EDIT
// must, or a revocation goes stale in the cache and never lands.
func TestSourceNamespaceSnapshot_StoreDetectsObservableChange(t *testing.T) {
	scope := &sourceNamespaceScope{
		wanted:    map[string]struct{}{},
		snapshots: map[string]namespaceSnapshot{},
		grants:    map[k8stypes.NamespacedName]sourceScopeGrant{},
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
