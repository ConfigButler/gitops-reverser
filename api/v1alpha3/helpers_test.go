// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	"testing"

	meta "github.com/fluxcd/pkg/apis/meta"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// IsInCluster must follow spec.inCluster ALONE. Nothing may key "is this the operator's own
// cluster?" off the provider's name, and nothing may read an ABSENT credential as the operator's own
// identity: a provider that sets neither arm (unvalidated, or malformed) is not in-cluster. If a
// name or an omission ever became a proxy for in-cluster-ness, a provider meant for a remote cluster
// would be served with the operator's own credentials against the wrong cluster.
func TestIsInCluster_FollowsInClusterNotName(t *testing.T) {
	t.Parallel()

	secretRef := &meta.SecretKeyReference{Name: "remote-kubeconfig"}

	tests := []struct {
		name     string
		provider KubeProvider
		want     bool
	}{
		{
			name: "spec.inCluster means the operator's own cluster",
			provider: KubeProvider{
				ObjectMeta: metav1.ObjectMeta{Namespace: "platform", Name: "prod"},
				Spec:       KubeProviderSpec{InCluster: &KubeProviderInCluster{}},
			},
			want: true,
		},
		{
			name: "the name \"default\" does not by itself mean in-cluster",
			provider: KubeProvider{
				ObjectMeta: metav1.ObjectMeta{Namespace: "platform", Name: "default"},
				Spec:       KubeProviderSpec{SecretRef: secretRef},
			},
			want: false,
		},
		{
			name: "a provider setting neither arm never falls through to the operator's identity",
			provider: KubeProvider{
				ObjectMeta: metav1.ObjectMeta{Namespace: "platform", Name: "some-name"},
			},
			want: false,
		},
		{
			name: "a secretRef means remote",
			provider: KubeProvider{
				ObjectMeta: metav1.ObjectMeta{Namespace: "platform", Name: "prod"},
				Spec:       KubeProviderSpec{SecretRef: secretRef},
			},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, tc.provider.IsInCluster())
		})
	}
}

// SourceCluster is the id the watch data plane keys a source cluster on: the referenced
// KubeProvider's namespace/name, its namespace defaulted to the GitTarget's own. It keys the GVK→GVR
// registry and the watch context, so two providers sharing a name in two namespaces must yield two
// ids, or one tenant's watches would be served from another tenant's cluster.
func TestSourceCluster_IsTheProvidersNamespacedName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ref  meta.NamespacedObjectReference
		want string
	}{
		{
			name: "an omitted namespace is the GitTarget's own",
			ref:  meta.NamespacedObjectReference{Name: "prod-eu"},
			want: "tenant-a/prod-eu",
		},
		{
			name: "an explicit namespace is returned verbatim",
			ref:  meta.NamespacedObjectReference{Namespace: "platform", Name: "prod-eu"},
			want: "platform/prod-eu",
		},
		{
			name: "restating the GitTarget's own namespace is the same as omitting it",
			ref:  meta.NamespacedObjectReference{Namespace: "tenant-a", Name: "prod-eu"},
			want: "tenant-a/prod-eu",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			target := &GitTarget{
				ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a"},
				Spec:       GitTargetSpec{KubeProviderRef: tc.ref},
			}

			assert.Equal(t, tc.want, target.SourceCluster())
		})
	}

	other := &GitTarget{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-b"},
		Spec:       GitTargetSpec{KubeProviderRef: meta.NamespacedObjectReference{Name: "prod-eu"}},
	}
	assert.Equal(t, "tenant-b/prod-eu", other.SourceCluster(),
		"one provider name in another namespace is another source cluster")

	// The config plane's id is the empty string; even an unset reference must never collapse onto
	// it, or a GitTarget naming no provider would be served the operator's own identity.
	unset := &GitTarget{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a"}}
	assert.NotEmpty(t, unset.SourceCluster())
}

// TestApplierClaim_AppliersDefaultToTheClaimsNamespace: a reference with no namespace names an
// applier in the applier claim's own namespace, as Flux defaults a sourceRef.
func TestApplierClaim_AppliersDefaultToTheClaimsNamespace(t *testing.T) {
	flux := &FluxClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a"},
		Spec: FluxClaimSpec{
			KubeProviderRef:  meta.NamespacedObjectReference{Name: "hub"},
			KustomizationRef: meta.NamespacedObjectReference{Name: "apps"},
		},
	}
	assert.Equal(t, "team-a/apps", flux.Applier().String())
	assert.Equal(t, "team-a/hub", flux.KubeProviderKey().String())

	argo := &ArgoCDClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a"},
		Spec: ArgoCDClaimSpec{
			ApplicationRef: meta.NamespacedObjectReference{Namespace: "argocd", Name: "apps"},
		},
	}
	assert.Equal(t, "argocd/apps", argo.Applier().String())
}

