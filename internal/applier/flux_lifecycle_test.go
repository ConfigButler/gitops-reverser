// SPDX-License-Identifier: Apache-2.0

package applier

// The suspended Flux lifecycle, against fluxSim: a resume with a token, the outcome read once the
// reconcile that read it ended, the suspend fence, and the hand-back. The first tests are the third
// review's probes and the measurements of docs/design/apply-trigger.md, each of which
// failed against the request-based client this replaced.

import (
	"context"
	"errors"
	"testing"
	"time"

	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/applier/fluxsim"
	"github.com/ConfigButler/gitops-reverser/internal/git"
)

var (
	checkoutKey = types.NamespacedName{Namespace: fluxSystemNS, Name: "checkout"}
	platformKey = types.NamespacedName{Namespace: fluxSystemNS, Name: "platform"}
)

// simCluster is a source cluster with the Kustomization flux-system/checkout applying the
// GitRepository flux-system/platform on testBranch, whose artifact and last apply are at revision,
// and whose fetch finds testCommit.
func simCluster(t *testing.T, revision string, suspend bool) *fluxsim.Sim {
	t.Helper()
	ks := kustomizationObj(fluxSystemNS, "checkout", gitRepoSourceRef("platform"))
	ks.SetGeneration(1)
	if suspend {
		ks = fluxsim.OwnSuspend(t, ks, true, SuspendFieldManager, metav1.ManagedFieldsOperationApply)
	}
	require.NoError(t, unstructured.SetNestedField(ks.Object, revision, "status", "lastAppliedRevision"))
	fluxsim.SetReady(ks, metav1.ConditionTrue, fluxmeta.ReconciliationSucceededReason, "Applied revision: "+revision)
	sim := fluxsim.New(t, newSourceClient(ks,
		gitRepositoryObj(fluxSystemNS, "platform", testRepoURL, branchRef(testBranch), revision)))
	sim.Fetches(platformKey, "main@sha1:"+testCommit)
	return sim
}

// simFlux is a Flux client of the simulated cluster, with a clock that never repeats a token.
func simFlux(sim *fluxsim.Sim) *Flux {
	flux := NewFlux(sim.Client)
	flux.Now = fluxsim.TickingClock(stampTime)
	flux.PollInterval = time.Millisecond
	return flux
}

// ask is a request for testCommit, for reason, with token T1.
func ask(reason v1alpha3.ApplyReason) Ask {
	return Ask{Want: testBinding, Commit: testCommit, Reason: reason, Token: "T1"}
}

