// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const branchInfoMetric = "gitopsreverser_git_branch_info"

// The info series carries the GitBranch identity the worker series carry, so a join on
// {gitbranch_namespace, gitbranch_name} recovers the GitProvider and the branch for any of them.
func TestGitBranchInfo_NamesTheProviderAndBranch(t *testing.T) {
	reader, err := InitTestExporter()
	require.NoError(t, err)
	defer ForgetGitBranchInfo("team-a", "acme-main")

	RecordGitBranchInfo("team-a", "acme-main", "platform", "acme", "main")

	value, ok := CollectInt64Sum(reader, branchInfoMetric, map[string]string{
		"gitbranch_namespace": "team-a",
		"gitbranch_name":      "acme-main",
		"provider_namespace":  "platform",
		"provider_name":       "acme",
		"branch":              "main",
	})
	require.True(t, ok, "the join keys must match gitBranchAttrs' key names exactly")
	assert.Equal(t, int64(1), value, "an info gauge is always 1; the labels carry the mapping")
}

// The delete path: an info series that outlives its GitBranch names a provider for a branch nobody
// writes, and forgetting one GitBranch leaves another on the same provider alone.
func TestGitBranchInfo_ForgetStopsOnlyThatSeries(t *testing.T) {
	reader, err := InitTestExporter()
	require.NoError(t, err)
	defer ForgetGitBranchInfo("team-a", "acme-docs")

	RecordGitBranchInfo("team-a", "acme-main", "team-a", "acme", "main")
	RecordGitBranchInfo("team-a", "acme-docs", "team-a", "acme", "docs")
	ForgetGitBranchInfo("team-a", "acme-main")

	_, ok := CollectInt64Sum(reader, branchInfoMetric, map[string]string{"gitbranch_name": "acme-main"})
	assert.False(t, ok, "a deleted GitBranch must leave no series, not a stale 1")
	_, ok = CollectInt64Sum(reader, branchInfoMetric, map[string]string{"gitbranch_name": "acme-docs"})
	assert.True(t, ok, "a GitBranch on the same provider must be untouched")
}
