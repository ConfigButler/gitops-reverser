// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	"testing"

	meta "github.com/fluxcd/pkg/apis/meta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// AdmitsNamespace is the authorization predicate behind the reconcile-time refusal, and there is
// no admission webhook backstopping it — that reconcile call site is the ENTIRE boundary. So every
// contract below is a security boundary rather than a routing convenience: a provider holds a
// credential that can read a whole remote cluster, and referencing it from a GitTarget mirrors that
// cluster into the target's Git destination. Every namespace below is another than the provider's
// own ("platform"), which is always admitted and is covered separately.
func TestAdmitsNamespace_Authorization(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		policy  *NamespaceMatcher
		nsName  string
		labels  map[string]string
		want    bool
		wantErr bool
	}{
		{
			// Deny-by-default: a provider whose author never wrote a policy grants nothing.
			// The dangerous inversion would be treating "no policy" as "no restriction", which
			// would silently open a freshly created provider to every namespace in the cluster.
			name:   "nil policy denies",
			policy: nil,
			nsName: "team-a",
			want:   false,
		},
		{
			// A policy object that exists but says nothing is the same statement as no policy:
			// it enumerates zero namespaces, so it admits zero namespaces.
			name:   "empty policy denies",
			policy: &NamespaceMatcher{},
			nsName: "team-a",
			want:   false,
		},
		{
			name:   "listed name is allowed",
			policy: &NamespaceMatcher{Names: []string{"team-a", "team-b"}},
			nsName: "team-b",
			want:   true,
		},
		{
			// Names is an exact allow-list, never a prefix or substring match; otherwise an
			// attacker could create "team-a-evil" and inherit "team-a"'s grant.
			name:   "unlisted name is denied",
			policy: &NamespaceMatcher{Names: []string{"team-a"}},
			nsName: "team-a-evil",
			want:   false,
		},
		{
			name:   "name match is case-sensitive and exact",
			policy: &NamespaceMatcher{Names: []string{"team-a"}},
			nsName: "Team-A",
			want:   false,
		},
		{
			name: "selector match is allowed",
			policy: &NamespaceMatcher{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "prod"}},
			},
			nsName: "anything",
			labels: map[string]string{"tier": "prod"},
			want:   true,
		},
		{
			name: "selector miss is denied",
			policy: &NamespaceMatcher{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "prod"}},
			},
			nsName: "anything",
			labels: map[string]string{"tier": "dev"},
			want:   false,
		},
		{
			// Names and Selector are ORed, so a listed namespace stays allowed even when its
			// labels do not match. Requiring both would be a silent tightening that breaks
			// existing grants.
			name: "name allows even when the selector misses",
			policy: &NamespaceMatcher{
				Names:    []string{"team-a"},
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "prod"}},
			},
			nsName: "team-a",
			labels: map[string]string{"tier": "dev"},
			want:   true,
		},
		{
			name: "selector allows even when the name is unlisted",
			policy: &NamespaceMatcher{
				Names:    []string{"team-a"},
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "prod"}},
			},
			nsName: "team-z",
			labels: map[string]string{"tier": "prod"},
			want:   true,
		},
		{
			// GOTCHA worth pinning loudly: an EMPTY selector is the Kubernetes "match everything"
			// selector, not "match nothing". `selector: {}` therefore grants every namespace in
			// the cluster. It is deliberate (it is how a platform admin says "any namespace"),
			// but it looks identical to an accidentally blank field, so the behavior must be
			// asserted rather than rediscovered in production.
			name:   "empty selector matches every namespace",
			policy: &NamespaceMatcher{Selector: &metav1.LabelSelector{}},
			nsName: "any-namespace-at-all",
			want:   true,
		},
		{
			// ...including a namespace that carries no labels at all.
			name:   "empty selector matches a namespace with no labels",
			policy: &NamespaceMatcher{Selector: &metav1.LabelSelector{}},
			nsName: "bare",
			labels: nil,
			want:   true,
		},
		{
			// A malformed selector must FAIL CLOSED: the error is surfaced to the caller AND the
			// allow answer is false. Returning true on a parse failure would turn a typo in a
			// provider's policy into a cluster-wide grant.
			name: "invalid selector fails closed",
			policy: &NamespaceMatcher{
				Selector: &metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{
						{Key: "tier", Operator: "NotAnOperator", Values: []string{"prod"}},
					},
				},
			},
			nsName:  "team-a",
			labels:  map[string]string{"tier": "prod"},
			want:    false,
			wantErr: true,
		},
		{
			// The name allow-list short-circuits before the selector is parsed, so a listed
			// namespace is admitted even when the selector alongside it is malformed. Pinned
			// because it is the one path where a broken policy does not surface an error.
			name: "listed name short-circuits an invalid selector",
			policy: &NamespaceMatcher{
				Names: []string{"team-a"},
				Selector: &metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{
						{Key: "tier", Operator: "NotAnOperator"},
					},
				},
			},
			nsName: "team-a",
			labels: map[string]string{"tier": "prod"},
			want:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			provider := &KubeProvider{
				ObjectMeta: metav1.ObjectMeta{Namespace: "platform", Name: "remote"},
				Spec:       KubeProviderSpec{AccessFrom: tc.policy},
			}

			allowed, err := provider.AdmitsNamespace(tc.nsName, tc.labels)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.want, allowed)
		})
	}
}

