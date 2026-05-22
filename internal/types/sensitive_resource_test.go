/*
SPDX-License-Identifier: Apache-2.0

Copyright 2025 ConfigButler

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

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
