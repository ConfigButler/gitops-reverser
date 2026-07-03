// SPDX-License-Identifier: Apache-2.0

package types

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseSensitiveResourcePolicy_MatchesBuiltInAndAdditionalTypes(t *testing.T) {
	policy, err := ParseSensitiveResourcePolicy(
		" core.cozystack.io/TenantSecrets , credentials ,core.cozystack.io/tenantsecrets ",
	)
	require.NoError(t, err)

	assert.True(t, policy.IsSensitive("", "secrets"))
	assert.True(t, policy.IsSensitive("core.cozystack.io", "tenantsecrets"))
	assert.True(t, policy.IsSensitive("core.cozystack.io", "TENANTSECRETS"))
	assert.True(t, policy.IsSensitive("", "credentials"))
	assert.False(t, policy.IsSensitive("example.io", "credentials"))
	assert.False(t, policy.IsSensitive("", "configmaps"))
	assert.Equal(t, []string{"core.cozystack.io/tenantsecrets", "credentials", "secrets"}, policy.Entries())
}

func TestParseSensitiveResourcePolicy_RejectsInvalidAdditionalEntries(t *testing.T) {
	tests := []struct {
		name       string
		additional string
	}{
		{name: "empty entry", additional: "core.cozystack.io/tenantsecrets,"},
		{name: "empty group", additional: "/tenantsecrets"},
		{name: "empty resource", additional: "core.cozystack.io/"},
		{name: "extra separator", additional: "core.cozystack.io/v1/tenantsecrets"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseSensitiveResourcePolicy(tt.additional)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "invalid additional sensitive resource")
		})
	}
}
