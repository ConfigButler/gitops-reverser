// SPDX-License-Identifier: Apache-2.0

package applier

// The suspended Flux lifecycle, against fluxSim: a resume with a token, the outcome read once the
// reconcile that read it ended, the suspend fence, and the hand-back. The first tests are the third
// review's probes and the measurements of docs/design/apply-trigger.md, each of which
// failed against the request-based client this replaced.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	ks.SetUID("checkout-1")
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
func follow(t *testing.T, flux *Flux, s *Started) []Progress {
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

	started, err := flux.Request(context.Background(), checkoutKey, ask(v1alpha3.ApplyReasonForeignCommit))
	require.NoError(t, err)
	require.Equal(t, OutcomeResumed, started.Outcome)
	<-held
	assert.Equal(t, "main@sha1:"+testCommit, statusString(sim.Kustomization(checkoutKey), "lastAttemptedRevision"))
	progress, err := flux.Progress(context.Background(), checkoutKey, &started)
	require.NoError(t, err)
	assert.Equal(t, Progress{Outcome: git.ApplyRunning}, progress, "the apply is still running")

	sim.Release()
	steps := follow(t, flux, &started)
	require.GreaterOrEqual(t, len(steps), 2)
	tail := steps[len(steps)-2:]
	assert.Equal(t, []git.ApplyOutcome{git.ApplySettling, git.ApplySucceeded}, outcomes(tail))
	assert.Equal(t, "main@sha1:"+testCommit, tail[1].AppliedRevision)
	assert.Equal(t, started.FenceToken, tail[1].FenceToken)
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

	started, err := flux.Request(context.Background(), checkoutKey, ask(v1alpha3.ApplyReasonRefusal))
	require.NoError(t, err)
	steps := follow(t, flux, &started)
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
	started, err := simFlux(sim).Request(context.Background(), checkoutKey, ask(v1alpha3.ApplyReasonCatchUp))
	require.NoError(t, err)
	assert.Equal(t, OutcomeResumed, started.Outcome, "the Kustomization never applied the commit")
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
				assert.Equal(t, []any{OutcomeResumed, "T1", testCommit, nil},
					[]any{got.Outcome, got.Token, got.Commit, got.Unconfirmed})
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

// resumedWithT1 reports whether the Kustomization was resumed with the token ask gives, T1.
func resumedWithT1(ks *unstructured.Unstructured) bool {
	return !suspended(ks) || ks.GetAnnotations()[fluxmeta.ReconcileRequestAnnotation] == "T1"
}

// TestFluxRequest_AKustomizationThatChangesDuringTheFetchIsNotResumed is review 5's fourth
// finding: a request read the Kustomization once, waited for the fetch, and resumed whatever the
// Kustomization was by then. One that applies another source now, one that was replaced, and one
// whose spec.suspend another manager applies now are all read again before the resume, and none is
// resumed.
func TestFluxRequest_AKustomizationThatChangesDuringTheFetchIsNotResumed(t *testing.T) {
	for name, tt := range map[string]struct {
		change   func(t *testing.T, sim *fluxsim.Sim)
		wantErr  string
		wantPerm bool
	}{
		"it applies another GitRepository now": {
			change: func(_ *testing.T, sim *fluxsim.Sim) {
				sim.Edit(checkoutKey, func(ks *unstructured.Unstructured) {
					_ = unstructured.SetNestedMap(ks.Object, gitRepoSourceRef("other"), "spec", "sourceRef")
				})
			},
			wantErr: "applies GitRepository flux-system/other now",
		},
		"it was replaced": {
			change: func(t *testing.T, sim *fluxsim.Sim) {
				// Through the tracker: a reactor runs while the client is locked.
				tracker := sim.Client.Tracker()
				held, err := tracker.Get(kustomizationGVR(), fluxSystemNS, checkoutKey.Name)
				require.NoError(t, err)
				replacement, _ := held.(*unstructured.Unstructured)
				replacement = replacement.DeepCopy()
				require.NoError(t, tracker.Delete(kustomizationGVR(), fluxSystemNS, checkoutKey.Name))
				replacement.SetUID("checkout-2")
				replacement.SetResourceVersion("replaced")
				require.NoError(t, tracker.Create(kustomizationGVR(), replacement, fluxSystemNS))
			},
			wantErr: "was replaced",
		},
		"another manager applies its spec.suspend now": {
			change: func(t *testing.T, sim *fluxsim.Sim) {
				sim.Edit(checkoutKey, func(ks *unstructured.Unstructured) {
					fluxsim.OwnSuspend(t, ks, true, "kustomize-controller", metav1.ManagedFieldsOperationApply)
				})
			},
			wantErr: `applied by field manager "kustomize-controller"`, wantPerm: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			sim := simCluster(t, "main@sha1:"+olderCommit, true)
			require.NoError(t, sim.Client.Tracker().Add(
				gitRepositoryObj(fluxSystemNS, "other", testRepoURL, branchRef(testBranch), "main@sha1:"+testCommit)))
			changed := false
			stamped := func(k8stesting.Action) (bool, runtime.Object, error) {
				if !changed {
					// The GitRepository is stamped: the request waits for the fetch now.
					changed = true
					tt.change(t, sim)
				}
				return false, nil, nil
			}
			sim.Client.PrependReactor("patch", "gitrepositories", stamped)
			recorded := false
			a := ask(v1alpha3.ApplyReasonForeignCommit)
			a.Record = func(context.Context) error {
				recorded = true
				return nil
			}

			_, err := simFlux(sim).Request(context.Background(), checkoutKey, a)

			require.ErrorContains(t, err, tt.wantErr)
			assert.Equal(t, tt.wantPerm, isPermanent(err))
			require.NotErrorIs(t, err, errStale)
			assert.True(t, changed)
			assert.False(t, recorded, "found before anything is recorded")
			assert.False(t, resumedWithT1(sim.Kustomization(checkoutKey)), "nothing resumed")
		})
	}
}

