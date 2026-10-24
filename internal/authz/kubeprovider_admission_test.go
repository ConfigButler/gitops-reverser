// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

const (
	testNS       = "team-a"
	platformNS   = "platform"
	platformPrev = "platform-old"
)

func admissionScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, configv1alpha3.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))
	return s
}

// targetIn builds a GitTarget in testNS referencing the KubeProvider providerName in platformNS
// ("" sets no kubeProviderRef at all).
func targetIn(providerName string) *configv1alpha3.GitTarget {
	t := &configv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "mirror", Namespace: testNS},
	}
	if providerName != "" {
		t.Spec.KubeProviderRef = meta.NamespacedObjectReference{Name: providerName, Namespace: platformNS}
	}
	return t
}

// grantList is grants written out, never omitted: no grants is [], which admits nobody, where an
// omitted list would be the default grant for the provider's own namespace.
func grantList[G any](grants []G) []G {
	if grants == nil {
		return []G{}
	}
	return grants
}

// providerNamed builds a KubeProvider in platformNS with grants as its spec.allowedGitTargets, so
// the GitTarget in testNS is admitted only through a grant.
func providerNamed(name string, grants ...configv1alpha3.KubeProviderGitTargetGrant) *configv1alpha3.KubeProvider {
	return &configv1alpha3.KubeProvider{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: platformNS},
		Spec:       configv1alpha3.KubeProviderSpec{AllowedGitTargets: grantList(grants)},
	}
}

// grantNames converts namespace names into a grant's names.
func grantNames(namespaces ...string) []configv1alpha3.GrantNamespace {
	names := make([]configv1alpha3.GrantNamespace, 0, len(namespaces))
	for _, ns := range namespaces {
		names = append(names, configv1alpha3.GrantNamespace(ns))
	}
	return names
}

// mirrorFrom is a grant that admits namespaces by name to mirror their own namespace.
func mirrorFrom(namespaces ...string) configv1alpha3.KubeProviderGitTargetGrant {
	return configv1alpha3.KubeProviderGitTargetGrant{
		ConsumerNamespaces: configv1alpha3.ConsumerNamespaces{
			From:  configv1alpha3.GrantFromNames,
			Names: grantNames(namespaces...),
		},
		SourceNamespaces: []configv1alpha3.ScopeNamespace{configv1alpha3.ConsumerNamespaceVariable},
	}
}

func namespaceLabeled(labels map[string]string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNS, Labels: labels}}
}

