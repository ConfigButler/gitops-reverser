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

package watch

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/typeset"
)

// gitTargetSnapshotSynced gates the per-type path on the GitTarget's bootstrap: only a target
// whose SnapshotSynced condition is True is acted on, so a transition during bootstrap never
// races the whole-GitTarget resync. A missing or not-yet-synced target reports false.
func TestGitTargetSnapshotSynced(t *testing.T) {
	scheme := makeScheme(t)
	synced := &configv1alpha1.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "synced", Namespace: "gitops-reverser"},
		Status: configv1alpha1.GitTargetStatus{Conditions: []metav1.Condition{{
			Type: gitTargetSnapshotSyncedCondition, Status: metav1.ConditionTrue,
			Reason: "OK", LastTransitionTime: metav1.Now(),
		}}},
	}
	pending := &configv1alpha1.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "pending", Namespace: "gitops-reverser"},
	}
	client := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(synced, pending).Build()
	m := &Manager{Client: client, Log: logr.Discard()}
	ctx := context.Background()

	assert.True(t, m.gitTargetSnapshotSynced(ctx, itypes.NewResourceReference("synced", "gitops-reverser")))
	assert.False(t, m.gitTargetSnapshotSynced(ctx, itypes.NewResourceReference("pending", "gitops-reverser")),
		"a target still mid-bootstrap is not yet eligible for per-type reconcile")
	assert.False(t, m.gitTargetSnapshotSynced(ctx, itypes.NewResourceReference("absent", "gitops-reverser")),
		"an unreadable target is treated as not synced, so the per-type path waits")
}

// The transitions that carry no git action (Wobbling/Recovered/Refused) are inert in the
// handler: they neither reconcile nor sweep, so a Manager with no EventRouter handles them
// without touching the (nil) router.
func TestHandleTypeLifecycleEvent_NoActionTransitionsAreInert(t *testing.T) {
	m := &Manager{Log: logr.Discard()}
	ctx := context.Background()
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

	for _, kind := range []typeset.EventKind{typeset.TypeWobbling, typeset.TypeRecovered, typeset.TypeRefused} {
		// Must not panic or dereference the nil EventRouter for a no-git-action transition.
		assert.NotPanics(t, func() {
			m.handleTypeLifecycleEvent(ctx, logr.Discard(), typeset.LifecycleEvent{Kind: kind, GVR: gvr})
		})
	}
}
