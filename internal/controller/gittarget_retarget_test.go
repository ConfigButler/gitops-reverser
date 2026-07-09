// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

func targetAt(branch, path string) *configbutleraiv1alpha3.GitTarget {
	return &configbutleraiv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "acme", Namespace: "team-a", Generation: 1},
		Spec: configbutleraiv1alpha3.GitTargetSpec{
			ProviderRef: configbutleraiv1alpha3.GitProviderReference{Name: "prov"},
			Branch:      branch,
			Path:        path,
		},
	}
}

// materializedAtMainApps stamps the destination the GitTarget's content currently lives at.
// Every fixture here materialized at main:apps first; the spec is what moves.
func materializedAtMainApps(target *configbutleraiv1alpha3.GitTarget) *configbutleraiv1alpha3.GitTarget {
	target.Status.ObservedDestination = &configbutleraiv1alpha3.GitTargetDestination{Branch: "main", Path: "apps"}
	return target
}

func TestDestinationMoved(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		target *configbutleraiv1alpha3.GitTarget
		want   bool
	}{
		"never materialized": {
			// Absent observedDestination is not a move: the target has not arrived anywhere.
			target: targetAt("main", "apps"),
			want:   false,
		},
		"settled": {
			target: materializedAtMainApps(targetAt("main", "apps")),
			want:   false,
		},
		"path changed": {
			target: materializedAtMainApps(targetAt("main", "clusters/acme")),
			want:   true,
		},
		"branch changed": {
			target: materializedAtMainApps(targetAt("live", "apps")),
			want:   true,
		},
		"trailing slash is not a move": {
			// spec.path normalizes a trailing slash away, so "apps/" and "apps" name one folder.
			target: materializedAtMainApps(targetAt("main", "apps/")),
			want:   false,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, destinationMoved(tc.target))
		})
	}
}

// The teardown is generation-scoped, not a plain "is Retargeting true?", so that a second
// destination change arriving mid-retarget tears down again instead of quietly continuing
// to build the first one.
func TestRetargetAlreadyTornDown(t *testing.T) {
	t.Parallel()

	target := materializedAtMainApps(targetAt("live", "apps"))
	assert.False(t, retargetAlreadyTornDown(target), "no Retargeting condition yet")

	r := &GitTargetReconciler{}
	r.setCondition(target, GitTargetConditionRetargeting, metav1.ConditionTrue,
		GitTargetReasonDestinationChanged, "moving")
	assert.True(t, retargetAlreadyTornDown(target))

	// A second spec change bumps the generation, which makes the recorded teardown stale.
	target.Generation = 2
	assert.False(t, retargetAlreadyTornDown(target),
		"a destination change arriving mid-retarget must tear down again")

	// A False condition is never a completed teardown, whatever its generation.
	r.setCondition(target, GitTargetConditionRetargeting, metav1.ConditionFalse,
		GitTargetReasonDestinationSettled, "settled")
	assert.False(t, retargetAlreadyTornDown(target))
}

func TestBeginRetarget_MarksRetargetingAndClearsPushTime(t *testing.T) {
	t.Parallel()

	target := materializedAtMainApps(targetAt("main", "clusters/acme"))
	now := metav1.Now()
	target.Status.LastPushTime = &now

	r := &GitTargetReconciler{}
	require.NoError(t, r.beginRetarget(t.Context(), target, logr.Discard()))

	c := conditionByType(target.Status.Conditions, GitTargetConditionRetargeting)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionTrue, c.Status)
	assert.Equal(t, GitTargetReasonDestinationChanged, c.Reason)
	assert.Contains(t, c.Message, "main:apps", "the abandoned folder is named")
	assert.Contains(t, c.Message, "main:clusters/acme", "and so is the new one")
	assert.Contains(t, c.Message, "left in place",
		"the operator must be told the old folder is not deleted")

	assert.Nil(t, target.Status.LastPushTime, "the push time belonged to the abandoned folder")
	assert.Equal(t, "apps", target.Status.ObservedDestination.Path,
		"observedDestination still names the folder being abandoned until the move settles")
}

func TestSettleDestination_FirstMaterialization(t *testing.T) {
	t.Parallel()

	target := targetAt("main", "apps")
	r := &GitTargetReconciler{}
	r.settleDestination(target, logr.Discard())

	require.NotNil(t, target.Status.ObservedDestination)
	assert.Equal(t, "main", target.Status.ObservedDestination.Branch)
	assert.Equal(t, "apps", target.Status.ObservedDestination.Path)

	c := conditionByType(target.Status.Conditions, GitTargetConditionRetargeting)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Equal(t, GitTargetReasonDestinationSettled, c.Reason)
	assert.NotContains(t, c.Message, "abandoned", "a first materialization abandons nothing")
}

func TestSettleDestination_CompletedRetargetNamesTheAbandonedFolder(t *testing.T) {
	t.Parallel()

	target := materializedAtMainApps(targetAt("live", "clusters/acme"))
	r := &GitTargetReconciler{}
	r.settleDestination(target, logr.Discard())

	assert.Equal(t, "live", target.Status.ObservedDestination.Branch)
	assert.Equal(t, "clusters/acme", target.Status.ObservedDestination.Path)

	c := conditionByType(target.Status.Conditions, GitTargetConditionRetargeting)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Contains(t, c.Message, "main:apps was abandoned")
	assert.Contains(t, c.Message, "remove it by hand")
}

// The path is normalized on the way into status, so a settled target never looks moved to
// the next reconcile just because the user wrote a trailing slash.
func TestSettleDestination_NormalizesThePath(t *testing.T) {
	t.Parallel()

	target := targetAt("main", "apps/")
	r := &GitTargetReconciler{}
	r.settleDestination(target, logr.Discard())

	assert.Equal(t, "apps", target.Status.ObservedDestination.Path)
	assert.False(t, destinationMoved(target))
}

// A control-plane gate that blocks the reconcile has not put the old materialization back,
// so an in-flight retarget keeps reporting True.
func TestMarkRetargetingUnknown_KeepsAnInFlightRetarget(t *testing.T) {
	t.Parallel()

	r := &GitTargetReconciler{}

	inFlight := materializedAtMainApps(targetAt("live", "apps"))
	require.NoError(t, r.beginRetarget(t.Context(), inFlight, logr.Discard()))
	r.markRetargetingUnknown(inFlight, "Blocked by Validated=False")
	assert.Equal(t, metav1.ConditionTrue,
		conditionByType(inFlight.Status.Conditions, GitTargetConditionRetargeting).Status)

	settled := materializedAtMainApps(targetAt("main", "apps"))
	r.markRetargetingUnknown(settled, "Blocked by Validated=False")
	c := conditionByType(settled.Status.Conditions, GitTargetConditionRetargeting)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionUnknown, c.Status)
	assert.Equal(t, GitTargetReasonNotChecked, c.Reason)
}

func TestDestinationString(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "main:apps",
		destinationString(configbutleraiv1alpha3.GitTargetDestination{Branch: "main", Path: "apps"}))
}