func TestGitTargetAdmitted_Policy(t *testing.T) {
	ownNamespace := func(grants ...configv1alpha3.KubeProviderGitTargetGrant) *configv1alpha3.KubeProvider {
		return &configv1alpha3.KubeProvider{
			ObjectMeta: metav1.ObjectMeta{Name: "local", Namespace: testNS},
			Spec:       configv1alpha3.KubeProviderSpec{AllowedGitTargets: grantList(grants)},
		}
	}
	ownTarget := &configv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "mirror", Namespace: testNS},
		Spec:       configv1alpha3.GitTargetSpec{KubeProviderRef: meta.NamespacedObjectReference{Name: "local"}},
	}
	tenant := map[string]string{"tenant": "true"}
	tests := []struct {
		name        string
		objects     []client.Object
		target      *configv1alpha3.GitTarget
		wantAllowed bool
		wantReason  string
		wantMessage string
	}{
		{
			name:        "provider missing is a hard denial, not an implicit allow",
			objects:     []client.Object{namespaceLabeled(nil)},
			target:      targetIn("prod-eu-1"),
			wantAllowed: false,
			wantReason:  ReasonKubeProviderNotFound,
			wantMessage: "referenced KubeProvider platform/prod-eu-1 was not found",
		},
		{
			// No reference names nothing; there is no provider an omitted reference falls back to.
			name:        "an unset kubeProviderRef is denied, never defaulted",
			objects:     []client.Object{namespaceLabeled(nil)},
			target:      targetIn(""),
			wantAllowed: false,
			wantReason:  ReasonKubeProviderNotFound,
			wantMessage: "spec.kubeProviderRef names no KubeProvider",
		},
		{
			// Nothing is automatic, the provider's own namespace included.
			name:        "the provider's own namespace is refused by an empty list",
			objects:     []client.Object{ownNamespace(), namespaceLabeled(nil)},
			target:      ownTarget,
			wantReason:  ReasonNamespaceNotAuthorized,
			wantMessage: `KubeProvider team-a/local admits no namespace in spec.allowedGitTargets; namespace "team-a"`,
		},
		{
			name: "from: Same admits the provider's own namespace",
			objects: []client.Object{ownNamespace(configv1alpha3.KubeProviderGitTargetGrant{
				ConsumerNamespaces: configv1alpha3.ConsumerNamespaces{From: configv1alpha3.GrantFromSame},
				SourceNamespaces:   []configv1alpha3.ScopeNamespace{"{consumer.namespace}"},
			}), namespaceLabeled(nil)},
			target:      ownTarget,
			wantAllowed: true,
		},
		{
			name: "from: Same admits nothing else",
			objects: []client.Object{providerNamed("prod-eu-1", configv1alpha3.KubeProviderGitTargetGrant{
				ConsumerNamespaces: configv1alpha3.ConsumerNamespaces{From: configv1alpha3.GrantFromSame},
				SourceNamespaces:   []configv1alpha3.ScopeNamespace{"*"},
			}),
				namespaceLabeled(nil)},
			target:      targetIn("prod-eu-1"),
			wantReason:  ReasonNamespaceNotAuthorized,
			wantMessage: `KubeProvider platform/prod-eu-1 admits platform in spec.allowedGitTargets; namespace "team-a"`,
		},
		{
			name: "an omitted list admits the provider's own namespace, mirroring its own",
			objects: []client.Object{&configv1alpha3.KubeProvider{
				ObjectMeta: metav1.ObjectMeta{Name: "local", Namespace: testNS},
			}, namespaceLabeled(nil)},
			target:      ownTarget,
			wantAllowed: true,
		},
		{
			name: "an omitted list admits no other namespace",
			objects: []client.Object{&configv1alpha3.KubeProvider{
				ObjectMeta: metav1.ObjectMeta{Name: "prod-eu-1", Namespace: platformNS},
			}, namespaceLabeled(nil)},
			target:      targetIn("prod-eu-1"),
			wantReason:  ReasonNamespaceNotAuthorized,
			wantMessage: "admits platform in spec.allowedGitTargets",
		},
		{
			name:        "an empty list denies every namespace",
			objects:     []client.Object{providerNamed("prod-eu-1"), namespaceLabeled(nil)},
			target:      targetIn("prod-eu-1"),
			wantReason:  ReasonNamespaceNotAuthorized,
			wantMessage: "admits no namespace in spec.allowedGitTargets",
		},
		{
			name: "a Names grant admits",
			objects: []client.Object{
				providerNamed("prod-eu-1", mirrorFrom("team-b", "team-a")),
				namespaceLabeled(nil),
			},
			target:      targetIn("prod-eu-1"),
			wantAllowed: true,
		},
		{
			name:        "a Names grant not listing the namespace denies, naming what it admits",
			objects:     []client.Object{providerNamed("prod-eu-1", mirrorFrom("team-b")), namespaceLabeled(nil)},
			target:      targetIn("prod-eu-1"),
			wantReason:  ReasonNamespaceNotAuthorized,
			wantMessage: `KubeProvider platform/prod-eu-1 admits team-b in spec.allowedGitTargets; namespace "team-a" is not admitted`,
		},
		{
			name: "a Selector grant admits a namespace by its labels",
			objects: []client.Object{providerNamed("prod-eu-1", configv1alpha3.KubeProviderGitTargetGrant{
				ConsumerNamespaces: configv1alpha3.ConsumerNamespaces{
					From:     configv1alpha3.GrantFromSelector,
					Selector: &metav1.LabelSelector{MatchLabels: tenant},
				},
				SourceNamespaces: []configv1alpha3.ScopeNamespace{"{consumer.namespace}"},
			}), namespaceLabeled(tenant)},
			target:      targetIn("prod-eu-1"),
			wantAllowed: true,
		},
		{
			name: "a Selector grant does not admit a namespace without the labels",
			objects: []client.Object{providerNamed("prod-eu-1", configv1alpha3.KubeProviderGitTargetGrant{
				ConsumerNamespaces: configv1alpha3.ConsumerNamespaces{
					From:     configv1alpha3.GrantFromSelector,
					Selector: &metav1.LabelSelector{MatchLabels: tenant},
				},
				SourceNamespaces: []configv1alpha3.ScopeNamespace{"{consumer.namespace}"},
			}), namespaceLabeled(nil)},
			target:      targetIn("prod-eu-1"),
			wantReason:  ReasonNamespaceNotAuthorized,
			wantMessage: "admits namespaces labelled tenant=true",
		},
		{
			// A namespace not created yet has no labels: a Names grant still admits it.
			name:        "a Names grant admits a namespace that does not exist yet",
			objects:     []client.Object{providerNamed("prod-eu-1", mirrorFrom("team-a"))},
			target:      targetIn("prod-eu-1"),
			wantAllowed: true,
		},
		{
			// A malformed selector must FAIL CLOSED, and say so.
			name: "an invalid selector denies, and the message says why",
			objects: []client.Object{providerNamed("prod-eu-1", configv1alpha3.KubeProviderGitTargetGrant{
				ConsumerNamespaces: configv1alpha3.ConsumerNamespaces{
					From: configv1alpha3.GrantFromSelector,
					Selector: &metav1.LabelSelector{
						MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "tier", Operator: "NotAnOperator"}},
					},
				},
				SourceNamespaces: []configv1alpha3.ScopeNamespace{"*"},
			}), namespaceLabeled(nil)},
			target:      targetIn("prod-eu-1"),
			wantReason:  ReasonNamespaceNotAuthorized,
			wantMessage: "a grant's selector is invalid",
		},
		{
			name: "from: All admits every namespace",
			objects: []client.Object{providerNamed("prod-eu-1", configv1alpha3.KubeProviderGitTargetGrant{
				ConsumerNamespaces: configv1alpha3.ConsumerNamespaces{From: configv1alpha3.GrantFromAll},
				SourceNamespaces:   []configv1alpha3.ScopeNamespace{"{consumer.namespace}"},
			}),
				namespaceLabeled(nil)},
			target:      targetIn("prod-eu-1"),
			wantAllowed: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reader := fake.NewClientBuilder().
				WithScheme(admissionScheme(t)).
				WithObjects(tc.objects...).
				Build()

			decision, err := GitTargetAdmitted(context.Background(), reader, tc.target)
			require.NoError(t, err)

			assert.Equal(t, tc.wantAllowed, decision.Allowed, decision.Message)
			if tc.wantAllowed {
				assert.Empty(t, decision.Reason, "an admission carries no reason")
				assert.Empty(t, decision.Message, "an admission carries no message")
				return
			}
			assert.Equal(t, tc.wantReason, decision.Reason)
			assert.NotEmpty(t, decision.Message, "a denial must always explain itself")
			if tc.wantMessage != "" {
				assert.Contains(t, decision.Message, tc.wantMessage)
			}
		})
	}
}