// TestFluxRequest_TheResumeIsGuardedByTheKustomizationAsRead: what changes between the last read
// and the resume (here, while the operation is recorded) makes the resume's preconditions fail,
// so it never lands on an object other than the one checked. A change that leaves the binding as
// it was is read again and resumed; one that does not is not resumed.
func TestFluxRequest_TheResumeIsGuardedByTheKustomizationAsRead(t *testing.T) {
	for name, tt := range map[string]struct {
		change  func(ks *unstructured.Unstructured)
		wantErr string
	}{
		"a label, which changes nothing it was checked for": {
			change: func(ks *unstructured.Unstructured) { ks.SetLabels(map[string]string{"team": "platform"}) },
		},
		"another GitRepository": {
			change: func(ks *unstructured.Unstructured) {
				_ = unstructured.SetNestedMap(ks.Object, gitRepoSourceRef("other"), "spec", "sourceRef")
			},
			wantErr: "applies GitRepository flux-system/other now",
		},
	} {
		t.Run(name, func(t *testing.T) {
			sim := simCluster(t, "main@sha1:"+testCommit, true)
			var resumes []string
			counted := func(action k8stesting.Action) (bool, runtime.Object, error) {
				patch, _ := action.(k8stesting.PatchActionImpl)
				if patch.PatchType == types.ApplyPatchType && strings.Contains(string(patch.Patch), `"suspend":false`) {
					resumes = append(resumes, string(patch.Patch))
				}
				return false, nil, nil
			}
			sim.Client.PrependReactor("patch", "kustomizations", counted)
			a := ask(v1alpha3.ApplyReasonRefusal)
			a.Record = func(context.Context) error {
				sim.Edit(checkoutKey, tt.change)
				return nil
			}

			started, err := simFlux(sim).Request(context.Background(), checkoutKey, a)

			require.NotEmpty(t, resumes)
			assert.Contains(t, resumes[0], `"resourceVersion"`, "the resume carries the resourceVersion read")
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				assert.False(t, resumedWithT1(sim.Kustomization(checkoutKey)), "nothing resumed")
				return
			}
			require.NoError(t, err)
			require.NoError(t, started.Unconfirmed)
			assert.Len(t, resumes, 2, "the first resume conflicted, and the second was made over a fresh read")
			assert.True(t, resumedWithT1(sim.Kustomization(checkoutKey)))
		})
	}
}

