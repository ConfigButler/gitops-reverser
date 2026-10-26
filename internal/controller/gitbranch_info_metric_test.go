// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	k8stypes "k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

const gitBranchInfoMetricName = "gitopsreverser_git_branch_info"

// The GitBranch reconcile publishes git_branch_info for every GitBranch, the one reporting
// BranchConflict included: it has no worker series to join, and its provider is still worth reading
// beside its conditions. Deleting a GitBranch ends its series and leaves the other's alone.
func TestGitBranch_PublishesItsInfoUntilDeleted(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	defer telemetry.ForgetGitBranchInfo("team-b", "main")

	f := newGitBranchFixture(t,
		branchProvider("platform", "acme"), branchObject("platform", "main", "acme", "main", 2*time.Hour),
		branchProvider("team-b", "mirror"), branchObject("team-b", "main", "mirror", "main", time.Hour))
	f.reconcile(t, "platform", "main")
	conflicted := f.reconcile(t, "team-b", "main")
	require.NotEqual(t, "True", string(readyOf(conflicted).Status), "precondition: team-b/main does not hold")

	info := func(namespace, provider string) bool {
		_, ok := telemetry.CollectInt64Sum(reader, gitBranchInfoMetricName, map[string]string{
			"gitbranch_namespace": namespace,
			"gitbranch_name":      "main",
			"provider_namespace":  namespace,
			"provider_name":       provider,
			"branch":              "main",
		})
		return ok
	}
	assert.True(t, info("platform", "acme"), "the holder publishes its provider and branch")
	assert.True(t, info("team-b", "mirror"), "so does a GitBranch that does not hold its branch")

	holder := k8stypes.NamespacedName{Namespace: "platform", Name: "main"}
	stored := &configbutleraiv1alpha3.GitBranch{}
	require.NoError(t, f.client.Get(context.Background(), holder, stored))
	require.NoError(t, f.client.Delete(context.Background(), stored))
	_, err = f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: holder})
	require.NoError(t, err)

	assert.False(t, info("platform", "acme"), "a deleted GitBranch must leave no info series")
	assert.True(t, info("team-b", "mirror"), "another GitBranch's series is untouched")
}