// follow reads the operation's progress until it ends, and returns every step it read, the last
// being the end.
func follow(t *testing.T, flux *Flux, s *Stamped) []Progress {
	t.Helper()
	var steps []Progress
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		progress, err := flux.Progress(context.Background(), checkoutKey, s)
		require.NoError(t, err)
		if len(steps) == 0 || steps[len(steps)-1] != progress {
			steps = append(steps, progress)
		}
		if progress.Outcome.Ends() {
			return steps
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("the operation did not end; it read %v", steps)
	return nil
}

func outcomes(steps []Progress) []git.ApplyOutcome {
	out := make([]git.ApplyOutcome, 0, len(steps))
	for _, step := range steps {
		out = append(out, step.Outcome)
	}
	return out
}

// TestFluxProbe_AnApplyIsNotFinishedWhenItWasAttempted is the third review's first finding:
// kustomize-controller writes lastAttemptedRevision before it builds, so reading it as "finished"
// closed the apply window while the apply still ran. Now the operation runs until the reconcile
// that read the resume's token ended, and completes once the suspend after it is handled.
func TestFluxProbe_AnApplyIsNotFinishedWhenItWasAttempted(t *testing.T) {
	sim := simCluster(t, "main@sha1:"+olderCommit, true)
	held := sim.HoldNextApply()
	flux := simFlux(sim)

	stamped, err := flux.Request(context.Background(), checkoutKey, ask(v1alpha3.ApplyReasonForeignCommit))
	require.NoError(t, err)
	require.Equal(t, OutcomeResumed, stamped.Outcome)
	<-held
	assert.Equal(t, "main@sha1:"+testCommit, statusString(sim.Kustomization(checkoutKey), "lastAttemptedRevision"))
	progress, err := flux.Progress(context.Background(), checkoutKey, &stamped)
	require.NoError(t, err)
	assert.Equal(t, Progress{Outcome: git.ApplyRunning}, progress, "the apply is still running")

	sim.Release()
	steps := follow(t, flux, &stamped)
	require.GreaterOrEqual(t, len(steps), 2)
	tail := steps[len(steps)-2:]
	assert.Equal(t, []git.ApplyOutcome{git.ApplySettling, git.ApplySucceeded}, outcomes(tail))
	assert.Equal(t, "main@sha1:"+testCommit, tail[1].AppliedRevision)
	assert.Equal(t, stamped.FenceToken, tail[1].FenceToken)
	assert.Equal(t, []string{"flux-system/checkout main@sha1:" + testCommit}, sim.Applied())
	assert.True(t, suspended(sim.Kustomization(checkoutKey)), "suspended again")
}

// TestFluxProbe_ATokenHandledWhileSuspendedIsNoApply is measurement point 3: a suspended
// Kustomization marks a requestedAt handled without applying anything, so a Refusal that only
// stamped it completed with nothing reverted. Now a Refusal resumes it (point 4: a resume alone
// applies), and is reverted by that apply.
func TestFluxProbe_ATokenHandledWhileSuspendedIsNoApply(t *testing.T) {
	sim := simCluster(t, "main@sha1:"+testCommit, true)
	flux := simFlux(sim)

	stamped, err := flux.Request(context.Background(), checkoutKey, ask(v1alpha3.ApplyReasonRefusal))
	require.NoError(t, err)
	steps := follow(t, flux, &stamped)
	assert.Equal(t, git.ApplySucceeded, steps[len(steps)-1].Outcome)
	assert.Equal(t, []string{"flux-system/checkout main@sha1:" + testCommit}, sim.Applied(),
		"the refusal is reverted by an apply of the unchanged commit")
}

// TestFluxProbe_AnArtifactNotAppliedIsNotCurrent is the first finding's second half: an artifact
// that holds the commit says nothing about whether the Kustomization applied it.
func TestFluxProbe_AnArtifactNotAppliedIsNotCurrent(t *testing.T) {
	sim := simCluster(t, "main@sha1:"+testCommit, true)
	sim.WriteStatus(context.Background(), kustomizationGVR(), checkoutKey, func(obj *unstructured.Unstructured) {
		_ = unstructured.SetNestedField(obj.Object, "main@sha1:"+olderCommit, "status", "lastAppliedRevision")
	})
	stamped, err := simFlux(sim).Request(context.Background(), checkoutKey, ask(v1alpha3.ApplyReasonCatchUp))
	require.NoError(t, err)
	assert.Equal(t, OutcomeResumed, stamped.Outcome, "the Kustomization never applied the commit")
}

// TestFluxRequest_Outcomes pins what a request does, by where the artifact and the last apply are,
// and by reason. Every request that asks for an apply resumes the Kustomization with its token, and
// fetches first when the artifact does not hold the commit.
func TestFluxRequest_Outcomes(t *testing.T) {
	tests := []struct {
		name      string
		artifact  string
		applied   string
		suspended bool
		commit    string
		reason    v1alpha3.ApplyReason
		want      Outcome
		wantFetch bool
	}{
		{
			name: "a foreign commit not fetched yet fetches, then resumes", artifact: "main@sha1:" + olderCommit,
			suspended: true, commit: testCommit, reason: v1alpha3.ApplyReasonForeignCommit,
			want: OutcomeResumed, wantFetch: true,
		},
		{
			name: "a catch-up behind fetches, then resumes", artifact: "main@sha1:" + olderCommit,
			suspended: true, commit: testCommit, reason: v1alpha3.ApplyReasonCatchUp,
			want: OutcomeResumed, wantFetch: true,
		},
		{
			name: "no artifact yet fetches, then resumes", suspended: true,
			commit: testCommit, reason: v1alpha3.ApplyReasonForeignCommit,
			want: OutcomeResumed, wantFetch: true,
		},
		{
			name: "a branch absent on the remote fetches, and resumes nothing", artifact: "main@sha1:" + testCommit,
			suspended: true, commit: "", reason: v1alpha3.ApplyReasonForeignCommit,
			want: OutcomeCurrent, wantFetch: true,
		},
		{
			name: "a refusal at the applied commit resumes", artifact: "main@sha1:" + testCommit,
			applied: "main@sha1:" + testCommit, suspended: true,
			commit: testCommit, reason: v1alpha3.ApplyReasonRefusal, want: OutcomeResumed,
		},
		{
			name: "a repair at the applied commit resumes", artifact: "main@sha1:" + testCommit,
			applied: "main@sha1:" + testCommit, suspended: true,
			commit: testCommit, reason: v1alpha3.ApplyReasonRepair, want: OutcomeResumed,
		},
		{
			name: "a foreign commit applied while suspended asks nothing", artifact: "main/" + testCommit,
			applied: "main@sha1:" + testCommit, suspended: true,
			commit: testCommit, reason: v1alpha3.ApplyReasonForeignCommit, want: OutcomeCurrent,
		},
		{
			name: "a catch-up applied while suspended asks nothing", artifact: "sha1:" + testCommit,
			applied: "main@sha1:" + testCommit, suspended: true,
			commit: testCommit, reason: v1alpha3.ApplyReasonCatchUp, want: OutcomeCurrent,
		},
		{
			// Not suspended yet (the trigger has not adopted it): it may apply on its own, so the
			// operation runs, and its fence suspends it.
			name: "a catch-up applied but not suspended resumes", artifact: "main@sha1:" + testCommit,
			applied: "main@sha1:" + testCommit,
			commit:  testCommit, reason: v1alpha3.ApplyReasonCatchUp, want: OutcomeResumed,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sim := simCluster(t, tt.artifact, tt.suspended)
			sim.WriteStatus(
				context.Background(),
				kustomizationGVR(),
				checkoutKey,
				func(obj *unstructured.Unstructured) {
					_ = unstructured.SetNestedField(obj.Object, tt.applied, "status", "lastAppliedRevision")
				},
			)
			a := ask(tt.reason)
			a.Commit = tt.commit
			got, err := simFlux(sim).Request(context.Background(), checkoutKey, a)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got.Outcome)

			assert.Equal(t, tt.wantFetch,
				requestedAt(t, sim.Client, gitRepositoryGVR(), fluxSystemNS, "platform") != "")
			ks := sim.Kustomization(checkoutKey)
			if tt.want == OutcomeResumed {
				assert.Equal(t, "T1", ks.GetAnnotations()[fluxmeta.ReconcileRequestAnnotation])
				assert.Equal(t, Stamped{Outcome: OutcomeResumed, Token: "T1", Commit: testCommit}, got)
				return
			}
			assert.Empty(t, ks.GetAnnotations()[fluxmeta.ReconcileRequestAnnotation])
			assert.Equal(t, tt.suspended, suspended(ks), "nothing resumed")
		})
	}
}

