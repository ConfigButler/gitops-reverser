// SPDX-License-Identifier: Apache-2.0

package authz_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/authz"
)

// Admission rejects each of these on write, so only an object written by an EARLIER release
// reaches this. That is precisely the population that admission cannot reach and that most needs
// telling, which is why a stored value is refused rather than ignored.
func TestSupersededFieldRefusal(t *testing.T) {
	tests := []struct {
		name    string
		obj     any
		refused bool
		names   []string
	}{
		{
			name: "a clean GitTarget",
			obj: &configv1alpha3.GitTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "t", Namespace: "ns"},
			},
		},
		{
			name: "a GitTarget still carrying allowedSourceNamespaces",
			obj: &configv1alpha3.GitTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "t", Namespace: "ns"},
				Spec: configv1alpha3.GitTargetSpec{
					//nolint:staticcheck // setting the removed field is the point.
					AllowedSourceNamespaces: &configv1alpha3.NamespaceMatcher{Names: []string{"a"}},
				},
			},
			refused: true,
			// The message has to carry the semantic change too: a target that had a policy is
			// exactly the one for which "*" widens, and it is the only place that will be read.
			names: []string{"spec.allowedSourceNamespaces", "allowAnySourceNamespace", `"*"`},
		},
		{
			name: "an EMPTY declared policy is still a stored policy",
			obj: &configv1alpha3.GitTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "t", Namespace: "ns"},
				Spec: configv1alpha3.GitTargetSpec{
					//nolint:staticcheck // setting the removed field is the point.
					AllowedSourceNamespaces: &configv1alpha3.NamespaceMatcher{},
				},
			},
			refused: true,
			names:   []string{"spec.allowedSourceNamespaces"},
		},
		{
			name: "a clean GitProvider",
			obj: &configv1alpha3.GitProvider{
				ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
				Spec: configv1alpha3.GitProviderSpec{
					Commit: &configv1alpha3.CommitSpec{
						Committer: &configv1alpha3.CommitterSpec{Name: "Bot"},
					},
				},
			},
		},
		{
			name: "a GitProvider still carrying spec.push",
			obj: &configv1alpha3.GitProvider{
				ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
				Spec: configv1alpha3.GitProviderSpec{
					//nolint:staticcheck // setting the relocated field is the point.
					Push: &configv1alpha3.PushStrategy{CommitWindow: ptr.To("30s")},
				},
			},
			refused: true,
			// "STOPPED" is the operational fact an operator needs from this message: the mirror is
			// not writing until they edit the object.
			names: []string{"spec.push", "GitTarget.spec.commit.window", "STOPPED"},
		},
		{
			name: "a GitProvider carrying BOTH relocated fields names both",
			obj: &configv1alpha3.GitProvider{
				ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
				Spec: configv1alpha3.GitProviderSpec{
					//nolint:staticcheck // setting the relocated fields is the point.
					Push: &configv1alpha3.PushStrategy{CommitWindow: ptr.To("30s")},
					Commit: &configv1alpha3.CommitSpec{
						//nolint:staticcheck // setting the relocated field is the point.
						Message: &configv1alpha3.CommitMessageSpec{EventTemplate: "x"},
					},
				},
			},
			refused: true,
			names: []string{
				"spec.push and spec.commit.message",
				"GitTarget.spec.commit.window and GitTarget.spec.commit.message",
			},
		},
		{
			name: "a clean ClusterProvider",
			obj: &configv1alpha3.ClusterProvider{
				ObjectMeta: metav1.ObjectMeta{Name: "cp"},
				Spec: configv1alpha3.ClusterProviderSpec{
					AccessFrom:              &configv1alpha3.NamespaceMatcher{Names: []string{"ns"}},
					AllowAnySourceNamespace: true,
				},
			},
		},
		{
			name: "a ClusterProvider still carrying allowedNamespaces",
			obj: &configv1alpha3.ClusterProvider{
				ObjectMeta: metav1.ObjectMeta{Name: "cp"},
				Spec: configv1alpha3.ClusterProviderSpec{
					//nolint:staticcheck // setting the renamed field is the point.
					AllowedNamespaces: &configv1alpha3.NamespaceMatcher{Names: []string{"ns"}},
				},
			},
			refused: true,
			names:   []string{"spec.allowedNamespaces", "spec.accessFrom"},
		},
		{
			// Ignoring a stored `true` would REVOKE a delegation a platform admin granted, which is
			// the sharpest silent change any of these four fields could make.
			name: "a ClusterProvider still carrying allowSourceNamespaceOverride: true",
			obj: &configv1alpha3.ClusterProvider{
				ObjectMeta: metav1.ObjectMeta{Name: "cp"},
				Spec: configv1alpha3.ClusterProviderSpec{
					AccessFrom: &configv1alpha3.NamespaceMatcher{Names: []string{"ns"}},
					//nolint:staticcheck // setting the renamed field is the point.
					AllowSourceNamespaceOverride: ptr.To(true),
				},
			},
			refused: true,
			names:   []string{"spec.allowSourceNamespaceOverride", "spec.allowAnySourceNamespace"},
		},
		{
			// THE upgrade-safety case. This field carried +kubebuilder:default=false, so the
			// apiserver wrote it into every stored ClusterProvider — the chart-owned "default" one
			// included — whether or not anyone used the feature. Refusing it would refuse every
			// existing install, and `kubectl apply` cannot remove a server-defaulted field because
			// it was never in the user's manifest. An unfixable upgrade; hence not refused.
			name: "a ClusterProvider carrying the DEFAULTED allowSourceNamespaceOverride: false",
			obj: &configv1alpha3.ClusterProvider{
				ObjectMeta: metav1.ObjectMeta{Name: "cp"},
				Spec: configv1alpha3.ClusterProviderSpec{
					AccessFrom: &configv1alpha3.NamespaceMatcher{Names: []string{"ns"}},
					//nolint:staticcheck // setting the renamed field is the point.
					AllowSourceNamespaceOverride: ptr.To(false),
				},
			},
		},
		{
			name: "an unrelated kind is never refused",
			obj:  &configv1alpha3.WatchRule{ObjectMeta: metav1.ObjectMeta{Name: "w", Namespace: "ns"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			refusal := authz.SupersededFieldRefusal(tt.obj)

			if !tt.refused {
				assert.Empty(t, refusal)
				return
			}
			assert.NotEmpty(t, refusal)
			for _, want := range tt.names {
				assert.Contains(t, refusal, want,
					"the refusal must name the field and its replacement, since the object is the "+
						"only place an operator will look")
			}
		})
	}
}
