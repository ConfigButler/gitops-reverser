// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseServedNamespaces_EmptyServesEveryNamespace(t *testing.T) {
	for _, raw := range []string{"", "  "} {
		served, err := ParseServedNamespaces(raw)
		require.NoError(t, err)
		assert.True(t, served.All())
		assert.True(t, served.Serves("anything"))
		assert.Nil(t, served.Names())
	}
}

func TestParseServedNamespaces_ServesOnlyTheNamedNamespaces(t *testing.T) {
	served, err := ParseServedNamespaces(" team-b,team-a ,team-b")
	require.NoError(t, err)

	assert.False(t, served.All())
	assert.True(t, served.Serves("team-a"))
	assert.True(t, served.Serves("team-b"))
	assert.False(t, served.Serves("team-c"))
	assert.False(t, served.Serves(""), "a cluster-scoped object's empty namespace is not served")
	assert.Equal(t, []string{"team-a", "team-b"}, served.Names())
}

func TestParseServedNamespaces_RefusesWhatIsNotANamespaceName(t *testing.T) {
	for _, raw := range []string{"team-a,", ",team-a", "team-a,,team-b", "Team-A", "team_a", "team-a/x"} {
		_, err := ParseServedNamespaces(raw)
		assert.Error(t, err, raw)
	}
}