// TestFluxRequest_TheOperationIsRecordedBeforeTheResume: a restart reads the trigger's
// status.operation to find every apply that may have started, so the record is written before the
// resume, and a record that cannot be written resumes nothing.
func TestFluxRequest_TheOperationIsRecordedBeforeTheResume(t *testing.T) {
	sim := simCluster(t, "main@sha1:"+testCommit, true)
	a := ask(v1alpha3.ApplyReasonRefusal)
	recorded := false
	a.Record = func(context.Context) error {
		assert.True(t, suspended(sim.Kustomization(checkoutKey)), "not resumed before it is recorded")
		recorded = true
		return nil
	}
	_, err := simFlux(sim).Request(context.Background(), checkoutKey, a)
	require.NoError(t, err)
	assert.True(t, recorded)

	sim = simCluster(t, "main@sha1:"+testCommit, true)
	a.Record = func(context.Context) error { return errors.New("the trigger is being deleted") }
	_, err = simFlux(sim).Request(context.Background(), checkoutKey, a)
	require.ErrorContains(t, err, "record the operation")
	assert.False(t, isPermanent(err))
	assert.True(t, suspended(sim.Kustomization(checkoutKey)), "nothing resumed")
}

// TestFluxRequest_AStaleDecisionResumesNothing is the customer review's first blocker, still: a
// decision is checked again just before the resume, and one that no longer holds resumes nothing.
func TestFluxRequest_AStaleDecisionResumesNothing(t *testing.T) {
	for _, artifact := range []string{"main@sha1:" + testCommit, "main@sha1:" + olderCommit} {
		sim := simCluster(t, artifact, true)
		a := ask(v1alpha3.ApplyReasonRefusal)
		a.Current = func() bool { return false }
		_, err := simFlux(sim).Request(context.Background(), checkoutKey, a)
		require.ErrorIs(t, err, errStale, artifact)
		assert.True(t, suspended(sim.Kustomization(checkoutKey)), "a stale decision resumes nothing")
	}
}

