// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
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
		policy  *AllowedNamespaces
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
			policy: &AllowedNamespaces{},
			nsName: "team-a",
			want:   false,
		},
		{
			name:   "listed name is allowed",
			policy: &AllowedNamespaces{Names: []string{"team-a", "team-b"}},
			nsName: "team-b",
			want:   true,
		},
		{
			// Names is an exact allow-list, never a prefix or substring match; otherwise an
			// attacker could create "team-a-evil" and inherit "team-a"'s grant.
			name:   "unlisted name is denied",
			policy: &AllowedNamespaces{Names: []string{"team-a"}},
			nsName: "team-a-evil",
			want:   false,
		},
		{
			name:   "name match is case-sensitive and exact",
			policy: &AllowedNamespaces{Names: []string{"team-a"}},
			nsName: "Team-A",
			want:   false,
		},
		{
			name: "selector match is allowed",
			policy: &AllowedNamespaces{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "prod"}},
			},
			nsName: "anything",
			labels: map[string]string{"tier": "prod"},
			want:   true,
		},
		{
			name: "selector miss is denied",
			policy: &AllowedNamespaces{
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
			policy: &AllowedNamespaces{
				Names:    []string{"team-a"},
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "prod"}},
			},
			nsName: "team-a",
			labels: map[string]string{"tier": "dev"},
			want:   true,
		},
		{
			name: "selector allows even when the name is unlisted",
			policy: &AllowedNamespaces{
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
			policy: &AllowedNamespaces{Selector: &metav1.LabelSelector{}},
			nsName: "any-namespace-at-all",
			want:   true,
		},
		{
			// ...including a namespace that carries no labels at all.
			name:   "empty selector matches a namespace with no labels",
			policy: &AllowedNamespaces{Selector: &metav1.LabelSelector{}},
			nsName: "bare",
			labels: nil,
			want:   true,
		},
		{
			// A malformed selector must FAIL CLOSED: the error is surfaced to the caller AND the
			// allow answer is false. Returning true on a parse failure would turn a typo in a
			// provider's policy into a cluster-wide grant.
			name: "invalid selector fails closed",
			policy: &AllowedNamespaces{
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
			policy: &AllowedNamespaces{
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

			provider := &ClusterProvider{Spec: ClusterProviderSpec{AllowedNamespaces: tc.policy}}

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
		ref  *ClusterProviderReference
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
			ref:  &ClusterProviderReference{Name: ""},
			want: DefaultClusterProviderName,
		},
		{
			name: "explicit name is returned verbatim",
			ref:  &ClusterProviderReference{Name: "prod-eu"},
			want: "prod-eu",
		},
		{
			name: "explicitly naming default is the same as omitting the ref",
			ref: &ClusterProviderReference{
				Group: "configbutler.ai",
				Kind:  "ClusterProvider",
				Name:  DefaultClusterProviderName,
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
		ref  *ClusterProviderReference
		want bool
	}{
		{name: "omitted ref is local", ref: nil, want: true},
		{name: "empty name is local", ref: &ClusterProviderReference{Name: ""}, want: true},
		{
			name: "explicit default is local",
			ref:  &ClusterProviderReference{Name: DefaultClusterProviderName},
			want: true,
		},
		{name: "any other name is not local", ref: &ClusterProviderReference{Name: "prod-eu"}, want: false},
		{
			// Near-miss names must not be treated as the default; the comparison is exact.
			name: "a name that merely contains \"default\" is not local",
			ref:  &ClusterProviderReference{Name: "default-eu"},
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
