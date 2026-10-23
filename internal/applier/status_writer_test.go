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
	"k8s.io/apimachinery/pkg/api/equality"
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

var appsClaim = git.ApplierClaimRef{Kind: v1alpha3.ApplierClaimKindFlux, Namespace: targetNS, Name: "apps"}

// statusWriterFixture is a StatusWriter over a fake control cluster holding the FluxClaim "apps",
// whose reconciler has already published Configured.
func statusWriterFixture(t *testing.T, funcs interceptor.Funcs) (*StatusWriter, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, v1alpha3.AddToScheme(scheme))
	claim := &v1alpha3.FluxClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: targetNS, Name: "apps"},
		Spec: v1alpha3.FluxClaimSpec{
			KubeProviderRef:  kubeProviderRef(localCluster),
			KustomizationRef: fluxmeta.NamespacedObjectReference{Name: "apps"},
		},
		Status: v1alpha3.FluxClaimStatus{Conditions: []metav1.Condition{{
			Type: "Configured", Status: metav1.ConditionTrue, Reason: "Succeeded",
			LastTransitionTime: metav1.Now(),
		}}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(claim).
		WithStatusSubresource(&v1alpha3.FluxClaim{}).WithInterceptorFuncs(funcs).Build()
	writer := NewStatusWriter(c, c, logr.Discard())
	writer.RetryInterval = time.Millisecond
	return writer, c
}

func readClaim(t *testing.T, c client.Client) *v1alpha3.FluxClaim {
	t.Helper()
	claim := &v1alpha3.FluxClaim{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: targetNS, Name: "apps"}, claim))
	return claim
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
	writer.PublishApplierClaimStatus(appsClaim, git.ApplierClaimStatus{
		Operation: operationAt(v1alpha3.ApplyOperationRunning), RepairPending: true,
	})
	assert.False(t, writer.writePending(context.Background()))

	claim := readClaim(t, c)
	require.NotNil(t, claim.Status.Operation)
	assert.Equal(t, v1alpha3.ApplyOperationRunning, claim.Status.Operation.Phase)
	assert.Equal(t, "T1", claim.Status.Operation.Token)
	assert.True(t, claim.Status.RepairPending)
	require.Len(t, claim.Status.Conditions, 1, "the reconciler's conditions are kept")

	// No operation in the publication leaves the recorded one.
	writer.PublishApplierClaimStatus(appsClaim, git.ApplierClaimStatus{RepairPending: false})
	assert.False(t, writer.writePending(context.Background()))
	claim = readClaim(t, c)
	require.NotNil(t, claim.Status.Operation, "a worker with nothing to say about it keeps it")
	assert.False(t, claim.Status.RepairPending)
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
		writer.PublishApplierClaimStatus(appsClaim, git.ApplierClaimStatus{Operation: operationAt(phase)})
	}
	writer.writePending(context.Background())

	assert.Equal(t, 1, updates)
	assert.Equal(t, v1alpha3.ApplyOperationSucceeded, readClaim(t, c).Status.Operation.Phase)

	writer.PublishApplierClaimStatus(appsClaim, git.ApplierClaimStatus{
		Operation: operationAt(v1alpha3.ApplyOperationSucceeded),
	})
	writer.writePending(context.Background())
	assert.Equal(t, 1, updates, "a status that already holds it is not written again")
}

// TestStatusWriter_AConflictReadsAgain: a write that lost to the reconciler reads the applier claim again
// and writes over what the reconciler wrote, never over it blind.
func TestStatusWriter_AConflictReadsAgain(t *testing.T) {
	conflicts := 1
	writer, c := statusWriterFixture(t, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object,
			opts ...client.SubResourceUpdateOption) error {
			if conflicts > 0 {
				conflicts--
				return apierrors.NewConflict(schema.GroupResource{Resource: "fluxclaims"}, "apps",
					errors.New("the object has been modified"))
			}
			return cl.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
	writer.PublishApplierClaimStatus(
		appsClaim,
		git.ApplierClaimStatus{Operation: operationAt(v1alpha3.ApplyOperationFailed)},
	)
	assert.False(t, writer.writePending(context.Background()))
	assert.Equal(t, v1alpha3.ApplyOperationFailed, readClaim(t, c).Status.Operation.Phase)
}

// TestStatusWriter_AFailedWriteIsHeldUntilItLands: a write that failed is tried again, unless a newer
// publication took its place; an applier claim that is gone takes its publication with it.
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
	writer.PublishApplierClaimStatus(
		appsClaim,
		git.ApplierClaimStatus{Operation: operationAt(v1alpha3.ApplyOperationRunning)},
	)
	assert.True(t, writer.writePending(context.Background()), "held for a retry")

	failing = false
	assert.False(t, writer.writePending(context.Background()))
	assert.Equal(t, v1alpha3.ApplyOperationRunning, readClaim(t, c).Status.Operation.Phase)

	gone := git.ApplierClaimRef{Kind: v1alpha3.ApplierClaimKindFlux, Namespace: targetNS, Name: "gone"}
	writer.PublishApplierClaimStatus(gone, git.ApplierClaimStatus{RepairPending: true})
	assert.False(t, writer.writePending(context.Background()), "nothing to hold for an applier claim that is gone")
}