// TestArgoCDClaim_SourceBindingNamesTheBranch: targetRevision may spell a branch as refs/heads/,
// and an empty one is Argo CD's HEAD, which follows no branch GitOps Reverser writes.
func TestArgoCDClaim_SourceBindingNamesTheBranch(t *testing.T) {
	claim := &ArgoCDClaim{}
	_, _, known := claim.SourceBinding()
	assert.False(t, known)

	for revision, branch := range map[string]string{"main": "main", "refs/heads/main": "main", "": "HEAD"} {
		claim.Status.Source = &ArgoCDClaimSource{RepoURL: "https://example.com/r.git", TargetRevision: revision}
		url, got, known := claim.SourceBinding()
		assert.True(t, known)
		assert.Equal(t, "https://example.com/r.git", url)
		assert.Equal(t, branch, got, "targetRevision %q", revision)
	}
}

// TestNewApplierClaim_KnowsBothKinds: a kind this release does not know is nil, never a guess.
// TestApplierClaim_SharedSource: a FluxClaim shares the GitRepository it last read; an
// ArgoCDClaim shares nothing.
func TestApplierClaim_SharedSource(t *testing.T) {
	flux := &FluxClaim{}
	_, known := flux.SharedSource()
	assert.False(t, known, "an applier claim that has not read its source shares none yet")

	flux.Status.Source = &FluxClaimSource{
		GitRepository: meta.NamespacedObjectReference{Namespace: "flux-system", Name: "platform"},
	}
	source, known := flux.SharedSource()
	assert.True(t, known)
	assert.Equal(t, "flux-system/platform", source.String())

	argo := &ArgoCDClaim{}
	_, known = argo.SharedSource()
	assert.False(t, known, "every sync pins its own revision")
}

func TestNewApplierClaim_KnowsBothKinds(t *testing.T) {
	assert.IsType(t, &FluxClaim{}, NewApplierClaim(ApplierClaimKindFlux))
	assert.IsType(t, &ArgoCDClaim{}, NewApplierClaim(ApplierClaimKindArgoCD))
	assert.Nil(t, NewApplierClaim("WebhookClaim"))
}

// TestApplierClaim_StatusAccessors: both kinds answer the shared interface from their own status,
// and SetOperation replaces the operation and repairPending together.
func TestApplierClaim_StatusAccessors(t *testing.T) {
	operation := &ApplyOperation{Phase: ApplyOperationRunning, Commit: "abc"}
	flux := &FluxClaim{}
	argo := &ArgoCDClaim{}
	for _, claim := range []ApplierClaim{flux, argo} {
		assert.Nil(t, claim.Operation())
		assert.False(t, claim.RepairPending())
		claim.SetOperation(operation, true)
		assert.Equal(t, operation, claim.Operation())
		assert.True(t, claim.RepairPending())
		claim.SetObservedGeneration(3)
		assert.NotNil(t, claim.StatusConditions())
	}
	assert.Equal(t, int64(3), flux.Status.ObservedGeneration)
	assert.Equal(t, int64(3), argo.Status.ObservedGeneration)

	assert.Equal(t, ApplierClaimKindFlux, flux.ClaimKind())
	assert.Equal(t, ApplierKindKustomization, flux.ApplierKind())
	assert.Equal(t, ApplierClaimKindArgoCD, argo.ClaimKind())
	assert.Equal(t, ApplierKindApplication, argo.ApplierKind())
	assert.Equal(t, types.NamespacedName{}, argo.KubeProviderKey(),
		"an omitted kubeProviderRef names nothing: there is no default provider")
	argo.Spec.KubeProviderRef = meta.NamespacedObjectReference{Namespace: "platform", Name: "hub"}
	assert.Equal(t, types.NamespacedName{Namespace: "platform", Name: "hub"}, argo.KubeProviderKey())

	_, _, known := flux.SourceBinding()
	assert.False(t, known)
	flux.Status.Source = &FluxClaimSource{URL: "https://example.com/r.git", Branch: "main"}
	url, branch, known := flux.SourceBinding()
	assert.True(t, known)
	assert.Equal(t, "https://example.com/r.git", url)
	assert.Equal(t, "main", branch)
}

// TestApplyOperationPhase_Completed: only Succeeded and Failed end an operation.
func TestApplyOperationPhase_Completed(t *testing.T) {
	for phase, completed := range map[ApplyOperationPhase]bool{
		ApplyOperationRequested: false, ApplyOperationRunning: false, ApplyOperationSettling: false,
		ApplyOperationUnresolved: false, ApplyOperationSucceeded: true, ApplyOperationFailed: true,
	} {
		assert.Equal(t, completed, phase.Completed(), phase)
	}
}
