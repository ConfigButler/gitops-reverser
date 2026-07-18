// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// provider builds a ClusterProvider with the given generation, optionally marked for deletion.
func providerAt(generation int64, deleting bool) *configbutleraiv1alpha3.ClusterProvider {
	p := &configbutleraiv1alpha3.ClusterProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "prod-eu-1", Generation: generation},
	}
	if deleting {
		now := metav1.Now()
		p.DeletionTimestamp = &now
		p.Finalizers = []string{LegacyClusterProviderFinalizer}
	}
	return p
}

// TestClusterProviderReconcilePredicate_AdmitsDeletionAndSpecChanges pins the predicate contract.
// The deletion case lets the controller promptly shed the retired fact-purge finalizer from an
// object created by an older operator.
func TestClusterProviderReconcilePredicate_AdmitsDeletionAndSpecChanges(t *testing.T) {
	p := clusterProviderReconcilePredicate()

	tests := []struct {
		name string
		old  *configbutleraiv1alpha3.ClusterProvider
		new  *configbutleraiv1alpha3.ClusterProvider
		want bool
	}{
		{
			name: "deletion begins is admitted even with no generation bump",
			old:  providerAt(3, false),
			new:  providerAt(3, true),
			want: true,
		},
		{
			name: "spec change is admitted",
			old:  providerAt(3, false),
			new:  providerAt(4, false),
			want: true,
		},
		{
			name: "status-only update is filtered",
			old:  providerAt(3, false),
			new:  providerAt(3, false),
			want: false,
		},
		{
			name: "repeated update while already deleting is filtered",
			old:  providerAt(3, true),
			new:  providerAt(3, true),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := p.Update(event.UpdateEvent{ObjectOld: tt.old, ObjectNew: tt.new})
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestClusterProviderReconcilePredicate_NilObjectsAreAdmitted keeps the predicate fail-open on a
// malformed event: dropping it would silently skip a reconcile, which is worse than one extra pass.
func TestClusterProviderReconcilePredicate_NilObjectsAreAdmitted(t *testing.T) {
	p := clusterProviderReconcilePredicate()
	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: nil, ObjectNew: providerAt(1, false)}))
	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: providerAt(1, false), ObjectNew: nil}))
}
