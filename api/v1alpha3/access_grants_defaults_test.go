// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// crdSpecFieldDefault is the default the generated CRD gives spec.<field> of its first version.
func crdSpecFieldDefault(t *testing.T, file, field string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases", file))
	require.NoError(t, err)
	var crd apiextensionsv1.CustomResourceDefinition
	require.NoError(t, yaml.Unmarshal(raw, &crd))
	require.NotEmpty(t, crd.Spec.Versions)
	prop, ok := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties[field]
	require.True(t, ok, "%s has no spec.%s", file, field)
	require.NotNil(t, prop.Default, "%s spec.%s has no default", file, field)
	return prop.Default.Raw
}

// TestGrantDefaultsMatchCRDs keeps the Go defaults the grant methods use for a nil list equal to the
// defaults the API server applies from the CRDs.
func TestGrantDefaultsMatchCRDs(t *testing.T) {
	t.Run("GitBranch allowedGitTargets", func(t *testing.T) {
		assertCRDDefault(t, "configbutler.ai_gitbranches.yaml", "allowedGitTargets", DefaultGitBranchGitTargetGrants())
	})
	t.Run("KubeProvider allowedGitTargets", func(t *testing.T) {
		assertCRDDefault(t, "configbutler.ai_kubeproviders.yaml", "allowedGitTargets",
			DefaultKubeProviderGitTargetGrants())
	})
	t.Run("KubeProvider allowedApplierClaims", func(t *testing.T) {
		assertCRDDefault(t, "configbutler.ai_kubeproviders.yaml", "allowedApplierClaims",
			DefaultKubeProviderApplierClaimGrants())
	})
}

// assertCRDDefault asserts that the CRD's default for spec.<field> decodes to want.
func assertCRDDefault[G any](t *testing.T, file, field string, want []G) {
	t.Helper()
	var got []G
	require.NoError(t, json.Unmarshal(crdSpecFieldDefault(t, file, field), &got))
	assert.Equal(t, want, got)
}

// TestGrantLists_EmptySurvivesSerialization pins that [] stays [] on the wire, so an explicit deny-all
// is never sent as an omitted field that the API server would default back to a grant.
func TestGrantLists_EmptySurvivesSerialization(t *testing.T) {
	branch := GitBranchSpec{AllowedGitTargets: []GitBranchGitTargetGrant{}}
	provider := KubeProviderSpec{
		AllowedGitTargets:    []KubeProviderGitTargetGrant{},
		AllowedApplierClaims: []KubeProviderApplierClaimGrant{},
	}
	for name, spec := range map[string]any{"GitBranch": branch, "KubeProvider": provider} {
		raw, err := json.Marshal(spec)
		require.NoError(t, err)
		var fields map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(raw, &fields))
		for _, field := range []string{"allowedGitTargets", "allowedApplierClaims"} {
			if name == "GitBranch" && field == "allowedApplierClaims" {
				continue
			}
			assert.JSONEq(t, "[]", string(fields[field]), "%s spec.%s", name, field)
		}
	}

	var decoded GitBranchSpec
	require.NoError(t, json.Unmarshal([]byte(`{"allowedGitTargets":[]}`), &decoded))
	assert.NotNil(t, decoded.AllowedGitTargets, "a decoded [] stays distinguishable from omitted")
}