// TestStatusWriter_StartWritesWhatIsPublished: the runnable writes a publication without being asked
// twice, and stops with its context.
func TestStatusWriter_StartWritesWhatIsPublished(t *testing.T) {
	writer, c := statusWriterFixture(t, interceptor.Funcs{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- writer.Start(ctx) }()

	writer.PublishApplierClaimStatus(
		appsClaim,
		git.ApplierClaimStatus{Operation: operationAt(v1alpha3.ApplyOperationRequested)},
	)
	require.Eventually(t, func() bool {
		operation := readClaim(t, c).Status.Operation
		return operation != nil && operation.Phase == v1alpha3.ApplyOperationRequested
	}, 5*time.Second, 5*time.Millisecond)

	cancel()
	require.NoError(t, <-done)
	assert.True(t, writer.NeedLeaderElection())
}

// setOperation gives the applier claim's status an operation, as another writer would.
func setOperation(t *testing.T, c client.Client, operation *v1alpha3.ApplyOperation) {
	t.Helper()
	claim := readClaim(t, c)
	claim.Status.Operation = operation
	require.NoError(t, c.Status().Update(context.Background(), claim))
}

// operationRequested is a worker's operation with its own token, requested after operationAt's.
func operationRequested(
	phase v1alpha3.ApplyOperationPhase, token string, after time.Duration,
) *v1alpha3.ApplyOperation {
	operation := operationAt(phase)
	operation.Token, operation.RequestedAt = token, metav1.NewTime(stampTime.Add(after))
	return operation
}

// assertOperation asserts the status holds want, as the API server stores it.
func assertOperation(t *testing.T, want, got *v1alpha3.ApplyOperation) {
	t.Helper()
	if want == nil {
		assert.Nil(t, got)
		return
	}
	assert.True(t, equality.Semantic.DeepEqual(stored(want), got), "want %+v, got %+v", want, got)
}

// countingUpdates counts the status updates a fixture makes.
func countingUpdates(updates *int) interceptor.Funcs {
	return interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object,
			opts ...client.SubResourceUpdateOption) error {
			*updates++
			return cl.SubResource(sub).Update(ctx, obj, opts...)
		},
	}
}

// TestStatusWriter_ARecordIsWrittenThroughBeforeItReturns: the dispatcher records the operation
// just before it starts the apply. It is on the applier claim when RecordApplyOperation returns, over the
// completed operation before it, and the reconciler's conditions and the recorded repairPending stay.
func TestStatusWriter_ARecordIsWrittenThroughBeforeItReturns(t *testing.T) {
	writer, c := statusWriterFixture(t, interceptor.Funcs{})
	setOperation(t, c, operationAt(v1alpha3.ApplyOperationSucceeded))
	writer.PublishApplierClaimStatus(appsClaim,
		git.ApplierClaimStatus{Operation: operationAt(v1alpha3.ApplyOperationSucceeded), RepairPending: true})

	requested := operationRequested(v1alpha3.ApplyOperationRequested, "T2", time.Minute)
	require.NoError(t, writer.RecordApplyOperation(context.Background(), appsClaim, requested))
	recorded := readClaim(t, c)
	require.NotNil(t, recorded.Status.Operation)
	assert.Equal(t, "T2", recorded.Status.Operation.Token)
	assert.Equal(t, v1alpha3.ApplyOperationRequested, recorded.Status.Operation.Phase)
	assert.True(t, recorded.Status.RepairPending, "what was held is written first")
	require.Len(t, recorded.Status.Conditions, 1, "the reconciler's conditions are kept")

	assert.False(t, writer.writePending(context.Background()))
	assert.Equal(t, recorded.ResourceVersion, readClaim(t, c).ResourceVersion, "nothing left to write")
	require.NoError(t, writer.RecordApplyOperation(context.Background(), appsClaim, requested),
		"an operation recorded already is recorded")
}