// TestFluxRequest_ADecisionThatGoesStaleWhileRecordedResumesNothing is review 5's sixth finding:
// recording the operation can wait on the status writer and the API server, and the branch can
// move on meanwhile. The decision is asked again after the record, just before the resume.
func TestFluxRequest_ADecisionThatGoesStaleWhileRecordedResumesNothing(t *testing.T) {
	sim := simCluster(t, "main@sha1:"+testCommit, true)
	holds := true
	a := ask(v1alpha3.ApplyReasonRefusal)
	a.Current = func() bool { return holds }
	a.Record = func(context.Context) error {
		holds = false
		return nil
	}

	_, err := simFlux(sim).Request(context.Background(), checkoutKey, a)

	require.ErrorIs(t, err, errStale)
	assert.False(t, resumedWithT1(sim.Kustomization(checkoutKey)), "nothing resumed")
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
	started, err := flux.Request(context.Background(), checkoutKey, ask(v1alpha3.ApplyReasonRefusal))
	require.NoError(t, err)
	follow(t, flux, &started)
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
	started, err := flux.Request(context.Background(), checkoutKey, ask(v1alpha3.ApplyReasonForeignCommit))
	require.NoError(t, err)

	steps := follow(t, flux, &started)
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
	started, err := flux.Request(context.Background(), checkoutKey, ask(v1alpha3.ApplyReasonRefusal))
	require.NoError(t, err)
	done := follow(t, flux, &started)
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
	started, err := flux.Request(context.Background(), checkoutKey, ask(v1alpha3.ApplyReasonForeignCommit))
	require.NoError(t, err)
	<-held

	started.Overdue = true
	progress, err := flux.Progress(context.Background(), checkoutKey, &started)
	require.NoError(t, err)
	assert.Equal(t, git.ApplySettling, progress.Outcome, "suspended at once")
	require.NotEmpty(t, progress.FenceToken)
	assert.True(t, suspended(sim.Kustomization(checkoutKey)))
	progress, err = flux.Progress(context.Background(), checkoutKey, &started)
	require.NoError(t, err)
	assert.Equal(t, git.ApplySettling, progress.Outcome, "the apply still runs, so the suspend is not handled")

	sim.Release()
	steps := follow(t, flux, &started)
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
	started, err := flux.Request(context.Background(), checkoutKey, ask(v1alpha3.ApplyReasonRefusal))
	require.NoError(t, err)
	<-held
	patch := []byte(`{"metadata":{"annotations":{"reconcile.fluxcd.io/requestedAt":"by-hand"}}}`)
	_, err = sim.Client.Resource(kustomizationGVR()).Namespace(fluxSystemNS).
		Patch(context.Background(), "checkout", types.MergePatchType, patch, metav1.PatchOptions{FieldManager: "flux"})
	require.NoError(t, err)

	sim.Release()
	steps := follow(t, flux, &started)
	assert.Equal(t, git.ApplySucceeded, steps[len(steps)-1].Outcome)
	assert.True(t, suspended(sim.Kustomization(checkoutKey)))
}

// TestFluxProgress_AResumeAfterTheFenceIsFencedAgain: a Kustomization resumed by somebody else while
// the operation settles is suspended again before the operation completes.
func TestFluxProgress_AResumeAfterTheFenceIsFencedAgain(t *testing.T) {
	sim := simCluster(t, "main@sha1:"+testCommit, true)
	flux := simFlux(sim)
	started, err := flux.Request(context.Background(), checkoutKey, ask(v1alpha3.ApplyReasonRefusal))
	require.NoError(t, err)
	var progress Progress
	for progress.Outcome != git.ApplySettling {
		progress, err = flux.Progress(context.Background(), checkoutKey, &started)
		require.NoError(t, err)
	}
	first := started.FenceToken
	patch := []byte(`{"spec":{"suspend":false}}`)
	_, err = sim.Client.Resource(kustomizationGVR()).Namespace(fluxSystemNS).
		Patch(context.Background(), "checkout", types.MergePatchType, patch, metav1.PatchOptions{FieldManager: "flux"})
	require.NoError(t, err)

	steps := follow(t, flux, &started)
	assert.Equal(t, git.ApplySucceeded, steps[len(steps)-1].Outcome)
	assert.NotEqual(t, first, started.FenceToken, "fenced again")
	assert.True(t, suspended(sim.Kustomization(checkoutKey)))
}

