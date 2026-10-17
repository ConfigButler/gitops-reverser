// SPDX-License-Identifier: Apache-2.0

package applier

import (
	"context"
	"errors"
	"testing"
	"time"

	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
)

var appsTrigger = git.TriggerRef{Kind: v1alpha3.ApplyTriggerKindFlux, Namespace: targetNS, Name: "apps"}

// statusWriterFixture is a StatusWriter over a fake control cluster holding the FluxTrigger "apps",
// whose reconciler has already published Configured.
func statusWriterFixture(t *testing.T, funcs interceptor.Funcs) (*StatusWriter, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, v1alpha3.AddToScheme(scheme))
	trigger := &v1alpha3.FluxTrigger{
		ObjectMeta: metav1.ObjectMeta{Namespace: targetNS, Name: "apps"},
		Spec: v1alpha3.FluxTriggerSpec{
			ClusterProviderRef: fluxmeta.LocalObjectReference{Name: v1alpha3.DefaultClusterProviderName},
			KustomizationRef:   fluxmeta.NamespacedObjectReference{Name: "apps"},
		},
		Status: v1alpha3.FluxTriggerStatus{Conditions: []metav1.Condition{{
			Type: "Configured", Status: metav1.ConditionTrue, Reason: "Succeeded",
			LastTransitionTime: metav1.Now(),
		}}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(trigger).
		WithStatusSubresource(&v1alpha3.FluxTrigger{}).WithInterceptorFuncs(funcs).Build()
	writer := NewStatusWriter(c, c, logr.Discard())
	writer.RetryInterval = time.Millisecond
	return writer, c
}

func readTrigger(t *testing.T, c client.Client) *v1alpha3.FluxTrigger {
	t.Helper()
	trigger := &v1alpha3.FluxTrigger{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: targetNS, Name: "apps"}, trigger))
	return trigger
}

func operationAt(phase v1alpha3.ApplyOperationPhase) *v1alpha3.ApplyOperation {
	return &v1alpha3.ApplyOperation{
		Phase: phase, Reason: v1alpha3.ApplyReasonRefusal, Commit: testCommit, Token: "T1",
		RequestedAt: metav1.NewTime(stampTime),
	}
}

// TestStatusWriter_WritesTheOperationAndKeepsTheRest: the worker's two fields land, and the
// reconciler's conditions stay as they were.
func TestStatusWriter_WritesTheOperationAndKeepsTheRest(t *testing.T) {
	writer, c := statusWriterFixture(t, interceptor.Funcs{})
	writer.PublishApplyTriggerStatus(appsTrigger, git.ApplyTriggerStatus{
		Operation: operationAt(v1alpha3.ApplyOperationRunning), RepairOwed: true,
	})
	assert.False(t, writer.writePending(context.Background()))

	trigger := readTrigger(t, c)
	require.NotNil(t, trigger.Status.Operation)
	assert.Equal(t, v1alpha3.ApplyOperationRunning, trigger.Status.Operation.Phase)
	assert.Equal(t, "T1", trigger.Status.Operation.Token)
	assert.True(t, trigger.Status.RepairOwed)
	require.Len(t, trigger.Status.Conditions, 1, "the reconciler's conditions are kept")

	// No operation in the publication leaves the recorded one.
	writer.PublishApplyTriggerStatus(appsTrigger, git.ApplyTriggerStatus{RepairOwed: false})
	assert.False(t, writer.writePending(context.Background()))
	trigger = readTrigger(t, c)
	require.NotNil(t, trigger.Status.Operation, "a worker with nothing to say about it keeps it")
	assert.False(t, trigger.Status.RepairOwed)
}

