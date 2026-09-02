// SPDX-License-Identifier: Apache-2.0

package authz_test

import (
	"context"
	"errors"
	"strings"
	"testing"

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
			ProviderRef:        configv1alpha3.GitProviderReference{Name: "acme-git"},
			ClusterProviderRef: &configv1alpha3.ClusterProviderReference{Name: snProvider},
			Branch:             "main",
			Path:               "tenants/acme",
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
			TargetRef: configv1alpha3.LocalTargetReference{Name: snTargetName},
			Rules:     items,
		},
	}
}

// snClusterProvider admits snTenantNS by name, so the "provider admits the target's namespace" leg
// of the gate passes and each case isolates the leg it is actually about.
func snClusterProvider(delegate bool) *configv1alpha3.ClusterProvider {
	return &configv1alpha3.ClusterProvider{
		ObjectMeta: metav1.ObjectMeta{Name: snProvider},
		Spec: configv1alpha3.ClusterProviderSpec{
			AccessFrom:              &configv1alpha3.NamespaceMatcher{Names: []string{snTenantNS}},
			AllowAnySourceNamespace: delegate,
		},
	}
}

func snReader(t *testing.T, objects ...client.Object) client.Reader {
	t.Helper()
	objects = append(objects,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: snTenantNS}})
	return fake.NewClientBuilder().WithScheme(snScheme(t)).WithObjects(objects...).Build()
}

// The gate is two rules and this is the table for both of them: an item on the rule's own namespace
// is free, and anything else needs the ClusterProvider's delegation.
//
// There is deliberately no row for a GitTarget policy. There is no longer such a field: what it
// bounded is bounded by the source credential's own RBAC, which this gate cannot and does not try
// to predict.
func TestResolveWatchRuleSourceScope(t *testing.T) {
	tests := []struct {
		name       string
		items      []string
		delegate   bool
		admitted   bool
		reason     string
		namespaces [][]string
		says       string
	}{
		{
			name:       "an omitted item watches the rule's own namespace, ungated",
			items:      []string{""},
			delegate:   false,
			admitted:   true,
			reason:     authz.ReasonLegacySourceNamespace,
			namespaces: [][]string{{snTenantNS}},
			says:       "gating the legacy case would break every existing WatchRule on upgrade",
		},
		{
			name:       "restating the rule's own namespace is not an override",
			items:      []string{snTenantNS},
			delegate:   false,
			admitted:   true,
			reason:     authz.ReasonLegacySourceNamespace,
			namespaces: [][]string{{snTenantNS}},
			says:       "spelling out what omission means must not need a platform-admin flag",
		},
		{
			name:       "a named override is allowed once the provider delegates",
			items:      []string{snSourceNS},
			delegate:   true,
			admitted:   true,
			reason:     authz.ReasonSourceNamespaceAllowed,
			namespaces: [][]string{{snSourceNS}},
		},
		{
			name:     "a named override is refused while the provider does not delegate",
			items:    []string{snSourceNS},
			delegate: false,
			admitted: false,
			reason:   authz.ReasonSourceNamespaceNotAllowed,
		},
		{
			name:       "the wildcard resolves to the cluster-wide cell",
			items:      []string{snWildcard},
			delegate:   true,
			admitted:   true,
			reason:     authz.ReasonSourceNamespaceAllowed,
			namespaces: [][]string{{""}},
			says:       `"*" is one cluster-wide list and watch, which is the empty namespace`,
		},
		{
			name:     "the wildcard is refused while the provider does not delegate",
			items:    []string{snWildcard},
			delegate: false,
			admitted: false,
			reason:   authz.ReasonSourceNamespaceNotAllowed,
			says:     "the widest request in the API must not be the one that slips through",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := snReader(t, snTarget(), snClusterProvider(tt.delegate))

			resolved, err := authz.ResolveWatchRuleSourceScope(
				context.Background(), reader, snRule(tt.items...), snTarget())

			require.NoError(t, err)
			assert.Equal(t, tt.admitted, resolved.Admitted(), tt.says)
			assert.Equal(t, tt.reason, resolved.Reason, tt.says)
			if tt.namespaces != nil {
				for i, want := range tt.namespaces {
					assert.Equal(t, want, resolved.NamespacesFor(i), tt.says)
				}
			}
			assert.NotEmpty(t, resolved.Message, "every verdict must explain itself")
		})
	}
}

// A rule mixing a legacy item with an authorized override resolves each independently, and the
// aggregate is the one condition the object publishes.
func TestResolveWatchRuleSourceScope_MixedItemsResolveIndependently(t *testing.T) {
	reader := snReader(t, snTarget(), snClusterProvider(true))

	resolved, err := authz.ResolveWatchRuleSourceScope(
		context.Background(), reader, snRule("", snSourceNS, snWildcard), snTarget())

	require.NoError(t, err)
	require.True(t, resolved.Admitted())
	assert.Equal(t, []string{snTenantNS}, resolved.NamespacesFor(0))
	assert.Equal(t, []string{snSourceNS}, resolved.NamespacesFor(1))
	assert.Equal(t, []string{""}, resolved.NamespacesFor(2))
	assert.Equal(t, authz.ReasonSourceNamespaceAllowed, resolved.Reason,
		"one overriding item makes the whole rule an override for reporting purposes")
}

// A denied item refuses the WHOLE rule rather than being trimmed away. Mirroring two of the three
// namespaces a rule asked for is worse than a loud failure: the operator would have no way to see
// that the third was dropped.
func TestResolveWatchRuleSourceScope_DeniedItemRefusesTheWholeRule(t *testing.T) {
	reader := snReader(t, snTarget(), snClusterProvider(false))

	resolved, err := authz.ResolveWatchRuleSourceScope(
		context.Background(), reader, snRule("", snSourceNS), snTarget())

	require.NoError(t, err)
	assert.False(t, resolved.Admitted(),
		"an authorized sibling item must not rescue a denied one")
	assert.Equal(t, authz.ReasonSourceNamespaceNotAllowed, resolved.Reason)
	assert.Contains(t, resolved.Message, "spec.rules[1]",
		"the message must name the item that decided it, since the index is what gets edited")
}

