// SPDX-License-Identifier: Apache-2.0

package authz_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fluxcd/pkg/apis/meta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/authz"
)

const (
	snTenantNS   = "tenant-acme"
	snSourceNS   = "repo-config"
	snTargetName = "acme"
	snRuleName   = "repo-config-rule"
	snProvider   = "workspaces"
	snProviderNS = "platform"
	snWildcard   = configv1alpha3.SourceNamespaceWildcard
)

func snScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, configv1alpha3.AddToScheme(s))
	return s
}

func snTarget() *configv1alpha3.GitTarget {
	return &configv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: snTargetName, Namespace: snTenantNS},
		Spec: configv1alpha3.GitTargetSpec{
			GitBranchRef:    meta.NamespacedObjectReference{Name: "acme-git-main"},
			KubeProviderRef: meta.NamespacedObjectReference{Name: snProvider, Namespace: snProviderNS},
			Path:            "tenants/acme",
		},
	}
}

// snRule builds a WatchRule with one item per given sourceNamespace ("" = omitted).
func snRule(sourceNamespaces ...string) *configv1alpha3.WatchRule {
	items := make([]configv1alpha3.ResourceRule, 0, len(sourceNamespaces))
	for _, ns := range sourceNamespaces {
		items = append(items, configv1alpha3.ResourceRule{
			Resources: []string{"configmaps"}, SourceNamespace: ns,
		})
	}
	return &configv1alpha3.WatchRule{
		ObjectMeta: metav1.ObjectMeta{Name: snRuleName, Namespace: snTenantNS},
		Spec: configv1alpha3.WatchRuleSpec{
			GitTargetRef: meta.LocalObjectReference{Name: snTargetName},
			Rules:        items,
		},
	}
}

// snKubeProvider admits snTenantNS by name with scope as its sourceNamespaces, so the "provider
// admits the target's namespace" leg of the gate passes and each case isolates the scope.
func snKubeProvider(scope ...configv1alpha3.ScopeNamespace) *configv1alpha3.KubeProvider {
	grant := configv1alpha3.KubeProviderGitTargetGrant{
		ConsumerNamespaces: configv1alpha3.ConsumerNamespaces{
			From:  configv1alpha3.GrantFromNames,
			Names: []configv1alpha3.GrantNamespace{snTenantNS},
		},
		SourceNamespaces: scope,
	}
	if len(scope) == 0 {
		grant.ClusterResources = []configv1alpha3.ClusterResourceIdentity{"*"}
	}
	return &configv1alpha3.KubeProvider{
		ObjectMeta: metav1.ObjectMeta{Name: snProvider, Namespace: snProviderNS},
		Spec: configv1alpha3.KubeProviderSpec{
			AllowedGitTargets: []configv1alpha3.KubeProviderGitTargetGrant{grant},
		},
	}
}

const (
	own = configv1alpha3.ScopeNamespace(configv1alpha3.ConsumerNamespaceVariable)
	all = configv1alpha3.ScopeNamespace("*")
)

func snReader(t *testing.T, objects ...client.Object) client.Reader {
	t.Helper()
	objects = append(objects,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: snTenantNS}})
	return fake.NewClientBuilder().WithScheme(snScheme(t)).WithObjects(objects...).Build()
}