// TestStatusWriter_TheLatestPublicationWins: publications not yet written are replaced, not
// replayed.
func TestStatusWriter_TheLatestPublicationWins(t *testing.T) {
	updates := 0
	writer, c := statusWriterFixture(t, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object,
			opts ...client.SubResourceUpdateOption) error {
			updates++
			return cl.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
	for _, phase := range []v1alpha3.ApplyOperationPhase{
		v1alpha3.ApplyOperationRequested, v1alpha3.ApplyOperationRunning, v1alpha3.ApplyOperationSucceeded,
	} {
		writer.PublishApplyTriggerStatus(appsTrigger, git.ApplyTriggerStatus{Operation: operationAt(phase)})
	}
	writer.writePending(context.Background())

	assert.Equal(t, 1, updates)
	assert.Equal(t, v1alpha3.ApplyOperationSucceeded, readTrigger(t, c).Status.Operation.Phase)

	writer.PublishApplyTriggerStatus(appsTrigger, git.ApplyTriggerStatus{
		Operation: operationAt(v1alpha3.ApplyOperationSucceeded),
	})
	writer.writePending(context.Background())
	assert.Equal(t, 1, updates, "a status that already holds it is not written again")
}

// TestStatusWriter_AConflictReadsAgain: a write that lost to the reconciler reads the trigger again
// and writes over what the reconciler wrote, never over it blind.
func TestStatusWriter_AConflictReadsAgain(t *testing.T) {
	conflicts := 1
	writer, c := statusWriterFixture(t, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object,
			opts ...client.SubResourceUpdateOption) error {
			if conflicts > 0 {
				conflicts--
				return apierrors.NewConflict(schema.GroupResource{Resource: "fluxtriggers"}, "apps",
					errors.New("the object has been modified"))
			}
			return cl.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
	writer.PublishApplyTriggerStatus(
		appsTrigger,
		git.ApplyTriggerStatus{Operation: operationAt(v1alpha3.ApplyOperationFailed)},
	)
	assert.False(t, writer.writePending(context.Background()))
	assert.Equal(t, v1alpha3.ApplyOperationFailed, readTrigger(t, c).Status.Operation.Phase)
}

// TestStatusWriter_AFailedWriteIsHeldUntilItLands: a write that failed is tried again, unless a newer
// publication took its place; a trigger that is gone takes its publication with it.
func TestStatusWriter_AFailedWriteIsHeldUntilItLands(t *testing.T) {
	failing := true
	writer, c := statusWriterFixture(t, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object,
			opts ...client.SubResourceUpdateOption) error {
			if failing {
				return errors.New("the API server is away")
			}
			return cl.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
	writer.PublishApplyTriggerStatus(
		appsTrigger,
		git.ApplyTriggerStatus{Operation: operationAt(v1alpha3.ApplyOperationRunning)},
	)
	assert.True(t, writer.writePending(context.Background()), "held for a retry")

	failing = false
	assert.False(t, writer.writePending(context.Background()))
	assert.Equal(t, v1alpha3.ApplyOperationRunning, readTrigger(t, c).Status.Operation.Phase)

	gone := git.TriggerRef{Kind: v1alpha3.ApplyTriggerKindFlux, Namespace: targetNS, Name: "gone"}
	writer.PublishApplyTriggerStatus(gone, git.ApplyTriggerStatus{RepairOwed: true})
	assert.False(t, writer.writePending(context.Background()), "nothing to hold for a trigger that is gone")
}

// TestStatusWriter_StartWritesWhatIsPublished: the runnable writes a publication without being asked
// twice, and stops with its context.
func TestStatusWriter_StartWritesWhatIsPublished(t *testing.T) {
	writer, c := statusWriterFixture(t, interceptor.Funcs{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- writer.Start(ctx) }()

	writer.PublishApplyTriggerStatus(
		appsTrigger,
		git.ApplyTriggerStatus{Operation: operationAt(v1alpha3.ApplyOperationRequested)},
	)
	require.Eventually(t, func() bool {
		operation := readTrigger(t, c).Status.Operation
		return operation != nil && operation.Phase == v1alpha3.ApplyOperationRequested
	}, 5*time.Second, 5*time.Millisecond)

	cancel()
	require.NoError(t, <-done)
	assert.True(t, writer.NeedLeaderElection())
}