// TestFluxProgress_ASuspendBeforeTheReconcileIsNoApply is review 4's E: a suspend that lands
// between the resume and the reconcile that reads it makes that reconcile a suspended return, which
// marks the token handled and leaves the Ready of the last apply. A Refusal of the applied commit
// read that Ready as a clean Succeeded, with nothing reverted. Now no reconcile that built since
// the resume is no apply: Failed, and the worker asks again.
func TestFluxProgress_ASuspendBeforeTheReconcileIsNoApply(t *testing.T) {
	for name, suspend := range map[string]func(t *testing.T, sim *fluxsim.Sim, flux *Flux){
		"flux suspend": func(t *testing.T, sim *fluxsim.Sim, _ *Flux) {
			_, err := sim.Client.Resource(kustomizationGVR()).Namespace(fluxSystemNS).Patch(context.Background(),
				"checkout", types.MergePatchType, []byte(`{"spec":{"suspend":true}}`),
				metav1.PatchOptions{FieldManager: "flux"})
			require.NoError(t, err)
		},
		"the trigger's own suspend": func(t *testing.T, _ *fluxsim.Sim, flux *Flux) {
			_, err := flux.Suspend(context.Background(), checkoutKey)
			require.NoError(t, err)
		},
	} {
		t.Run(name, func(t *testing.T) {
			sim := simCluster(t, "main@sha1:"+testCommit, true)
			flux := simFlux(sim)
			resume := sim.PauseKustomizations()
			started, err := flux.Request(context.Background(), checkoutKey, ask(v1alpha3.ApplyReasonRefusal))
			require.NoError(t, err)
			suspend(t, sim, flux)
			resume()

			steps := follow(t, flux, &started)
			done := steps[len(steps)-1]
			assert.Equal(t, git.ApplyFailed, done.Outcome, "nothing was applied")
			assert.Contains(t, done.Message, "no reconcile applied anything after the resume")
			assert.Empty(t, sim.Applied())
		})
	}
}

// TestFluxProgress_AnApplyOfAnOlderArtifactIsAskedAgain is review 4's G: kustomize-controller reads
// the GitRepository from its informer cache, so the reconcile the resume started can apply the
// artifact before the one the resume was checked against. That was Succeeded ("another revision"),
// and the commit then sat in a Kustomization suspended again, never applied. Now only an applied
// revision the source holds now is the source having moved on; an older one is Failed, and the next
// request applies the commit.
func TestFluxProgress_AnApplyOfAnOlderArtifactIsAskedAgain(t *testing.T) {
	sim := simCluster(t, "main@sha1:"+olderCommit, true)
	sim.LagSourceCache(platformKey, "main@sha1:"+olderCommit)
	flux := simFlux(sim)

	started, err := flux.Request(context.Background(), checkoutKey, ask(v1alpha3.ApplyReasonForeignCommit))
	require.NoError(t, err)
	steps := follow(t, flux, &started)
	done := steps[len(steps)-1]
	assert.Equal(t, git.ApplyFailed, done.Outcome)
	assert.Contains(t, done.Message, "an artifact older than the one the Kustomization was resumed for")
	assert.Equal(t, "main@sha1:"+olderCommit, done.AppliedRevision)

	sim.LagSourceCache(platformKey, "")
	again := ask(v1alpha3.ApplyReasonForeignCommit)
	again.Token = "T2"
	started, err = flux.Request(context.Background(), checkoutKey, again)
	require.NoError(t, err)
	require.Equal(t, OutcomeResumed, started.Outcome, "the commit is not applied, so it is not current")
	steps = follow(t, flux, &started)
	assert.Equal(t, git.ApplySucceeded, steps[len(steps)-1].Outcome)
	assert.Equal(t, "main@sha1:"+testCommit, steps[len(steps)-1].AppliedRevision)
}