// The gate is one rule and this is its table: an item may watch a namespace only while the grants
// that admit the GitTarget give it that namespace. The rule's own namespace is no exception.
//
// There is deliberately no row for a GitTarget policy. There is no longer such a field: what it
// bounded is bounded by the source credential's own RBAC, which this gate cannot and does not try
// to predict.
func TestResolveWatchRuleSourceScope(t *testing.T) {
	tests := []struct {
		name       string
		items      []string
		scope      []configv1alpha3.ScopeNamespace
		admitted   bool
		namespaces [][]string
		says       string
	}{
		{
			name:       "an omitted item watches the rule's own namespace, granted by {consumer.namespace}",
			items:      []string{""},
			scope:      []configv1alpha3.ScopeNamespace{own},
			admitted:   true,
			namespaces: [][]string{{snTenantNS}},
		},
		{
			name:     "an explicit scope is the whole scope, the rule's own namespace excluded",
			items:    []string{""},
			scope:    []configv1alpha3.ScopeNamespace{"flux-system"},
			admitted: false,
			says:     `sourceNamespaces: [flux-system] means flux-system only`,
		},
		{
			name:     "a grant with clusterResources alone gives no namespace",
			items:    []string{""},
			admitted: false,
		},
		{
			name:       "restating the rule's own namespace is the same request",
			items:      []string{snTenantNS},
			scope:      []configv1alpha3.ScopeNamespace{own},
			admitted:   true,
			namespaces: [][]string{{snTenantNS}},
		},
		{
			name:       "a named namespace is allowed once a grant lists it",
			items:      []string{snSourceNS},
			scope:      []configv1alpha3.ScopeNamespace{own, configv1alpha3.ScopeNamespace(snSourceNS)},
			admitted:   true,
			namespaces: [][]string{{snSourceNS}},
		},
		{
			name:     "a named namespace is refused under {consumer.namespace}",
			items:    []string{snSourceNS},
			scope:    []configv1alpha3.ScopeNamespace{own},
			admitted: false,
		},
		{
			name:       "the wildcard resolves to the cluster-wide collection under *",
			items:      []string{snWildcard},
			scope:      []configv1alpha3.ScopeNamespace{all},
			admitted:   true,
			namespaces: [][]string{{""}},
			says:       `"*" is one cluster-wide list and watch, which is the empty namespace`,
		},
		{
			name:     "the wildcard is refused under {consumer.namespace}",
			items:    []string{snWildcard},
			scope:    []configv1alpha3.ScopeNamespace{own},
			admitted: false,
			says:     "the widest request in the API must not be the one that slips through",
		},
		{
			name:     "the wildcard is refused under a list of names",
			items:    []string{snWildcard},
			scope:    []configv1alpha3.ScopeNamespace{own, configv1alpha3.ScopeNamespace(snSourceNS)},
			admitted: false,
		},
		{
			name:       "* allows a named namespace",
			items:      []string{snSourceNS},
			scope:      []configv1alpha3.ScopeNamespace{all},
			admitted:   true,
			namespaces: [][]string{{snSourceNS}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := snReader(t, snTarget(), snKubeProvider(tt.scope...))

			resolved, err := authz.ResolveWatchRuleSourceScope(
				context.Background(), reader, snRule(tt.items...), snTarget())

			require.NoError(t, err)
			assert.Equal(t, tt.admitted, resolved.Admitted(), tt.says)
			reason := authz.ReasonSourceNamespaceNotAllowed
			if tt.admitted {
				reason = authz.ReasonSourceNamespaceAllowed
			}
			assert.Equal(t, reason, resolved.Reason, tt.says)
			if tt.namespaces != nil {
				for i, want := range tt.namespaces {
					assert.Equal(t, want, resolved.NamespacesFor(i), tt.says)
				}
			}
			assert.NotEmpty(t, resolved.Message, "every verdict must explain itself")
		})
	}
}

// A rule mixing its own namespace with other ones resolves each item independently, and the
// aggregate is the one condition the object publishes.
func TestResolveWatchRuleSourceScope_MixedItemsResolveIndependently(t *testing.T) {
	reader := snReader(t, snTarget(), snKubeProvider(own, all))

	resolved, err := authz.ResolveWatchRuleSourceScope(
		context.Background(), reader, snRule("", snSourceNS, snWildcard), snTarget())

	require.NoError(t, err)
	require.True(t, resolved.Admitted())
	assert.Equal(t, []string{snTenantNS}, resolved.NamespacesFor(0))
	assert.Equal(t, []string{snSourceNS}, resolved.NamespacesFor(1))
	assert.Equal(t, []string{""}, resolved.NamespacesFor(2))
	assert.Equal(t, authz.ReasonSourceNamespaceAllowed, resolved.Reason)
}