// TestFluxRequest_AnArtifactThatMovesBeforeTheResumeIsStale is measurement point 6: a resume applies
// whatever the artifact holds then, so the artifact is read again just before it, and one that
// moved off the commit resumes nothing.
func TestFluxRequest_AnArtifactThatMovesBeforeTheResumeIsStale(t *testing.T) {
	sim := simCluster(t, "main@sha1:"+testCommit, true)
	reads := 0
	sim.Client.PrependReactor("get", "gitrepositories", func(k8stesting.Action) (bool, runtime.Object, error) {
		reads++
		if reads < 2 {
			return false, nil, nil
		}
		return true, gitRepositoryObj(fluxSystemNS, "platform", testRepoURL, branchRef(testBranch),
			"main@sha1:"+olderCommit), nil
	})
	_, err := simFlux(sim).Request(context.Background(), checkoutKey, ask(v1alpha3.ApplyReasonRefusal))
	require.ErrorIs(t, err, errStale)
	assert.True(t, suspended(sim.Kustomization(checkoutKey)))
}

// TestFluxRequest_AFetchThatEndsElsewhere: a fetch that does not bring the commit is the decision
// going stale when the branch moved on, and a failure (retried with backoff) when it did not.
func TestFluxRequest_AFetchThatEndsElsewhere(t *testing.T) {
	sim := simCluster(t, "main@sha1:"+olderCommit, true)
	sim.Fetches(platformKey, "main@sha1:"+olderCommit)
	_, err := simFlux(sim).Request(context.Background(), checkoutKey, ask(v1alpha3.ApplyReasonForeignCommit))
	require.ErrorContains(t, err, "fetched main@sha1:"+olderCommit+", not commit "+testCommit)
	assert.False(t, isPermanent(err))
	require.NotErrorIs(t, err, errStale)

	a := ask(v1alpha3.ApplyReasonForeignCommit)
	a.Current = func() bool { return false }
	_, err = simFlux(sim).Request(context.Background(), checkoutKey, a)
	require.ErrorIs(t, err, errStale)
	assert.True(t, suspended(sim.Kustomization(checkoutKey)))
}

// TestFluxRequest_TakesSuspendBackFromAnUpdate: `flux resume` and `flux suspend` write spec.suspend
// with a merge patch, once. That is no manifest owning the field, so a request takes it back.
func TestFluxRequest_TakesSuspendBackFromAnUpdate(t *testing.T) {
	ks := fluxsim.OwnSuspend(t, kustomizationObj(fluxSystemNS, "checkout", gitRepoSourceRef("platform")), true,
		"flux", metav1.ManagedFieldsOperationUpdate)
	sim := fluxsim.New(t, newSourceClient(ks,
		gitRepositoryObj(fluxSystemNS, "platform", testRepoURL, branchRef(testBranch), "main@sha1:"+testCommit)))
	flux := simFlux(sim)
	stamped, err := flux.Request(context.Background(), checkoutKey, ask(v1alpha3.ApplyReasonRefusal))
	require.NoError(t, err)
	follow(t, flux, &stamped)
	assert.True(t, heldByUs(sim.Kustomization(checkoutKey)))
	assert.Empty(t, suspendOwner(sim.Kustomization(checkoutKey)))
}