// TestStatusWriter_ARecordWritesTheLatestViewFirst: the operation before the one recorded completed
// on the worker, and its completion is still held. The record writes it first, so it replaces a
// completed operation, not one in flight that it would refuse.
func TestStatusWriter_ARecordWritesTheLatestViewFirst(t *testing.T) {
	writer, c := statusWriterFixture(t, interceptor.Funcs{})
	setOperation(t, c, operationAt(v1alpha3.ApplyOperationRunning))
	writer.PublishApplierClaimStatus(appsClaim,
		git.ApplierClaimStatus{Operation: operationAt(v1alpha3.ApplyOperationSucceeded)})

	requested := operationRequested(v1alpha3.ApplyOperationRequested, "T2", time.Minute)
	require.NoError(t, writer.RecordApplyOperation(context.Background(), appsClaim, requested))
	assert.Equal(t, "T2", readClaim(t, c).Status.Operation.Token)
}

// TestStatusWriter_ARecordNeverReplacesAnotherOperationInFlight is the fourth review's first
// finding. The applier claim's reconciler recorded an external resume (no token, requested after the
// worker sent its attempt), and the record used to keep it, write nothing, and return nil, so the
// resume went ahead with no record of its own. The record fails, and leaves the status as it is.
func TestStatusWriter_ARecordNeverReplacesAnotherOperationInFlight(t *testing.T) {
	writer, c := statusWriterFixture(t, interceptor.Funcs{})
	external := &v1alpha3.ApplyOperation{
		Phase: v1alpha3.ApplyOperationUnresolved, SuspendToken: "F1",
		RequestedAt: metav1.NewTime(stampTime.Add(time.Hour)),
	}
	setOperation(t, c, external)

	requested := operationRequested(v1alpha3.ApplyOperationRequested, "T2", time.Minute)
	err := writer.RecordApplyOperation(context.Background(), appsClaim, requested)
	require.ErrorIs(t, err, errOperationInFlight)
	assertOperation(t, external, readClaim(t, c).Status.Operation)

	// An operation an earlier process left in flight is refused the same way: the worker follows it
	// before it asks for anything.
	setOperation(t, c, operationRequested(v1alpha3.ApplyOperationRunning, "T0", -time.Hour))
	require.ErrorIs(t, writer.RecordApplyOperation(context.Background(), appsClaim, requested),
		errOperationInFlight)
}

// TestStatusWriter_ARecordForAReleasedClaimFails: an applier claim that is being deleted is an error, so
// nothing is started for it, also when the background writer already dropped what was held for it.
func TestStatusWriter_ARecordForAReleasedClaimFails(t *testing.T) {
	writer, c := statusWriterFixture(t, interceptor.Funcs{})
	claim := readClaim(t, c)
	claim.Finalizers = []string{v1alpha3.FluxClaimFinalizer}
	require.NoError(t, c.Update(context.Background(), claim))
	require.NoError(t, c.Delete(context.Background(), claim))
	requested := operationRequested(v1alpha3.ApplyOperationRequested, "T2", time.Minute)

	writer.PublishApplierClaimStatus(appsClaim, git.ApplierClaimStatus{RepairPending: true})
	require.ErrorIs(t, writer.RecordApplyOperation(context.Background(), appsClaim, requested),
		errClaimReleased, "with a publication held")

	writer.PublishApplierClaimStatus(appsClaim, git.ApplierClaimStatus{RepairPending: true})
	assert.False(t, writer.writePending(context.Background()), "the background writer drops it")
	require.ErrorIs(t, writer.RecordApplyOperation(context.Background(), appsClaim, requested),
		errClaimReleased, "with nothing held")
	assert.Nil(t, readClaim(t, c).Status.Operation)
}