// A denied item refuses the WHOLE rule rather than being trimmed away. Mirroring two of the three
// namespaces a rule asked for is worse than a loud failure: the operator would have no way to see
// that the third was dropped.
func TestResolveWatchRuleSourceScope_DeniedItemRefusesTheWholeRule(t *testing.T) {
	reader := snReader(t, snTarget(), snKubeProvider(own))

	resolved, err := authz.ResolveWatchRuleSourceScope(
		context.Background(), reader, snRule("", snSourceNS), snTarget())

	require.NoError(t, err)
	assert.False(t, resolved.Admitted(),
		"an authorized sibling item must not rescue a denied one")
	assert.Equal(t, authz.ReasonSourceNamespaceNotAllowed, resolved.Reason)
	assert.Contains(t, resolved.Message, "spec.rules[1]",
		"the message must name the item that decided it, since the index is what gets edited")
	assert.Contains(t, resolved.Message, `KubeProvider platform/workspaces lets namespace "tenant-acme" watch `+
		`"tenant-acme" (spec.allowedGitTargets[].sourceNamespaces); spec.rules[1] (resources configmaps, `+
		`sourceNamespace "repo-config") is not allowed`, "the refusal names the grant and the request")
}

// A GitTarget the provider does not admit at all gets no scope, however wide the grant it is not
// admitted by.
func TestResolveWatchRuleSourceScope_UnadmittedGitTargetGetsNoScope(t *testing.T) {
	denying := snKubeProvider(own, all)
	denying.Spec.AllowedGitTargets[0].ConsumerNamespaces.Names = []configv1alpha3.GrantNamespace{"some-other-tenant"}
	reader := snReader(t, snTarget(), denying)

	resolved, err := authz.ResolveWatchRuleSourceScope(
		context.Background(), reader, snRule(snSourceNS), snTarget())

	require.NoError(t, err)
	assert.False(t, resolved.Admitted())
	assert.Equal(t, authz.ReasonSourceNamespaceNotAllowed, resolved.Reason)
	assert.Contains(t, resolved.Message, "may not mirror through KubeProvider")
}

// A missing KubeProvider denies rather than erroring: a provider that does not exist grants nothing,
// which is a decision the rule's owner can act on.
func TestResolveWatchRuleSourceScope_MissingKubeProviderDenies(t *testing.T) {
	reader := snReader(t, snTarget())

	resolved, err := authz.ResolveWatchRuleSourceScope(
		context.Background(), reader, snRule(snSourceNS), snTarget())

	require.NoError(t, err)
	assert.False(t, resolved.Admitted())
	assert.Contains(t, resolved.Message, "was not found")
}

// A TRANSIENT read failure is an error, not a denial. Encoding "the apiserver blipped" as "the
// policy says no" would tear down a running stream over an outage nobody chose.
func TestResolveWatchRuleSourceScope_ProviderReadErrorIsRequeued(t *testing.T) {
	boom := errors.New("apiserver unavailable")
	reader := fake.NewClientBuilder().
		WithScheme(snScheme(t)).
		WithObjects(snTarget(), snKubeProvider(own, all)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(
				_ context.Context, _ client.WithWatch, key client.ObjectKey,
				obj client.Object, _ ...client.GetOption,
			) error {
				if _, ok := obj.(*configv1alpha3.KubeProvider); ok && key.Name == snProvider {
					return boom
				}
				return nil
			},
		}).Build()

	_, err := authz.ResolveWatchRuleSourceScope(
		context.Background(), reader, snRule(snSourceNS), snTarget())

	require.ErrorIs(t, err, boom, "a transient read must requeue, never deny")
}

