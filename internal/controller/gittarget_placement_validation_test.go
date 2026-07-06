// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

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
				Normal: configbutleraiv1alpha3.GitTargetPlacementClass{
					ByType:  map[string]string{"v1/configmaps": "{namespace}/configmaps.yaml"},
					Default: "all.yaml",
				},
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
				Normal: configbutleraiv1alpha3.GitTargetPlacementClass{Default: "{bogus}/all.yaml"},
			},
			false,
		},
		{
			"malformed byType key",
			&configbutleraiv1alpha3.GitTargetPlacementSpec{
				Normal: configbutleraiv1alpha3.GitTargetPlacementClass{
					ByType: map[string]string{"not-a-type-key": "all.yaml"},
				},
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