// TestFluxProgress_AnApplyOfANewerArtifactSucceeds is the residual race the design accepts: the
// source moved on between the recheck and the reconcile, which applied what the source holds now.
func TestFluxProgress_AnApplyOfANewerArtifactSucceeds(t *testing.T) {
	const newer = "main@sha1:9999999999999999999999999999999999999999"
	sim := simCluster(t, "main@sha1:"+testCommit, true)
	flux := simFlux(sim)
	resume := sim.PauseKustomizations()
	started, err := flux.Request(context.Background(), checkoutKey, ask(v1alpha3.ApplyReasonRefusal))
	require.NoError(t, err)
	sim.WriteStatus(context.Background(), gitRepositoryGVR(), platformKey, func(obj *unstructured.Unstructured) {
		_ = unstructured.SetNestedField(obj.Object, newer, "status", "artifact", "revision")
	})
	resume()

	steps := follow(t, flux, &started)
	done := steps[len(steps)-1]
	assert.Equal(t, git.ApplySucceeded, done.Outcome)
	assert.Equal(t, newer, done.AppliedRevision)
	assert.Contains(t, done.Message, "not commit "+testCommit)
}

// TestFluxProgress_ARecoveredTokenStampedOverIsJudgedOnWhatRan is review 4's I: a process that
// resumed with T1, saw the apply end and suspended with its fence, then stopped before reporting,
// leaves T1 nowhere on the Kustomization. That ended Failed, though the apply ran. Now a recovered
// token handled by a later one is judged on what ran since the operation was asked for.
func TestFluxProgress_ARecoveredTokenStampedOverIsJudgedOnWhatRan(t *testing.T) {
	sim := simCluster(t, "main@sha1:"+olderCommit, true)
	flux := simFlux(sim)
	asked := time.Now()
	started, err := flux.Request(context.Background(), checkoutKey, ask(v1alpha3.ApplyReasonForeignCommit))
	require.NoError(t, err)
	var progress Progress
	for progress.Outcome != git.ApplySettling {
		progress, err = flux.Progress(context.Background(), checkoutKey, &started)
		require.NoError(t, err)
	}
	eventually(t, func() bool { return tokenHandled(sim.Kustomization(checkoutKey), started.FenceToken) },
		"the fence is handled; then the process stops")

	recovered := Started{Outcome: OutcomeResumed, Token: "T1", Commit: testCommit, Recovered: true, RequestedAt: asked}
	steps := follow(t, flux, &recovered)
	done := steps[len(steps)-1]
	assert.Equal(t, git.ApplySucceeded, done.Outcome, done.Message)
	assert.Equal(t, "main@sha1:"+testCommit, done.AppliedRevision)
	assert.Len(t, sim.Applied(), 1)
}

// TestFluxRequest_AnUnansweredResumeIsFollowed is review 4's B, the undelivered half: a resume
// whose write timed out may still have been made, and a RequestFailed left the Kustomization
// resumed with nothing following it. Now it is an Unconfirmed operation, followed like a recovered
// one: a resume that was made applies and is suspended again; one that was not is Failed, and
// nothing ran. A write the API server refused is still an error.
func TestFluxRequest_AnUnansweredResumeIsFollowed(t *testing.T) {
	for name, tt := range map[string]struct {
		landed  bool
		answer  error
		want    git.ApplyOutcome
		applied int
	}{
		"made, then timed out":     {landed: true, answer: context.DeadlineExceeded, want: git.ApplySucceeded, applied: 1},
		"not made, and timed out":  {answer: context.DeadlineExceeded, want: git.ApplyFailed},
		"made, then a 504 answers": {landed: true, answer: apierrors.NewTimeoutError("slow", 1), want: git.ApplySucceeded, applied: 1},
		"refused":                  {answer: apierrors.NewForbidden(kustomizationGVR().GroupResource(), "checkout", errors.New("no"))},
	} {
		t.Run(name, func(t *testing.T) {
			sim := simCluster(t, "main@sha1:"+testCommit, true)
			flux := simFlux(sim)
			answered := false
			resumeTheResume := func(obj *unstructured.Unstructured) {
				_ = unstructured.SetNestedField(obj.Object, false, "spec", "suspend")
				obj.SetAnnotations(map[string]string{fluxmeta.ReconcileRequestAnnotation: "T1"})
				obj.SetGeneration(obj.GetGeneration() + 1)
			}
			sim.Client.PrependReactor(
				"patch",
				"kustomizations",
				func(action k8stesting.Action) (bool, runtime.Object, error) {
					patch, _ := action.(k8stesting.PatchActionImpl)
					resume := patch.PatchType == types.ApplyPatchType &&
						strings.Contains(string(patch.Patch), `"suspend":false`)
					if answered || !resume {
						return false, nil, nil
					}
					answered = true
					if tt.landed {
						sim.WriteStatus(context.Background(), kustomizationGVR(), checkoutKey, resumeTheResume)
					}
					return true, nil, tt.answer
				},
			)

			started, err := flux.Request(context.Background(), checkoutKey, ask(v1alpha3.ApplyReasonRefusal))
			if tt.want == "" {
				require.ErrorContains(t, err, "resume")
				return
			}
			require.NoError(t, err)
			require.Error(t, started.Unconfirmed)
			steps := follow(t, flux, &started)
			done := steps[len(steps)-1]
			assert.Equal(t, tt.want, done.Outcome, done.Message)
			assert.Len(t, sim.Applied(), tt.applied)
			assert.True(t, suspended(sim.Kustomization(checkoutKey)), "suspended again either way")
		})
	}
}