// TestGitTargetAdmitted_ReadErrorsSurfaceAsError pins the difference that decides whether a
// transient apiserver failure tears down a running data plane: a read error must come back as err
// (caller requeues, keeps the stream) and never as a denial (caller stops the stream).
func TestGitTargetAdmitted_ReadErrorsSurfaceAsError(t *testing.T) {
	boom := errors.New("apiserver unavailable")

	tests := []struct {
		name    string
		failOn  func(key client.ObjectKey, obj client.Object) bool
		wantMsg string
	}{
		{
			name: "KubeProvider read failure",
			failOn: func(_ client.ObjectKey, obj client.Object) bool {
				_, ok := obj.(*configv1alpha3.KubeProvider)
				return ok
			},
			wantMsg: "read KubeProvider platform/prod-eu-1",
		},
		{
			name: "Namespace read failure",
			failOn: func(_ client.ObjectKey, obj client.Object) bool {
				_, ok := obj.(*corev1.Namespace)
				return ok
			},
			wantMsg: `read namespace "team-a"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cl := fake.NewClientBuilder().WithScheme(admissionScheme(t)).
				WithObjects(
					providerNamed("prod-eu-1", mirrorFrom("team-a")),
					namespaceLabeled(nil),
				).
				WithInterceptorFuncs(interceptor.Funcs{
					Get: func(
						ctx context.Context, c client.WithWatch, key client.ObjectKey,
						obj client.Object, opts ...client.GetOption,
					) error {
						if tc.failOn(key, obj) {
							return boom
						}
						return c.Get(ctx, key, obj, opts...)
					},
				}).Build()

			decision, err := GitTargetAdmitted(context.Background(), cl, targetIn("prod-eu-1"))

			require.Error(t, err)
			require.ErrorIs(t, err, boom)
			assert.Contains(t, err.Error(), tc.wantMsg)
			assert.False(t, decision.Allowed, "an errored decision must not read as admitted")
			assert.Empty(t, decision.Reason,
				"an errored decision must carry no denial reason: callers must requeue, not refuse")
		})
	}
}

// claimOn is a FluxClaim in testNS on the KubeProvider cluster, holding the Kustomization
// applierNamespace/apps.
func claimOn(cluster, applierNamespace string) configv1alpha3.ApplierClaim {
	return &configv1alpha3.FluxClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "apps"},
		Spec: configv1alpha3.FluxClaimSpec{
			KubeProviderRef:  meta.NamespacedObjectReference{Name: cluster, Namespace: platformNS},
			KustomizationRef: meta.NamespacedObjectReference{Name: "apps", Namespace: applierNamespace},
		},
	}
}

// appliersFrom is a grant that admits namespaces by name to hold appliers in scope.
func appliersFrom(
	scope []configv1alpha3.ScopeNamespace,
	namespaces ...string,
) configv1alpha3.KubeProviderApplierClaimGrant {
	return configv1alpha3.KubeProviderApplierClaimGrant{
		ConsumerNamespaces: configv1alpha3.ConsumerNamespaces{
			From:  configv1alpha3.GrantFromNames,
			Names: grantNames(namespaces...),
		},
		ApplierNamespaces: scope,
	}
}

// TestApplierClaimAdmitted: an applier claim needs its own grant, and the applier must live in the
// scope that grant gives. An explicit list is the whole scope.
func TestApplierClaimAdmitted(t *testing.T) {
	own := []configv1alpha3.ScopeNamespace{"{consumer.namespace}"}
	provider := func(grants ...configv1alpha3.KubeProviderApplierClaimGrant) *configv1alpha3.KubeProvider {
		p := providerNamed("hub", mirrorFrom(testNS))
		p.Spec.AllowedApplierClaims = grantList(grants)
		return p
	}
	tests := []struct {
		name        string
		provider    *configv1alpha3.KubeProvider
		applierNS   string
		wantAllowed bool
		wantReason  string
		wantMessage string
	}{
		{"admitted to mirror is not admitted to apply", provider(), testNS, false, ReasonNamespaceNotAuthorized,
			`KubeProvider platform/hub admits no namespace in spec.allowedApplierClaims; namespace "team-a"`},
		{"{consumer.namespace} admits an applier in the claim's own namespace",
			provider(appliersFrom(own, testNS)), testNS, true, "", ""},
		{"{consumer.namespace} refuses an applier elsewhere", provider(appliersFrom(own, testNS)), "flux-system", false,
			ReasonApplierNamespaceNotAllowed, `KubeProvider platform/hub lets namespace "team-a" hold appliers in ` +
				`"team-a" (spec.allowedApplierClaims); Kustomization flux-system/apps is in namespace "flux-system"`},
		{"an explicit list refuses the claim's own namespace",
			provider(appliersFrom([]configv1alpha3.ScopeNamespace{"flux-system"}, testNS)), testNS, false,
			ReasonApplierNamespaceNotAllowed, `hold appliers in "flux-system"`},
		{
			"an explicit list allows what it lists",
			provider(
				appliersFrom([]configv1alpha3.ScopeNamespace{"flux-system"}, testNS),
			),
			"flux-system",
			true,
			"",
			"",
		},
		{"* allows every namespace",
			provider(appliersFrom([]configv1alpha3.ScopeNamespace{"*"}, testNS)), "anywhere", true, "", ""},
		{"a grant for another namespace admits nothing here",
			provider(appliersFrom(own, "team-b")), testNS, false, ReasonNamespaceNotAuthorized,
			"admits team-b in spec.allowedApplierClaims"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reader := fake.NewClientBuilder().WithScheme(admissionScheme(t)).
				WithObjects(tc.provider, namespaceLabeled(nil)).Build()
			decision, err := ApplierClaimAdmitted(context.Background(), reader, claimOn("hub", tc.applierNS))
			require.NoError(t, err)
			assert.Equal(t, tc.wantAllowed, decision.Allowed, decision.Message)
			assert.Equal(t, tc.wantReason, decision.Reason)
			assert.Contains(t, decision.Message, tc.wantMessage)
		})
	}
}

// TestApplierClaimAdmitted_OwnNamespace: an omitted allowedApplierClaims admits an applier claim in
// the provider's own namespace; [] refuses it, also one the provider admits to mirror.
func TestApplierClaimAdmitted_OwnNamespace(t *testing.T) {
	provider := &configv1alpha3.KubeProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "local", Namespace: testNS},
		Spec: configv1alpha3.KubeProviderSpec{AllowedGitTargets: []configv1alpha3.KubeProviderGitTargetGrant{
			{
				ConsumerNamespaces: configv1alpha3.ConsumerNamespaces{From: configv1alpha3.GrantFromSame},
				SourceNamespaces:   []configv1alpha3.ScopeNamespace{"{consumer.namespace}"},
			},
		}},
	}
	provider.Spec.AllowedApplierClaims = []configv1alpha3.KubeProviderApplierClaimGrant{}
	claim := &configv1alpha3.FluxClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "apps"},
		Spec: configv1alpha3.FluxClaimSpec{
			KubeProviderRef:  meta.NamespacedObjectReference{Name: "local"},
			KustomizationRef: meta.NamespacedObjectReference{Name: "apps"},
		},
	}
	reader := fake.NewClientBuilder().
		WithScheme(admissionScheme(t)).
		WithObjects(provider, namespaceLabeled(nil)).
		Build()
	decision, err := ApplierClaimAdmitted(context.Background(), reader, claim)
	require.NoError(t, err)
	assert.False(t, decision.Allowed)
	assert.Equal(t, ReasonNamespaceNotAuthorized, decision.Reason)
	assert.Contains(t, decision.Message, "KubeProvider team-a/local admits no namespace in spec.allowedApplierClaims")

	provider.Spec.AllowedApplierClaims = nil
	reader = fake.NewClientBuilder().
		WithScheme(admissionScheme(t)).
		WithObjects(provider, namespaceLabeled(nil)).
		Build()
	decision, err = ApplierClaimAdmitted(context.Background(), reader, claim)
	require.NoError(t, err)
	assert.True(t, decision.Allowed, "omitted, the default grant admits the provider's own namespace")
}

// TestApplierAdmitted: asking an applier to apply needs both admissions, always. Mirroring and
// applying are granted separately, also on one KubeProvider from one namespace.
func TestApplierAdmitted(t *testing.T) {
	own := []configv1alpha3.ScopeNamespace{"{consumer.namespace}"}
	withAppliers := func(p *configv1alpha3.KubeProvider, grants ...configv1alpha3.KubeProviderApplierClaimGrant) *configv1alpha3.KubeProvider {
		p.Spec.AllowedApplierClaims = grantList(grants)
		return p
	}
	tests := []struct {
		name        string
		objects     []client.Object
		claim       configv1alpha3.ApplierClaim
		wantAllowed bool
		wantReason  string
		wantMessage string
	}{
		{
			name: "one provider granting both",
			objects: []client.Object{
				withAppliers(providerNamed("prod-eu-1", mirrorFrom(testNS)), appliersFrom(own, testNS)),
			},
			claim:       claimOn("prod-eu-1", testNS),
			wantAllowed: true,
		},
		{
			// The GitTarget and its applier claim are in one namespace on one KubeProvider; the
			// shortcut that once admitted this without reading allowedApplierClaims is gone.
			name:        "mirroring allowed, applying denied, on one provider",
			objects:     []client.Object{providerNamed("prod-eu-1", mirrorFrom(testNS))},
			claim:       claimOn("prod-eu-1", testNS),
			wantReason:  ReasonNamespaceNotAuthorized,
			wantMessage: "spec.allowedApplierClaims",
		},
		{
			name: "another provider that admits the applier claim",
			objects: []client.Object{providerNamed("prod-eu-1", mirrorFrom(testNS)),
				withAppliers(providerNamed("hub"), appliersFrom(own, testNS))},
			claim:       claimOn("hub", testNS),
			wantAllowed: true,
		},
		{
			name:        "another provider that does not exist",
			objects:     []client.Object{providerNamed("prod-eu-1", mirrorFrom(testNS))},
			claim:       claimOn("hub", testNS),
			wantReason:  ReasonKubeProviderNotFound,
			wantMessage: "FluxClaim " + testNS + "/apps spec.kubeProviderRef",
		},
		{
			name: "the source's denial comes first",
			objects: []client.Object{providerNamed("prod-eu-1", mirrorFrom("elsewhere")),
				withAppliers(providerNamed("hub"), appliersFrom(own, testNS))},
			claim:       claimOn("hub", testNS),
			wantReason:  ReasonNamespaceNotAuthorized,
			wantMessage: "KubeProvider platform/prod-eu-1",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reader := fake.NewClientBuilder().WithScheme(admissionScheme(t)).WithObjects(tc.objects...).Build()
			decision, err := ApplierAdmitted(context.Background(), reader, targetIn("prod-eu-1"), tc.claim)
			require.NoError(t, err)
			assert.Equal(t, tc.wantAllowed, decision.Allowed, decision.Message)
			assert.Equal(t, tc.wantReason, decision.Reason)
			assert.Contains(t, decision.Message, tc.wantMessage)
		})
	}
}

// TestAuditRouteHeld pins that a provider whose route another namespace holds is not used by any
// consumer, and that the holder and its own namespace's siblings are.
func TestAuditRouteHeld(t *testing.T) {
	older := metav1.NewTime(metav1.Now().Add(-time.Hour))
	withRoute := func(namespace, name, route string, created metav1.Time) *configv1alpha3.KubeProvider {
		p := &configv1alpha3.KubeProvider{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, CreationTimestamp: created},
			Spec: configv1alpha3.KubeProviderSpec{
				AllowedGitTargets: []configv1alpha3.KubeProviderGitTargetGrant{mirrorFrom(testNS)},
			},
		}
		if route != "" {
			p.Spec.Attribution = &configv1alpha3.KubeProviderAttribution{AuditRoute: route}
		}
		return p
	}
	holder := withRoute(platformPrev, "prod", "shared", older)
	sibling := withRoute(platformPrev, "prod-2", "shared", metav1.Now())
	borrower := withRoute(platformNS, "prod-eu-1", "shared", metav1.Now())

	reader := fake.NewClientBuilder().WithScheme(admissionScheme(t)).
		WithObjects(holder, sibling, borrower, namespaceLabeled(nil)).Build()

	for _, p := range []*configv1alpha3.KubeProvider{holder, sibling} {
		decision, err := AuditRouteHeld(context.Background(), reader, p)
		require.NoError(t, err)
		assert.True(t, decision.Allowed, "%s shares a route its own namespace holds", p.Key())
	}

	decision, err := AuditRouteHeld(context.Background(), reader, borrower)
	require.NoError(t, err)
	assert.False(t, decision.Allowed)
	assert.Equal(t, ReasonAuditRouteConflict, decision.Reason)
	assert.Contains(t, decision.Message, `namespace "platform-old"`)
	assert.Contains(t, decision.Message, `"platform.prod-eu-1"`, "the message names the default to fall back to")

	// The consumer of the borrowing provider is refused with the same reason, before any watch.
	admitted, err := GitTargetAdmitted(context.Background(), reader, targetIn("prod-eu-1"))
	require.NoError(t, err)
	assert.False(t, admitted.Allowed)
	assert.Equal(t, ReasonAuditRouteConflict, admitted.Reason)
}

// TestAuditRouteHeld_ListErrorSurfacesAsError: a failed list must requeue, never refuse.
func TestAuditRouteHeld_ListErrorSurfacesAsError(t *testing.T) {
	boom := errors.New("apiserver unavailable")
	reader := fake.NewClientBuilder().WithScheme(admissionScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return boom
			},
		}).Build()
	decision, err := AuditRouteHeld(context.Background(), reader, providerNamed("prod-eu-1"))
	require.ErrorIs(t, err, boom)
	assert.False(t, decision.Allowed)
	assert.Empty(t, decision.Reason)
}
