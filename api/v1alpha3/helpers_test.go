// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	"time"

	"testing"

	meta "github.com/fluxcd/pkg/apis/meta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// IsInCluster must follow spec.kubeConfig ALONE. Nothing may key "is this the operator's own
// cluster?" off the provider name: "default" is only the name an omitted clusterProviderRef
// resolves to, and it may just as well carry a kubeConfig for a remote cluster. If a name ever
// became a proxy for in-cluster-ness, a remote provider named "default" would be dialed with the
// operator's own credentials against the wrong cluster.
func TestIsInCluster_FollowsKubeConfigNotName(t *testing.T) {
	t.Parallel()

	secretRef := &meta.KubeConfigReference{
		SecretRef: &meta.SecretKeyReference{Name: "remote-kubeconfig"},
	}

	tests := []struct {
		name     string
		provider ClusterProvider
		want     bool
	}{
		{
			name:     "omitted kubeConfig means the operator's own cluster",
			provider: ClusterProvider{ObjectMeta: metav1.ObjectMeta{Name: "prod"}},
			want:     true,
		},
		{
			name: "the name \"default\" does not by itself mean in-cluster",
			provider: ClusterProvider{
				ObjectMeta: metav1.ObjectMeta{Name: DefaultClusterProviderName},
				Spec:       ClusterProviderSpec{KubeConfig: secretRef},
			},
			want: false,
		},
		{
			name: "a non-default name without kubeConfig is still in-cluster",
			provider: ClusterProvider{
				ObjectMeta: metav1.ObjectMeta{Name: "some-remote-sounding-name"},
			},
			want: true,
		},
		{
			name: "any kubeConfig means remote",
			provider: ClusterProvider{
				ObjectMeta: metav1.ObjectMeta{Name: "prod"},
				Spec:       ClusterProviderSpec{KubeConfig: secretRef},
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

// AllowsNamespace is the authorization predicate behind the reconcile-time refusal, and there is
// no admission webhook backstopping it — that reconcile call site is the ENTIRE boundary. So every
// contract below is a security boundary rather than a routing convenience: a cluster-scoped
// provider holds a credential that can read a whole remote cluster, and referencing it from a
// GitTarget mirrors that cluster into the target's Git destination.
func TestAllowsNamespace_Authorization(t *testing.T) {
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

			provider := &ClusterProvider{Spec: ClusterProviderSpec{AccessFrom: tc.policy}}

			allowed, err := provider.AllowsNamespace(tc.nsName, tc.labels)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.want, allowed)
		})
	}
}

// SourceCluster must always return a concrete, non-empty name so callers never have to handle a ""
// sentinel: it is the fact-index key, the GVK→GVR registry key, and the /audit-webhook/<name>
// route. An empty value there would collapse distinct clusters onto one key.
func TestSourceCluster_DefaultsToDefaultProviderName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ref  *meta.LocalObjectReference
		want string
	}{
		{
			name: "omitted ref resolves to the default provider name",
			ref:  nil,
			want: DefaultClusterProviderName,
		},
		{
			// An empty Name is rejected by CRD validation, but the helper must not depend on
			// admission having run — an unvalidated object still has to yield a usable key.
			name: "empty name resolves to the default provider name",
			ref:  &meta.LocalObjectReference{Name: ""},
			want: DefaultClusterProviderName,
		},
		{
			name: "explicit name is returned verbatim",
			ref:  &meta.LocalObjectReference{Name: "prod-eu"},
			want: "prod-eu",
		},
		{
			name: "explicitly naming default is the same as omitting the ref",
			ref: &meta.LocalObjectReference{
				Name: DefaultClusterProviderName,
			},
			want: DefaultClusterProviderName,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			target := &GitTarget{Spec: GitTargetSpec{ClusterProviderRef: tc.ref}}

			assert.Equal(t, tc.want, target.SourceCluster())
			assert.NotEmpty(t, target.SourceCluster(), "SourceCluster must never return the empty sentinel")
		})
	}
}