// TestReconcileMark_MovesOnAReconcileThatBuilt pins the evidence a verdict reads: Ready's
// lastTransitionTime, and the latest status.history snapshot (kustomize-controller v1.7 and later),
// either of which moving is a reconcile that got past the suspend check.
func TestReconcileMark_MovesOnAReconcileThatBuilt(t *testing.T) {
	ks := kustomizationObj(fluxSystemNS, "checkout", gitRepoSourceRef("platform"))
	fluxsim.SetReady(ks, metav1.ConditionTrue, fluxmeta.ReconciliationSucceededReason, "applied")
	before := markOf(ks)

	moved := ks.DeepCopy()
	conditions, _, _ := unstructured.NestedSlice(moved.Object, "status", "conditions")
	conditions[0].(map[string]any)["lastTransitionTime"] = "2026-10-18T10:00:05Z"
	require.NoError(t, unstructured.SetNestedSlice(moved.Object, conditions, "status", "conditions"))
	assert.NotEqual(t, before, markOf(moved), "Ready moved")
	assert.True(t, markOf(moved).since(time.Date(2026, 10, 18, 10, 0, 5, 900, time.UTC)), "within the second")
	assert.False(t, markOf(moved).since(time.Date(2026, 10, 18, 10, 0, 6, 0, time.UTC)))

	counted := ks.DeepCopy()
	require.NoError(t, unstructured.SetNestedSlice(counted.Object, []any{map[string]any{
		"digest": "sha256:abc", "totalReconciliations": int64(2), "lastReconciled": "2026-10-18T10:00:05Z",
	}}, "status", "history"))
	again := counted.DeepCopy()
	history, _, _ := unstructured.NestedSlice(again.Object, "status", "history")
	history[0].(map[string]any)["totalReconciliations"] = int64(3)
	require.NoError(t, unstructured.SetNestedSlice(again.Object, history, "status", "history"))
	assert.NotEqual(t, markOf(counted), markOf(again), "one more reconcile of the same digest, in the same second")
	assert.False(t, before.since(time.Time{}), "no mark at all is no evidence")
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
		return err == nil && held.Suspended && held.Held && held.Fenced &&
			sim.Kustomization(checkoutKey).GetAnnotations()[fluxmeta.ReconcileRequestAnnotation] == token
	}, "held and fenced")

	keeper, err := flux.Release(context.Background(), checkoutKey)
	require.NoError(t, err)
	assert.Empty(t, keeper)
	ks := sim.Kustomization(checkoutKey)
	assert.False(t, suspended(ks))
	assert.False(t, heldByUs(ks))
	assert.Empty(t, ks.GetAnnotations()[fluxmeta.ReconcileRequestAnnotation])
}

