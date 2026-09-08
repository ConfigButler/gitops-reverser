// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// statusAccessors restates, inside this package, the two methods internal/controller's status
// session reaches an object through. A kind listed below that is missing an accessor fails to
// compile HERE, next to the types, rather than at whichever controller happened to use it.
type statusAccessors interface {
	SetObservedGeneration(generation int64)
	StatusConditions() *[]metav1.Condition
}

// TestStatusAccessors_ReachTheLiveStatus covers every kind whose controller opens a status session.
// Both accessors must reach the object's OWN status: a SetObservedGeneration that wrote nowhere, or
// a StatusConditions handing back a copy, would leave the controllers publishing an empty status
// while nothing about the call sites looked wrong.
func TestStatusAccessors_ReachTheLiveStatus(t *testing.T) {
	t.Parallel()

	gitTarget := &GitTarget{}
	gitProvider := &GitProvider{}
	clusterProvider := &ClusterProvider{}
	watchRule := &WatchRule{}
	clusterWatchRule := &ClusterWatchRule{}

	tests := []struct {
		name     string
		object   statusAccessors
		observed func() int64
		stored   func() []metav1.Condition
	}{
		{
			name: "GitTarget", object: gitTarget,
			observed: func() int64 { return gitTarget.Status.ObservedGeneration },
			stored:   func() []metav1.Condition { return gitTarget.Status.Conditions },
		},
		{
			name: "GitProvider", object: gitProvider,
			observed: func() int64 { return gitProvider.Status.ObservedGeneration },
			stored:   func() []metav1.Condition { return gitProvider.Status.Conditions },
		},
		{
			name: "ClusterProvider", object: clusterProvider,
			observed: func() int64 { return clusterProvider.Status.ObservedGeneration },
			stored:   func() []metav1.Condition { return clusterProvider.Status.Conditions },
		},
		{
			name: "WatchRule", object: watchRule,
			observed: func() int64 { return watchRule.Status.ObservedGeneration },
			stored:   func() []metav1.Condition { return watchRule.Status.Conditions },
		},
		{
			name: "ClusterWatchRule", object: clusterWatchRule,
			observed: func() int64 { return clusterWatchRule.Status.ObservedGeneration },
			stored:   func() []metav1.Condition { return clusterWatchRule.Status.Conditions },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tc.object.SetObservedGeneration(9)
			assert.Equal(t, int64(9), tc.observed())

			*tc.object.StatusConditions() = append(*tc.object.StatusConditions(),
				metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Succeeded"})
			require.Len(t, tc.stored(), 1, "StatusConditions must point at the object's own slice")
		})
	}
}