// TestFluxProgress_AFailedApplyIsSuspendedAgain: the reconcile that read the token failed, which
// is Failed with Flux's message; the Kustomization is suspended again, so Flux's own retryInterval
// requeue applies nothing (a requeue into a suspended object does nothing). The worker owns the
// retry.
func TestFluxProgress_AFailedApplyIsSuspendedAgain(t *testing.T) {
	sim := simCluster(t, "main@sha1:"+olderCommit, true)
	sim.FailApplies("ConfigMap/apps/x dry-run failed")
	flux := simFlux(sim)
	stamped, err := flux.Request(context.Background(), checkoutKey, ask(v1alpha3.ApplyReasonForeignCommit))
	require.NoError(t, err)

	steps := follow(t, flux, &stamped)
	done := steps[len(steps)-1]
	assert.Equal(t, git.ApplyFailed, done.Outcome)
	assert.Contains(t, done.Message, "ConfigMap/apps/x dry-run failed")
	assert.True(t, suspended(sim.Kustomization(checkoutKey)))

	sim.FailApplies("")
	before := sim.Reconciles(checkoutKey)
	sim.Requeue(checkoutKey)
	eventually(t, func() bool { return sim.Reconciles(checkoutKey) > before }, "the retryInterval requeue runs")
	assert.Empty(t, sim.Applied(), "and applies nothing")
}

// TestFluxProgress_ADependencyNotReadyFails: a dependency that is not ready is a failure of its own
// reason, retried by the worker like any other.
func TestFluxProgress_ADependencyNotReadyFails(t *testing.T) {
	sim := simCluster(t, "main@sha1:"+testCommit, true)
	sim.BlockDependencies(true)
	flux := simFlux(sim)
	stamped, err := flux.Request(context.Background(), checkoutKey, ask(v1alpha3.ApplyReasonRefusal))
	require.NoError(t, err)
	done := follow(t, flux, &stamped)
	assert.Equal(t, git.ApplyFailed, done[len(done)-1].Outcome)
	assert.Contains(t, done[len(done)-1].Message, "a dependency is not ready")
}

// TestFluxProgress_ASuspendDoesNotStopARunningApply is measurement point 2, and the Unresolved half
// of finding 4: an overdue operation is suspended at once, but that suspend does not stop the apply
// already running, so the operation completes only once the suspend's token is handled, which is
// after that apply ended.
func TestFluxProgress_ASuspendDoesNotStopARunningApply(t *testing.T) {
	sim := simCluster(t, "main@sha1:"+olderCommit, true)
	held := sim.HoldNextApply()
	flux := simFlux(sim)
	stamped, err := flux.Request(context.Background(), checkoutKey, ask(v1alpha3.ApplyReasonForeignCommit))
	require.NoError(t, err)
	<-held

	stamped.Overdue = true
	progress, err := flux.Progress(context.Background(), checkoutKey, &stamped)
	require.NoError(t, err)
	assert.Equal(t, git.ApplySettling, progress.Outcome, "suspended at once")
	require.NotEmpty(t, progress.FenceToken)
	assert.True(t, suspended(sim.Kustomization(checkoutKey)))
	progress, err = flux.Progress(context.Background(), checkoutKey, &stamped)
	require.NoError(t, err)
	assert.Equal(t, git.ApplySettling, progress.Outcome, "the apply still runs, so the suspend is not handled")

	sim.Release()
	steps := follow(t, flux, &stamped)
	assert.Equal(t, git.ApplySucceeded, steps[len(steps)-1].Outcome)
	assert.Equal(t, []string{"flux-system/checkout main@sha1:" + testCommit}, sim.Applied(),
		"the suspend did not stop the apply")
}

// TestFluxProgress_ATokenStampedOverOursStandsInForIt: `flux reconcile ks` between the resume and
// its reconcile replaces the token; the reconcile that handles the newer one started after the
// resume, so it ends the operation as ours would have.
func TestFluxProgress_ATokenStampedOverOursStandsInForIt(t *testing.T) {
	sim := simCluster(t, "main@sha1:"+testCommit, true)
	held := sim.HoldNextApply()
	flux := simFlux(sim)
	stamped, err := flux.Request(context.Background(), checkoutKey, ask(v1alpha3.ApplyReasonRefusal))
	require.NoError(t, err)
	<-held
	patch := []byte(`{"metadata":{"annotations":{"reconcile.fluxcd.io/requestedAt":"by-hand"}}}`)
	_, err = sim.Client.Resource(kustomizationGVR()).Namespace(fluxSystemNS).
		Patch(context.Background(), "checkout", types.MergePatchType, patch, metav1.PatchOptions{FieldManager: "flux"})
	require.NoError(t, err)

	sim.Release()
	steps := follow(t, flux, &stamped)
	assert.Equal(t, git.ApplySucceeded, steps[len(steps)-1].Outcome)
	assert.True(t, suspended(sim.Kustomization(checkoutKey)))
}