// AdmitsNamespace admits the provider's own namespace before accessFrom is read at all: a tenant's
// provider serves its own namespace with no policy written, and even a malformed policy (which fails
// closed for every other namespace) cannot lock the owner out.
func TestAdmitsNamespace_OwnNamespaceAlwaysAdmitted(t *testing.T) {
	t.Parallel()

	invalid := &NamespaceMatcher{Selector: &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "tier", Operator: "NotAnOperator"}},
	}}
	for _, policy := range []*NamespaceMatcher{nil, {}, invalid} {
		provider := &KubeProvider{
			ObjectMeta: metav1.ObjectMeta{Namespace: "platform", Name: "remote"},
			Spec:       KubeProviderSpec{AccessFrom: policy},
		}
		admitted, err := provider.AdmitsNamespace("platform", nil)
		require.NoError(t, err)
		assert.True(t, admitted, "policy %+v", policy)
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

// TestApplyTrigger_AppliersDefaultToTheTriggersNamespace: a reference with no namespace names an
// applier in the trigger's own namespace, as Flux defaults a sourceRef.
func TestApplyTrigger_AppliersDefaultToTheTriggersNamespace(t *testing.T) {
	flux := &FluxTrigger{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a"},
		Spec: FluxTriggerSpec{
			KubeProviderRef:  meta.NamespacedObjectReference{Name: "hub"},
			KustomizationRef: meta.NamespacedObjectReference{Name: "apps"},
		},
	}
	assert.Equal(t, "team-a/apps", flux.Applier().String())
	assert.Equal(t, "team-a/hub", flux.KubeProviderKey().String())

	argo := &ArgoCDTrigger{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a"},
		Spec: ArgoCDTriggerSpec{
			ApplicationRef: meta.NamespacedObjectReference{Namespace: "argocd", Name: "apps"},
		},
	}
	assert.Equal(t, "argocd/apps", argo.Applier().String())
}

// TestArgoCDTrigger_SourceBindingNamesTheBranch: targetRevision may spell a branch as refs/heads/,
// and an empty one is Argo CD's HEAD, which follows no branch GitOps Reverser writes.
func TestArgoCDTrigger_SourceBindingNamesTheBranch(t *testing.T) {
	trigger := &ArgoCDTrigger{}
	_, _, known := trigger.SourceBinding()
	assert.False(t, known)

	for revision, branch := range map[string]string{"main": "main", "refs/heads/main": "main", "": "HEAD"} {
		trigger.Status.Source = &ArgoCDTriggerSource{RepoURL: "https://example.com/r.git", TargetRevision: revision}
		url, got, known := trigger.SourceBinding()
		assert.True(t, known)
		assert.Equal(t, "https://example.com/r.git", url)
		assert.Equal(t, branch, got, "targetRevision %q", revision)
	}
}

// TestNewApplyTrigger_KnowsBothKinds: a kind this release does not know is nil, never a guess.
// TestApplyTrigger_SharedSource: a FluxTrigger shares the GitRepository it last read; an
// ArgoCDTrigger shares nothing.
func TestApplyTrigger_SharedSource(t *testing.T) {
	flux := &FluxTrigger{}
	_, known := flux.SharedSource()
	assert.False(t, known, "a trigger that has not read its source shares none yet")

	flux.Status.Source = &FluxTriggerSource{
		GitRepository: meta.NamespacedObjectReference{Namespace: "flux-system", Name: "platform"},
	}
	source, known := flux.SharedSource()
	assert.True(t, known)
	assert.Equal(t, "flux-system/platform", source.String())

	argo := &ArgoCDTrigger{}
	_, known = argo.SharedSource()
	assert.False(t, known, "every sync pins its own revision")
}

func TestNewApplyTrigger_KnowsBothKinds(t *testing.T) {
	assert.IsType(t, &FluxTrigger{}, NewApplyTrigger(ApplyTriggerKindFlux))
	assert.IsType(t, &ArgoCDTrigger{}, NewApplyTrigger(ApplyTriggerKindArgoCD))
	assert.Nil(t, NewApplyTrigger("WebhookTrigger"))
}

// TestApplyTrigger_StatusAccessors: both kinds answer the shared interface from their own status,
// and SetOperation replaces the operation and repairPending together.
func TestApplyTrigger_StatusAccessors(t *testing.T) {
	operation := &ApplyOperation{Phase: ApplyOperationRunning, Commit: "abc"}
	flux := &FluxTrigger{}
	argo := &ArgoCDTrigger{}
	for _, trigger := range []ApplyTrigger{flux, argo} {
		assert.Nil(t, trigger.Operation())
		assert.False(t, trigger.RepairPending())
		trigger.SetOperation(operation, true)
		assert.Equal(t, operation, trigger.Operation())
		assert.True(t, trigger.RepairPending())
		trigger.SetObservedGeneration(3)
		assert.NotNil(t, trigger.StatusConditions())
	}
	assert.Equal(t, int64(3), flux.Status.ObservedGeneration)
	assert.Equal(t, int64(3), argo.Status.ObservedGeneration)

	assert.Equal(t, ApplyTriggerKindFlux, flux.TriggerKind())
	assert.Equal(t, ApplierKindKustomization, flux.ApplierKind())
	assert.Equal(t, ApplyTriggerKindArgoCD, argo.TriggerKind())
	assert.Equal(t, ApplierKindApplication, argo.ApplierKind())
	assert.Equal(t, types.NamespacedName{}, argo.KubeProviderKey(),
		"an omitted kubeProviderRef names nothing: there is no default provider")
	argo.Spec.KubeProviderRef = meta.NamespacedObjectReference{Namespace: "platform", Name: "hub"}
	assert.Equal(t, types.NamespacedName{Namespace: "platform", Name: "hub"}, argo.KubeProviderKey())

	_, _, known := flux.SourceBinding()
	assert.False(t, known)
	flux.Status.Source = &FluxTriggerSource{URL: "https://example.com/r.git", Branch: "main"}
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
