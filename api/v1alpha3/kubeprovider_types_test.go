// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	"testing"
	"time"

	meta "github.com/fluxcd/pkg/apis/meta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func kubeProvider(namespace, name, route string, created time.Time) KubeProvider {
	p := KubeProvider{ObjectMeta: metav1.ObjectMeta{
		Namespace: namespace, Name: name, UID: types.UID(namespace + "-" + name),
		CreationTimestamp: metav1.NewTime(created),
	}}
	if route != "" {
		p.Spec.Attribution = &KubeProviderAttribution{AuditRoute: route}
	}
	return p
}

// TestKubeProvider_AuditRoute covers the default every provider without a route resolves to:
// <namespace>.<name>, which names its own namespace first so no other namespace can resolve to it.
func TestKubeProvider_AuditRoute(t *testing.T) {
	t0 := time.Now()
	assert.Equal(t, "tenant-a.prod", new(kubeProvider("tenant-a", "prod", "", t0)).AuditRoute())

	empty := kubeProvider("tenant-a", "prod", "", t0)
	empty.Spec.Attribution = &KubeProviderAttribution{}
	assert.Equal(t, "tenant-a.prod", empty.AuditRoute(), "an empty auditRoute is the default")

	assert.Equal(t, "prod-eu-1", new(kubeProvider("tenant-a", "prod", "prod-eu-1", t0)).AuditRoute())
}

func TestKubeProvider_AdmitsNamespace(t *testing.T) {
	p := kubeProvider("platform", "remote", "", time.Now())

	admitted, err := p.AdmitsNamespace("platform", nil)
	require.NoError(t, err)
	assert.True(t, admitted, "a provider admits its own namespace without accessFrom")

	admitted, err = p.AdmitsNamespace("tenant-a", nil)
	require.NoError(t, err)
	assert.False(t, admitted, "omitted accessFrom admits no other namespace")

	p.Spec.AccessFrom = &NamespaceMatcher{Names: []string{"tenant-a"}}
	admitted, err = p.AdmitsNamespace("tenant-a", nil)
	require.NoError(t, err)
	assert.True(t, admitted)
}

func TestKubeProvider_CredentialAndGrants(t *testing.T) {
	p := kubeProvider("platform", "local", "", time.Now())
	assert.False(t, p.IsInCluster())
	assert.False(t, p.AllowsCrossNamespaceSources())
	assert.False(t, p.AllowsCrossNamespaceAppliers())

	p.Spec.InCluster = &KubeProviderInCluster{}
	p.Spec.AllowCrossNamespace = &KubeProviderCrossNamespace{Sources: true, Appliers: true}
	assert.True(t, p.IsInCluster())
	assert.True(t, p.AllowsCrossNamespaceSources())
	assert.True(t, p.AllowsCrossNamespaceAppliers())
	assert.Equal(t, types.NamespacedName{Namespace: "platform", Name: "local"}, p.Key())
}

// TestAuditRouteHolder pins who holds a route that providers in several namespaces carry.
func TestAuditRouteHolder(t *testing.T) {
	t0 := time.Now()
	older, younger := t0, t0.Add(time.Minute)

	tests := []struct {
		name      string
		route     string
		providers []KubeProvider
		want      string
		held      bool
	}{
		{
			name:      "nobody carries the route",
			route:     "shared",
			providers: []KubeProvider{kubeProvider("a", "p", "other", older)},
		},
		{
			name:  "the oldest carrier's namespace holds a route that names no namespace",
			route: "shared",
			providers: []KubeProvider{
				kubeProvider("b", "p", "shared", younger),
				kubeProvider("a", "p", "shared", older),
			},
			want: "a", held: true,
		},
		{
			name:  "the namespace a route names holds it against an older carrier elsewhere",
			route: "a.prod",
			providers: []KubeProvider{
				kubeProvider("b", "squatter", "a.prod", older),
				kubeProvider("a", "prod", "", younger),
			},
			want: "a", held: true,
		},
		{
			name:  "a route naming a namespace that does not carry it falls back to the oldest",
			route: "c.prod",
			providers: []KubeProvider{
				kubeProvider("b", "p", "c.prod", younger),
				kubeProvider("a", "p", "c.prod", older),
			},
			want: "a", held: true,
		},
		{
			name:  "equal creation times break on UID",
			route: "shared",
			providers: []KubeProvider{
				kubeProvider("b", "p", "shared", older),
				kubeProvider("a", "p", "shared", older),
			},
			want: "a", held: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, held := AuditRouteHolder(tt.route, tt.providers)
			assert.Equal(t, tt.held, held)
			assert.Equal(t, tt.want, got)
		})
	}

	deleting := kubeProvider("a", "p", "shared", older)
	deleting.DeletionTimestamp = &metav1.Time{Time: younger}
	got, held := AuditRouteHolder("shared", []KubeProvider{deleting, kubeProvider("b", "p", "shared", younger)})
	assert.True(t, held)
	assert.Equal(t, "b", got, "a provider being deleted holds nothing")
}

func TestKubeProviderKey_DefaultsToReferrerNamespace(t *testing.T) {
	target := &GitTarget{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a"}}
	assert.Equal(t, types.NamespacedName{}, target.KubeProviderKey(), "an unset reference names nothing")

	target.Spec.KubeProviderRef = meta.NamespacedObjectReference{Name: "remote"}
	assert.Equal(t, types.NamespacedName{Namespace: "tenant-a", Name: "remote"}, target.KubeProviderKey())

	flux := &FluxClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a"}}
	flux.Spec.KubeProviderRef = meta.NamespacedObjectReference{Name: "mgmt", Namespace: "platform"}
	assert.Equal(t, types.NamespacedName{Namespace: "platform", Name: "mgmt"}, flux.KubeProviderKey())

	argo := &ArgoCDClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a"}}
	argo.Spec.KubeProviderRef = meta.NamespacedObjectReference{Name: "mgmt"}
	assert.Equal(t, types.NamespacedName{Namespace: "tenant-a", Name: "mgmt"}, argo.KubeProviderKey())
}
