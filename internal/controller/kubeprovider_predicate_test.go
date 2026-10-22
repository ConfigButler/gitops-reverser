// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// providerAt builds a KubeProvider with the given generation, optionally marked for deletion.
func providerAt(generation int64, deleting bool) *configbutleraiv1alpha3.KubeProvider {
	p := &configbutleraiv1alpha3.KubeProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "prod-eu-1", Namespace: "team-a", Generation: generation},
	}
	if deleting {
		now := metav1.Now()
		p.DeletionTimestamp = &now
		p.Finalizers = []string{"example.com/elsewhere"}
	}
	return p
}

// TestKubeProviderReconcilePredicate_AdmitsSpecChangesOnly pins the predicate contract. The
// controller takes no finalizer, so the start of a deletion has nothing for it to do, and a
// status-only update (its own writes included) must not loop back into a reconcile.
func TestKubeProviderReconcilePredicate_AdmitsSpecChangesOnly(t *testing.T) {
	p := kubeProviderReconcilePredicate()

	tests := []struct {
		name string
		old  *configbutleraiv1alpha3.KubeProvider
		new  *configbutleraiv1alpha3.KubeProvider
		want bool
	}{
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
			name: "deletion beginning with no generation bump is filtered",
			old:  providerAt(3, false),
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
	assert.True(t, p.Create(event.CreateEvent{Object: providerAt(1, false)}), "a new provider is reconciled")
}