// The provider is read ONCE per rule however many items ask the same question of it.
func TestResolveWatchRuleSourceScope_ProviderIsReadOncePerRule(t *testing.T) {
	reads := 0
	reader := fake.NewClientBuilder().
		WithScheme(snScheme(t)).
		WithObjects(snTarget(), snKubeProvider(own, all),
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: snTenantNS}}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(
				ctx context.Context, c client.WithWatch, key client.ObjectKey,
				obj client.Object, opts ...client.GetOption,
			) error {
				if _, ok := obj.(*configv1alpha3.KubeProvider); ok {
					reads++
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()

	resolved, err := authz.ResolveWatchRuleSourceScope(
		context.Background(), reader, snRule("a", "b", "c", snWildcard), snTarget())

	require.NoError(t, err)
	require.True(t, resolved.Admitted())
	assert.LessOrEqual(t, reads, 2,
		"the provider's verdict cannot differ within one compile, so it is read once rather than "+
			"multiplying apiserver reads by the rule's item count")
}

// The fingerprint is what the watched-type re-projection gate compares, so it must move whenever
// the RESOLVED set moves — including for the wildcard, whose resolved value is the empty string.
func TestResolvedSourceScope_Fingerprint(t *testing.T) {
	ctx := context.Background()
	reader := snReader(t, snTarget(), snKubeProvider(own, all))

	named, err := authz.ResolveWatchRuleSourceScope(ctx, reader, snRule(snSourceNS), snTarget())
	require.NoError(t, err)
	wildcard, err := authz.ResolveWatchRuleSourceScope(ctx, reader, snRule(snWildcard), snTarget())
	require.NoError(t, err)
	same, err := authz.ResolveWatchRuleSourceScope(ctx, reader, snRule(snSourceNS), snTarget())
	require.NoError(t, err)

	assert.Equal(t, named.Fingerprint(), same.Fingerprint(),
		"an unchanged resolution must fingerprint identically, or the table rebuilds forever")
	assert.NotEqual(t, named.Fingerprint(), wildcard.Fingerprint(),
		"a named namespace and the cluster-wide collection are different watches")
}

// An empty rule is vacuously authorized. It is reachable through the API only transiently, and a
// gate that errored on it would turn an empty list into a stall.
func TestResolveWatchRuleSourceScope_NoItems(t *testing.T) {
	reader := snReader(t, snTarget(), snKubeProvider(own))

	resolved, err := authz.ResolveWatchRuleSourceScope(
		context.Background(), reader, snRule(), snTarget())

	require.NoError(t, err)
	assert.True(t, resolved.Admitted())
	assert.Equal(t, authz.ReasonSourceNamespaceAllowed, resolved.Reason)
}

// NamespacesFor is indexed by the item's position in spec.rules, and the store compiles from it
// position by position. An out-of-range index returns nil rather than panicking: the resolved scope
// and the spec are two objects, and a caller reading them apart must not take the process down.
func TestResolvedSourceScope_NamespacesForIsBoundsChecked(t *testing.T) {
	reader := snReader(t, snTarget(), snKubeProvider(own, all))

	resolved, err := authz.ResolveWatchRuleSourceScope(
		context.Background(), reader, snRule("", snSourceNS), snTarget())
	require.NoError(t, err)

	assert.Equal(t, []string{snTenantNS}, resolved.NamespacesFor(0))
	assert.Nil(t, resolved.NamespacesFor(2), "past the last item")
	assert.Nil(t, resolved.NamespacesFor(-1), "before the first")
}

// The aggregate message deduplicates and sorts, so a rule whose items overlap does not report the
// same namespace twice, and the cluster-wide collection is spelled out rather than shown as the empty
// string an operator would read as a missing value.
func TestResolveWatchRuleSourceScope_AggregateMessageIsDeduplicatedAndLegible(t *testing.T) {
	reader := snReader(t, snTarget(), snKubeProvider(own, all))

	resolved, err := authz.ResolveWatchRuleSourceScope(
		context.Background(), reader, snRule(snSourceNS, snSourceNS, snWildcard), snTarget())

	require.NoError(t, err)
	require.True(t, resolved.Admitted())
	assert.Equal(t, 1, strings.Count(resolved.Message, snSourceNS),
		"a namespace two items both name is reported once")
	assert.Contains(t, resolved.Message, "every namespace (cluster-wide)",
		`the cluster-wide collection must be named, not rendered as an empty string`)
}