// A GitTarget the provider does not admit at all cannot delegate anything to its rules: the
// provider-side leg is checked before the flag, so a target outside accessFrom is refused even
// through a provider that delegates freely.
func TestResolveWatchRuleSourceScope_UnadmittedGitTargetCannotDelegate(t *testing.T) {
	denying := snClusterProvider(true)
	denying.Spec.AccessFrom = &configv1alpha3.NamespaceMatcher{Names: []string{"some-other-tenant"}}
	reader := snReader(t, snTarget(), denying)

	resolved, err := authz.ResolveWatchRuleSourceScope(
		context.Background(), reader, snRule(snSourceNS), snTarget())

	require.NoError(t, err)
	assert.False(t, resolved.Admitted())
	assert.Equal(t, authz.ReasonSourceNamespaceNotAllowed, resolved.Reason)
	assert.Contains(t, resolved.Message, "may not mirror through ClusterProvider")
}

// A missing ClusterProvider denies an override rather than erroring: a provider that does not exist
// delegates nothing, which is a decision the rule's owner can act on.
func TestResolveWatchRuleSourceScope_MissingClusterProviderDeniesOverride(t *testing.T) {
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
		WithObjects(snTarget(), snClusterProvider(true)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(
				_ context.Context, _ client.WithWatch, key client.ObjectKey,
				obj client.Object, _ ...client.GetOption,
			) error {
				if _, ok := obj.(*configv1alpha3.ClusterProvider); ok && key.Name == snProvider {
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
		WithObjects(snTarget(), snClusterProvider(true),
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: snTenantNS}}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(
				ctx context.Context, c client.WithWatch, key client.ObjectKey,
				obj client.Object, opts ...client.GetOption,
			) error {
				if _, ok := obj.(*configv1alpha3.ClusterProvider); ok {
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
		"the delegation verdict cannot differ within one compile, so it is memoised rather than "+
			"multiplying apiserver reads by the rule's item count")
}

// The fingerprint is what the watched-type re-projection gate compares, so it must move whenever
// the RESOLVED set moves — including for the wildcard, whose resolved value is the empty string.
func TestResolvedSourceScope_Fingerprint(t *testing.T) {
	ctx := context.Background()
	reader := snReader(t, snTarget(), snClusterProvider(true))

	named, err := authz.ResolveWatchRuleSourceScope(ctx, reader, snRule(snSourceNS), snTarget())
	require.NoError(t, err)
	wildcard, err := authz.ResolveWatchRuleSourceScope(ctx, reader, snRule(snWildcard), snTarget())
	require.NoError(t, err)
	same, err := authz.ResolveWatchRuleSourceScope(ctx, reader, snRule(snSourceNS), snTarget())
	require.NoError(t, err)

	assert.Equal(t, named.Fingerprint(), same.Fingerprint(),
		"an unchanged resolution must fingerprint identically, or the table rebuilds forever")
	assert.NotEqual(t, named.Fingerprint(), wildcard.Fingerprint(),
		"a named namespace and the cluster-wide cell are different watches")
}

// An empty rule is vacuously authorized. It is reachable through the API only transiently, and a
// gate that errored on it would turn an empty list into a stall.
func TestResolveWatchRuleSourceScope_NoItems(t *testing.T) {
	reader := snReader(t, snTarget(), snClusterProvider(false))

	resolved, err := authz.ResolveWatchRuleSourceScope(
		context.Background(), reader, snRule(), snTarget())

	require.NoError(t, err)
	assert.True(t, resolved.Admitted())
	assert.Equal(t, authz.ReasonLegacySourceNamespace, resolved.Reason)
}

// NamespacesFor is indexed by the item's position in spec.rules, and the store compiles from it
// position by position. An out-of-range index returns nil rather than panicking: the resolved scope
// and the spec are two objects, and a caller reading them apart must not take the process down.
func TestResolvedSourceScope_NamespacesForIsBoundsChecked(t *testing.T) {
	reader := snReader(t, snTarget(), snClusterProvider(true))

	resolved, err := authz.ResolveWatchRuleSourceScope(
		context.Background(), reader, snRule("", snSourceNS), snTarget())
	require.NoError(t, err)

	assert.Equal(t, []string{snTenantNS}, resolved.NamespacesFor(0))
	assert.Nil(t, resolved.NamespacesFor(2), "past the last item")
	assert.Nil(t, resolved.NamespacesFor(-1), "before the first")
}

// The aggregate message deduplicates and sorts, so a rule whose items overlap does not report the
// same namespace twice, and the cluster-wide cell is spelled out rather than shown as the empty
// string an operator would read as a missing value.
func TestResolveWatchRuleSourceScope_AggregateMessageIsDeduplicatedAndLegible(t *testing.T) {
	reader := snReader(t, snTarget(), snClusterProvider(true))

	resolved, err := authz.ResolveWatchRuleSourceScope(
		context.Background(), reader, snRule(snSourceNS, snSourceNS, snWildcard), snTarget())

	require.NoError(t, err)
	require.True(t, resolved.Admitted())
	assert.Equal(t, 1, strings.Count(resolved.Message, snSourceNS),
		"a namespace two items both name is reported once")
	assert.Contains(t, resolved.Message, "every namespace (cluster-wide)",
		`the cluster-wide cell must be named, not rendered as an empty string`)
}
