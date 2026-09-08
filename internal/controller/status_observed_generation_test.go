// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// TestBeginStatus_StampsObservedGenerationForEveryKind checks the stamp on every kind that opens a
// status session, because the failure it prevents is silent: an object whose observedGeneration
// trails its generation is reported by kstatus as describing a spec nobody looked at, and nothing
// in the status itself looks wrong. The table is the enrolment list — a sixth kind that starts
// using the helper without appearing here is the gap this test exists to close.
func TestBeginStatus_StampsObservedGenerationForEveryKind(t *testing.T) {
	tests := []struct {
		name   string
		object statusObject
	}{
		{
			name:   "GitTarget",
			object: &configbutleraiv1alpha3.GitTarget{ObjectMeta: metav1.ObjectMeta{Generation: 7}},
		},
		{
			name:   "GitProvider",
			object: &configbutleraiv1alpha3.GitProvider{ObjectMeta: metav1.ObjectMeta{Generation: 7}},
		},
		{
			name:   "ClusterProvider",
			object: &configbutleraiv1alpha3.ClusterProvider{ObjectMeta: metav1.ObjectMeta{Generation: 7}},
		},
		{
			name:   "WatchRule",
			object: &configbutleraiv1alpha3.WatchRule{ObjectMeta: metav1.ObjectMeta{Generation: 7}},
		},
		{
			name:   "ClusterWatchRule",
			object: &configbutleraiv1alpha3.ClusterWatchRule{ObjectMeta: metav1.ObjectMeta{Generation: 7}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := beginStatus(nil, nil, tc.object)

			assert.Equal(t, int64(7), observedGenerationOf(t, tc.object),
				"beginStatus must stamp the generation it observed")

			// The same accessor supplies the conditions, so a session that reached the wrong slice
			// would write somewhere the object never publishes from.
			st.set(ConditionTypeReady, metav1.ConditionTrue, ReasonSucceeded, "converged")
			require.Len(t, *tc.object.StatusConditions(), 1)
			assert.Equal(t, int64(7), (*tc.object.StatusConditions())[0].ObservedGeneration)
		})
	}
}

// observedGenerationOf reads the field back through the concrete type, which is the only side the
// accessor interface does not expose — a getter would exist solely for this assertion.
func observedGenerationOf(t *testing.T, object client.Object) int64 {
	t.Helper()
	switch o := object.(type) {
	case *configbutleraiv1alpha3.GitTarget:
		return o.Status.ObservedGeneration
	case *configbutleraiv1alpha3.GitProvider:
		return o.Status.ObservedGeneration
	case *configbutleraiv1alpha3.ClusterProvider:
		return o.Status.ObservedGeneration
	case *configbutleraiv1alpha3.WatchRule:
		return o.Status.ObservedGeneration
	case *configbutleraiv1alpha3.ClusterWatchRule:
		return o.Status.ObservedGeneration
	default:
		t.Fatalf("unexpected kind %T", object)
		return 0
	}
}

// TestBeginStatus_TheStampAloneIsPersisted pins the ordering inside beginStatus against commit()'s
// no-op suppression. The snapshot the patch is computed from must be taken BEFORE the stamp: taken
// after, a bumped generation would be identical on both sides, the patch would be empty, and a
// reconcile that changed no condition would silently leave observedGeneration behind — which is
// exactly the reconcile where nothing else would ever reveal it.
func TestBeginStatus_TheStampAloneIsPersisted(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, configbutleraiv1alpha3.AddToScheme(scheme))

	target := &configbutleraiv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{
			Name: "acme", Namespace: "tenant-acme", ResourceVersion: "1", Generation: 4,
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(target).WithStatusSubresource(target).Build()

	st := beginStatus(c, nil, target)
	require.NoError(t, st.commit(context.Background()))

	var stored configbutleraiv1alpha3.GitTarget
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(target), &stored))
	assert.Equal(t, int64(4), stored.Status.ObservedGeneration,
		"a reconcile whose only status change is the stamp must still write it")
}