// TestFluxProgress_AResumeAfterTheFenceIsFencedAgain: a Kustomization resumed by somebody else while
// the operation settles is suspended again before the operation completes.
func TestFluxProgress_AResumeAfterTheFenceIsFencedAgain(t *testing.T) {
	sim := simCluster(t, "main@sha1:"+testCommit, true)
	flux := simFlux(sim)
	stamped, err := flux.Request(context.Background(), checkoutKey, ask(v1alpha3.ApplyReasonRefusal))
	require.NoError(t, err)
	var progress Progress
	for progress.Outcome != git.ApplySettling {
		progress, err = flux.Progress(context.Background(), checkoutKey, &stamped)
		require.NoError(t, err)
	}
	first := stamped.FenceToken
	patch := []byte(`{"spec":{"suspend":false}}`)
	_, err = sim.Client.Resource(kustomizationGVR()).Namespace(fluxSystemNS).
		Patch(context.Background(), "checkout", types.MergePatchType, patch, metav1.PatchOptions{FieldManager: "flux"})
	require.NoError(t, err)

	steps := follow(t, flux, &stamped)
	assert.Equal(t, git.ApplySucceeded, steps[len(steps)-1].Outcome)
	assert.NotEqual(t, first, stamped.FenceToken, "fenced again")
	assert.True(t, suspended(sim.Kustomization(checkoutKey)))
}

// TestFlux_SuspendAndRelease: the trigger reconciler's half. Suspend fences, Suspension reports
// held and fenced once the token is handled, and Release hands spec.suspend back: the Kustomization
// is resumed, and holds nothing of GitOps Reverser's.
func TestFlux_SuspendAndRelease(t *testing.T) {
	sim := simCluster(t, "main@sha1:"+testCommit, false)
	flux := simFlux(sim)

	before, err := flux.Suspension(context.Background(), checkoutKey)
	require.NoError(t, err)
	assert.False(t, before.Suspended)
	assert.False(t, before.Held)

	token, err := flux.Suspend(context.Background(), checkoutKey)
	require.NoError(t, err)
	eventually(t, func() bool {
		held, err := flux.Suspension(context.Background(), checkoutKey)
		return err == nil && held.Suspended && held.Held && held.Fenced && held.Token == token
	}, "held and fenced")

	require.NoError(t, flux.Release(context.Background(), checkoutKey))
	ks := sim.Kustomization(checkoutKey)
	assert.False(t, suspended(ks))
	assert.False(t, heldByUs(ks))
	assert.Empty(t, ks.GetAnnotations()[fluxmeta.ReconcileRequestAnnotation])
}

// TestFluxValidate_SuspendOwnedElsewhere: a manifest that sets spec.suspend, applied by another
// manager, would resume the Kustomization on its next apply: Configured=False, naming the manager.
// A merge patch by `flux resume` is no such owner.
func TestFluxValidate_SuspendOwnedElsewhere(t *testing.T) {
	for name, tt := range map[string]struct {
		operation metav1.ManagedFieldsOperationType
		want      string
	}{
		"applied by a parent Kustomization": {metav1.ManagedFieldsOperationApply, ReasonSuspendOwnedElsewhere},
		"written once by flux resume":       {metav1.ManagedFieldsOperationUpdate, ReasonSucceeded},
	} {
		t.Run(name, func(t *testing.T) {
			ks := fluxsim.OwnSuspend(t, kustomizationObj(fluxSystemNS, "checkout", gitRepoSourceRef("platform")), false,
				git.FluxFieldManager, tt.operation)
			flux := pinnedFlux(newSourceClient(ks,
				gitRepositoryObj(fluxSystemNS, "platform", testRepoURL, branchRef(testBranch), "")))
			flux.CanI = allowAll
			verdict := flux.Validate(context.Background(), checkoutKey)
			assert.Equal(t, tt.want, verdict.Reason, verdict.Message)
			if tt.want == ReasonSuspendOwnedElsewhere {
				assert.Contains(t, verdict.Message, `field manager "kustomize-controller"`)
				assert.NotNil(t, verdict.Source)
			}
		})
	}
}

// eventually waits for cond, failing the test after a few seconds.
func eventually(t *testing.T, cond func() bool, why string) {
	t.Helper()
	require.Eventually(t, cond, 5*time.Second, time.Millisecond, why)
}
