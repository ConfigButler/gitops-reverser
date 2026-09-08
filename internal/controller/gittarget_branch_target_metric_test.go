// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

const branchTargetsMetricName = "gitopsreverser_git_branch_targets"

// The delete path of the join series, wired rather than in isolation. A GitTarget that is gone must
// leave no mapping behind: the series would otherwise keep naming a deleted object as one of the
// GitTargets a live branch's push failures affect, and it would never clear.
//
// It rides the same call site as ForgetResourceConditions, which is the definitive NotFound
// reconcile rather than the deletion timestamp: an object deleted while this process was down
// produces no reconcile at all, and a fresh process publishes nothing about it either way.
func TestGitTargetCleanup_ForgetsTheBranchTargetMapping(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	defer telemetry.ForgetBranchTarget("team-a", "sibling")

	telemetry.RecordBranchTarget("team-a", "mirror", "team-a", "acme", "main")
	telemetry.RecordBranchTarget("team-a", "sibling", "team-a", "acme", "main")

	_, ok := telemetry.CollectInt64Sum(reader, branchTargetsMetricName, map[string]string{
		"gittarget_namespace": "team-a",
		"gittarget_name":      "mirror",
	})
	require.True(t, ok, "precondition: the mapping is published")

	r := &GitTargetReconciler{}
	r.cleanupDeletedGitTarget(k8stypes.NamespacedName{Namespace: "team-a", Name: "mirror"}, logf.Log)

	_, ok = telemetry.CollectInt64Sum(reader, branchTargetsMetricName, map[string]string{
		"gittarget_namespace": "team-a",
		"gittarget_name":      "mirror",
	})
	assert.False(t, ok, "a deleted GitTarget must leave no join series")

	_, ok = telemetry.CollectInt64Sum(reader, branchTargetsMetricName, map[string]string{
		"gittarget_namespace": "team-a",
		"gittarget_name":      "sibling",
	})
	assert.True(t, ok, "a sibling sharing the branch must be untouched")
}
