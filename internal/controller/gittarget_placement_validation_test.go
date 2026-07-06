// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

func TestValidatePlacementPolicy(t *testing.T) {
	cases := []struct {
		name string
		spec *configbutleraiv1alpha3.GitTargetPlacementSpec
		ok   bool
	}{
		{"nil spec is valid", nil, true},
		{"empty spec is valid", &configbutleraiv1alpha3.GitTargetPlacementSpec{}, true},
		{
			"valid normal byType and default",
			&configbutleraiv1alpha3.GitTargetPlacementSpec{
				ByType:  map[string]string{"v1/configmaps": "{namespace}/configmaps.yaml"},
				Default: "all.yaml",
			},
			true,
		},
		{
			"valid sensitive byType, identity-complete SOPS path",
			&configbutleraiv1alpha3.GitTargetPlacementSpec{
				Sensitive: configbutleraiv1alpha3.GitTargetPlacementClass{
					ByType: map[string]string{"v1/secrets": "{namespace}/secret-{name}.sops.yaml"},
				},
			},
			true,
		},
		{
			"unknown template variable",
			&configbutleraiv1alpha3.GitTargetPlacementSpec{
				Default: "{bogus}/all.yaml",
			},
			false,
		},
		{
			"normal template escapes spec.path with a parent traversal",
			&configbutleraiv1alpha3.GitTargetPlacementSpec{
				Default: "../outside.yaml",
			},
			false,
		},
		{
			"normal template does not end in a YAML suffix",
			&configbutleraiv1alpha3.GitTargetPlacementSpec{
				ByType: map[string]string{"v1/configmaps": "{namespace}/{name}.txt"},
			},
			false,
		},
		{
			"malformed byType key",
			&configbutleraiv1alpha3.GitTargetPlacementSpec{
				ByType: map[string]string{"not-a-type-key": "all.yaml"},
			},
			false,
		},
		{
			"sensitive template missing the sops suffix",
			&configbutleraiv1alpha3.GitTargetPlacementSpec{
				Sensitive: configbutleraiv1alpha3.GitTargetPlacementClass{
					ByType: map[string]string{"v1/secrets": "{namespace}/secret-{name}.yaml"},
				},
			},
			false,
		},
		{
			"sensitive default missing type variables",
			&configbutleraiv1alpha3.GitTargetPlacementSpec{
				Sensitive: configbutleraiv1alpha3.GitTargetPlacementClass{
					Default: "{namespaceOrCluster}/{name}.sops.yaml",
				},
			},
			false,
		},
		{
			"sensitive byType narrowed template needs only scope + name",
			&configbutleraiv1alpha3.GitTargetPlacementSpec{
				Sensitive: configbutleraiv1alpha3.GitTargetPlacementClass{
					ByType: map[string]string{"v1/secrets": "{namespaceOrCluster}/{name}.sops.yaml"},
				},
			},
			true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, msg := validatePlacementPolicy(tc.spec)
			if ok != tc.ok {
				t.Errorf("validatePlacementPolicy() = (%v, %q), want ok=%v", ok, msg, tc.ok)
			}
			if !tc.ok && msg == "" {
				t.Errorf("an invalid policy must carry a message")
			}
		})
	}
}

func TestValidPlacementTypeKeySyntax(t *testing.T) {
	cases := []struct {
		key  string
		want bool
	}{
		{"v1/secrets", true},
		{"apps/v1/deployments", true},
		{"cert-manager.io/v1/certificates", true},
		{"", false},
		{"v1", false},
		{"a/b/c/d", false},
		{"v1//secrets", false},
		{"/v1/secrets", false},
	}
	for _, tc := range cases {
		if got := validPlacementTypeKeySyntax(tc.key); got != tc.want {
			t.Errorf("validPlacementTypeKeySyntax(%q) = %v, want %v", tc.key, got, tc.want)
		}
	}
}

// TestEvaluateValidatedGate_InvalidPlacementPolicy proves the wiring, not just the
// pure function: an otherwise-valid GitTarget (provider found, branch allowed, no
// path conflict) whose declared placement policy is invalid must fail the
// Validated gate with reason InvalidConfig, not just make validatePlacementPolicy
// return false in isolation.
func TestEvaluateValidatedGate_InvalidPlacementPolicy(t *testing.T) {
	const ns = "default"
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, configbutleraiv1alpha3.AddToScheme(scheme))

	provider := &configbutleraiv1alpha3.GitProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "provider-a", Namespace: ns},
		Spec: configbutleraiv1alpha3.GitProviderSpec{
			AllowedBranches: []string{"main"},
		},
	}
	target := &configbutleraiv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "target-a", Namespace: ns},
		Spec: configbutleraiv1alpha3.GitTargetSpec{
			ProviderRef: configbutleraiv1alpha3.GitProviderReference{Name: "provider-a"},
			Branch:      "main",
			Path:        "apps",
			Placement: &configbutleraiv1alpha3.GitTargetPlacementSpec{
				Default: "{bogus}/all.yaml",
			},
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(provider, target).Build()
	reconciler := &GitTargetReconciler{Client: client}

	validated, msg, result, err := reconciler.evaluateValidatedGate(context.Background(), target, ns)

	require.NoError(t, err)
	assert.Nil(t, result)
	assert.False(t, validated)
	assert.Contains(t, msg, GitTargetReasonInvalidConfig)

	cond := apimeta.FindStatusCondition(target.Status.Conditions, GitTargetConditionValidated)
	require.NotNil(t, cond, "Validated condition must be set")
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, GitTargetReasonInvalidConfig, cond.Reason)
	assert.Contains(t, cond.Message, "bogus")
}