// TestStatusWriter_AWithdrawnRecordIsPutBack: the operation was recorded, and the apply then never
// started (the resume failed). The worker withdraws it, and the status shows what it held before
// the record again: the completed operation before it, or none.
func TestStatusWriter_AWithdrawnRecordIsPutBack(t *testing.T) {
	for name, before := range map[string]*v1alpha3.ApplyOperation{
		"a completed operation": operationAt(v1alpha3.ApplyOperationSucceeded),
		"none":                  nil,
	} {
		t.Run(name, func(t *testing.T) {
			writer, c := statusWriterFixture(t, interceptor.Funcs{})
			if before != nil {
				setOperation(t, c, before)
			}
			requested := operationRequested(v1alpha3.ApplyOperationRequested, "T2", time.Minute)
			require.NoError(t, writer.RecordApplyOperation(context.Background(), appsClaim, requested))
			require.Equal(t, "T2", readClaim(t, c).Status.Operation.Token)

			// The worker knows nothing older than the record: it withdraws it with no operation.
			writer.PublishApplierClaimStatus(appsClaim, git.ApplierClaimStatus{Withdrawn: requested})
			// A later publication that withdraws nothing does not take the withdrawal back.
			writer.PublishApplierClaimStatus(appsClaim, git.ApplierClaimStatus{RepairPending: true})
			assert.False(t, writer.writePending(context.Background()))

			written := readClaim(t, c)
			assertOperation(t, before, written.Status.Operation)
			assert.True(t, written.Status.RepairPending)
		})
	}
}

// TestStatusWriter_AWithdrawalOfAnOperationNotHeldChangesNothing: an attempt that ended before its
// record withdraws an operation the status never held, and the status is not written.
func TestStatusWriter_AWithdrawalOfAnOperationNotHeldChangesNothing(t *testing.T) {
	updates := 0
	writer, c := statusWriterFixture(t, countingUpdates(&updates))
	setOperation(t, c, operationAt(v1alpha3.ApplyOperationSucceeded))
	updates = 0

	writer.PublishApplierClaimStatus(appsClaim, git.ApplierClaimStatus{
		Operation: operationAt(v1alpha3.ApplyOperationSucceeded),
		Withdrawn: operationRequested(v1alpha3.ApplyOperationRequested, "T2", time.Minute),
	})
	assert.False(t, writer.writePending(context.Background()))
	assert.Zero(t, updates)
	assert.Equal(t, "T1", readClaim(t, c).Status.Operation.Token)
}

// TestStatusWriter_NeverWritesOverAnotherOperation: the reconciler records an external resume as an
// operation of its own. A publication of the worker's operation does not replace it, whether it was
// requested before it or after it; the rest of the publication still lands.
func TestStatusWriter_NeverWritesOverAnotherOperation(t *testing.T) {
	for name, requested := range map[string]time.Duration{"before": -time.Minute, "after": time.Minute} {
		t.Run(name, func(t *testing.T) {
			writer, c := statusWriterFixture(t, interceptor.Funcs{})
			external := &v1alpha3.ApplyOperation{
				Phase: v1alpha3.ApplyOperationUnresolved, SuspendToken: "F1", RequestedAt: metav1.NewTime(stampTime),
			}
			setOperation(t, c, external)

			writer.PublishApplierClaimStatus(appsClaim, git.ApplierClaimStatus{
				Operation:     operationRequested(v1alpha3.ApplyOperationSucceeded, "T2", requested),
				RepairPending: true,
			})
			assert.False(t, writer.writePending(context.Background()))
			written := readClaim(t, c)
			assertOperation(t, external, written.Status.Operation)
			assert.True(t, written.Status.RepairPending, "the rest of the publication still lands")
		})
	}
}

// TestStatusWriter_NeverReopensACompletedOperation is review 6: a worker that recovers the
// reconciler's record of an external resume publishes it as it was listed, Unresolved; the
// reconciler can have completed it since. The same operation is never written back in flight.
func TestStatusWriter_NeverReopensACompletedOperation(t *testing.T) {
	writer, c := statusWriterFixture(t, interceptor.Funcs{})
	listed := &v1alpha3.ApplyOperation{
		Phase: v1alpha3.ApplyOperationUnresolved, SuspendToken: "F1", RequestedAt: metav1.NewTime(stampTime),
	}
	completed := listed.DeepCopy()
	completed.Phase = v1alpha3.ApplyOperationSucceeded
	setOperation(t, c, completed)

	writer.PublishApplierClaimStatus(appsClaim, git.ApplierClaimStatus{Operation: listed})
	assert.False(t, writer.writePending(context.Background()))
	assertOperation(t, completed, readClaim(t, c).Status.Operation)

	failed := listed.DeepCopy()
	failed.Phase = v1alpha3.ApplyOperationFailed
	writer.PublishApplierClaimStatus(appsClaim, git.ApplierClaimStatus{Operation: failed})
	assert.False(t, writer.writePending(context.Background()))
	assertOperation(t, failed, readClaim(t, c).Status.Operation) // a completion still replaces a completion
}