// IsLocalSource is a NAME test, not a claim about the physical cluster — a provider named
// "default" is free to carry a kubeConfig and point at a remote cluster. It exists only to supply
// the pre-discovery default for SourceClusterReachable, so it must stay a pure derivation of
// SourceCluster and never be read as "this GitTarget is definitely watching the operator's own
// cluster".
func TestIsLocalSource_IsANameTest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ref  *meta.LocalObjectReference
		want bool
	}{
		{name: "omitted ref is local", ref: nil, want: true},
		{name: "empty name is local", ref: &meta.LocalObjectReference{Name: ""}, want: true},
		{
			name: "explicit default is local",
			ref:  &meta.LocalObjectReference{Name: DefaultClusterProviderName},
			want: true,
		},
		{name: "any other name is not local", ref: &meta.LocalObjectReference{Name: "prod-eu"}, want: false},
		{
			// Near-miss names must not be treated as the default; the comparison is exact.
			name: "a name that merely contains \"default\" is not local",
			ref:  &meta.LocalObjectReference{Name: "default-eu"},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			target := &GitTarget{Spec: GitTargetSpec{ClusterProviderRef: tc.ref}}

			assert.Equal(t, tc.want, target.IsLocalSource())
		})
	}
}

// TestApplyTrigger_AppliersDefaultToTheTriggersNamespace: a reference with no namespace names an
// applier in the trigger's own namespace, as Flux defaults a sourceRef.
func TestApplyTrigger_AppliersDefaultToTheTriggersNamespace(t *testing.T) {
	flux := &FluxTrigger{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a"},
		Spec: FluxTriggerSpec{
			ClusterProviderRef: &meta.LocalObjectReference{Name: "hub"},
			KustomizationRef:   meta.NamespacedObjectReference{Name: "apps"},
		},
	}
	assert.Equal(t, "team-a/apps", flux.Applier().String())
	assert.Equal(t, "hub", flux.ClusterProvider())
	assert.Equal(t, DefaultApplierQuietPeriod, flux.EffectiveQuietPeriod())

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
// TestApplyTrigger_SharedSource: a FluxTrigger shares the GitRepository it last read, and prefetches
// only when its status says so; an ArgoCDTrigger shares nothing and never prefetches.
func TestApplyTrigger_SharedSource(t *testing.T) {
	flux := &FluxTrigger{}
	_, known := flux.SharedSource()
	assert.False(t, known, "a trigger that has not read its source shares none yet")
	assert.False(t, flux.Prefetches())

	flux.Status.Source = &FluxTriggerSource{
		GitRepository: meta.NamespacedObjectReference{Namespace: "flux-system", Name: "platform"},
		Prefetch:      true,
	}
	source, known := flux.SharedSource()
	assert.True(t, known)
	assert.Equal(t, "flux-system/platform", source.String())
	assert.True(t, flux.Prefetches())

	argo := &ArgoCDTrigger{}
	_, known = argo.SharedSource()
	assert.False(t, known, "every sync pins its own revision")
	assert.False(t, argo.Prefetches())
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
	quiet := &metav1.Duration{Duration: time.Minute}
	flux := &FluxTrigger{Spec: FluxTriggerSpec{QuietPeriod: quiet}}
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
	assert.Equal(t, time.Minute, flux.EffectiveQuietPeriod())
	assert.Equal(t, ApplyTriggerKindArgoCD, argo.TriggerKind())
	assert.Equal(t, ApplierKindApplication, argo.ApplierKind())
	assert.Equal(t, DefaultApplierQuietPeriod, argo.EffectiveQuietPeriod())
	assert.Equal(t, DefaultClusterProviderName, argo.ClusterProvider(),
		"an omitted clusterProviderRef is the default provider, as for a GitTarget")
	argo.Spec.ClusterProviderRef = &meta.LocalObjectReference{Name: "hub"}
	assert.Equal(t, "hub", argo.ClusterProvider())

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