// TestFlux_ReleaseResumesASuspendItSharedWithFlux is the fourth review's co-ownership gap: a suspend
// that took over a `flux suspend` shares spec.suspend with that update, so the empty apply alone
// left the Kustomization suspended while the Released Event said it resumed. Release takes the
// update's share first. A manager that applies spec.suspend keeps it, and Release names it.
func TestFlux_ReleaseResumesASuspendItSharedWithFlux(t *testing.T) {
	sim := simCluster(t, "main@sha1:"+testCommit, false)
	flux := simFlux(sim)
	patch := []byte(`{"spec":{"suspend":true}}`)
	_, err := sim.Client.Resource(kustomizationGVR()).Namespace(fluxSystemNS).
		Patch(context.Background(), "checkout", types.MergePatchType, patch, metav1.PatchOptions{FieldManager: "flux"})
	require.NoError(t, err)
	_, err = flux.Suspend(context.Background(), checkoutKey)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"flux", SuspendFieldManager}, suspendManagers(sim.Kustomization(checkoutKey)),
		"shared, as the API server shares it")

	keeper, err := flux.Release(context.Background(), checkoutKey)
	require.NoError(t, err)
	assert.Empty(t, keeper)
	ks := sim.Kustomization(checkoutKey)
	assert.False(t, suspended(ks), "resumed, as the Released Event says")
	assert.Empty(t, suspendManagers(ks))

	applied := fluxsim.OwnSuspend(t, kustomizationObj(fluxSystemNS, "checkout", gitRepoSourceRef("platform")), true,
		"kustomize-controller", metav1.ManagedFieldsOperationApply)
	sim = fluxsim.New(t, newSourceClient(applied))
	flux = simFlux(sim)
	_, err = flux.Suspend(context.Background(), checkoutKey)
	require.NoError(t, err)
	keeper, err = flux.Release(context.Background(), checkoutKey)
	require.NoError(t, err)
	assert.Equal(t, "kustomize-controller", keeper, "its manifest sets spec.suspend")
	assert.True(t, suspended(sim.Kustomization(checkoutKey)))
}

// suspendManagers lists the field managers owning a Kustomization's spec.suspend.
func suspendManagers(ks *unstructured.Unstructured) []string {
	var managers []string
	for _, entry := range ks.GetManagedFields() {
		if ownsField(entry, "f:spec", "f:suspend") {
			managers = append(managers, entry.Manager)
		}
	}
	return managers
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

// TestFluxProgress_AnExternalResumeIsJudgedAsItsReconcilerJudgesIt is review 6: the trigger's
// reconciler records an external resume when it notices it, which can be after Flux applied it, and
// completes it on the Kustomization's last apply. A worker that recovered the record judged it on a
// reconcile run since the record's requestedAt, found none, and its Failed won over the
// reconciler's Succeeded. Now both read the last apply.
func TestFluxProgress_AnExternalResumeIsJudgedAsItsReconcilerJudgesIt(t *testing.T) {
	noticed := stampTime.Truncate(time.Second)
	appliedAt := noticed.Add(-40 * time.Second).UTC().Format(time.RFC3339)
	ks := kustomizationObj(fluxSystemNS, "checkout", gitRepoSourceRef("platform"))
	ks = fluxsim.OwnSuspend(t, ks, true, SuspendFieldManager, metav1.ManagedFieldsOperationApply)
	ks.SetAnnotations(map[string]string{fluxmeta.ReconcileRequestAnnotation: "F1"})
	_ = unstructured.SetNestedField(ks.Object, "F1", "status", "lastHandledReconcileAt")
	_ = unstructured.SetNestedField(ks.Object, "main@sha1:"+testCommit, "status", "lastAppliedRevision")
	_ = unstructured.SetNestedSlice(ks.Object, []any{map[string]any{
		"type": "Ready", "status": "True", "reason": fluxmeta.ReconciliationSucceededReason, "message": "Applied",
		"lastTransitionTime": appliedAt,
	}}, "status", "conditions")
	client := newSourceClient(ks, gitRepositoryObj(fluxSystemNS, "platform", testRepoURL, branchRef(testBranch),
		"main@sha1:"+testCommit))

	recovered := &Started{Outcome: OutcomeResumed, FenceToken: "F1", Recovered: true, RequestedAt: noticed}
	got, err := NewFlux(client).Progress(context.Background(), checkoutKey, recovered)

	require.NoError(t, err)
	assert.Equal(t, applyVerdict(ks, "").Outcome, got.Outcome, "the reconciler's verdict")
	assert.Equal(t, git.ApplySucceeded, got.Outcome)
}
