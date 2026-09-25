// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// The two repositories a repoint moves between: a recreated GitProvider mints a new UID, and here
// it names a different URL as well.
var (
	firstRepo  = git.RepoIdentity{ProviderUID: "uid-1", URL: "https://example.invalid/first.git"}
	secondRepo = git.RepoIdentity{ProviderUID: "uid-2", URL: "https://example.invalid/second.git"}
)

// publishPersisted is one reconcile's worth of publication: compute the status change, then settle
// the ledger as commit() does once the write has landed. Tests that care about a write which did
// NOT land call publishRemote directly and never settle.
func publishPersisted(
	r *GitTargetReconciler,
	target *configbutleraiv1alpha3.GitTarget,
	observed git.RemoteObservation,
	seen bool,
	repo git.RepoIdentity,
	now time.Time,
) {
	// The ledger dates itself from the reconciler's clock at PERSISTENCE, so a test that drives
	// synthetic time has to hold that clock still at the same instant it is evaluating.
	previous := r.clock
	r.clock = func() time.Time { return now }
	defer func() { r.clock = previous }()

	st := &reconcileStatus{}
	r.publishRemote(st, target, observed, seen, repo, now)
	st.runPersisted()
}

func remoteTestTarget() *configbutleraiv1alpha3.GitTarget {
	return &configbutleraiv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop", Generation: 4},
	}
}

func observation(revision string, at time.Time, by git.ObservationSource, repo git.RepoIdentity) git.RemoteObservation {
	return git.RemoteObservation{Revision: revision, At: at, By: by, Repo: repo}
}

// TestPublishRemote_AbsentUntilSomethingLooks keeps the two "no revision" cases apart. Nothing has
// looked, so there is no stanza at all — which is not the same as a look that found no branch.
func TestPublishRemote_AbsentUntilSomethingLooks(t *testing.T) {
	r := &GitTargetReconciler{}
	target := remoteTestTarget()

	publishPersisted(r, target, git.RemoteObservation{}, false, git.RepoIdentity{}, time.Now())

	assert.Nil(t, target.Status.Remote)
}

// TestPublishRemote_AnObservationWithNoRevisionIsStillPublished is the other half: the branch is
// not on the remote, and saying so is the answer rather than the absence of one.
func TestPublishRemote_AnObservationWithNoRevisionIsStillPublished(t *testing.T) {
	r := &GitTargetReconciler{}
	target := remoteTestTarget()
	at := time.Now()

	publishPersisted(
		r,
		target,
		observation("", at, git.ObservedByFetch, git.RepoIdentity{}),
		true,
		git.RepoIdentity{},
		at,
	)

	require.NotNil(t, target.Status.Remote)
	assert.Empty(t, target.Status.Remote.Revision)
	assert.Equal(t, "Fetch", target.Status.Remote.VerifiedBy)
	assert.Equal(t, at.Unix(), target.Status.Remote.LastVerifiedAt.Unix())
}

// TestPublishRemote_TheFloorIsMeasuredFromTheLastWrite is the rate limit, and the distinction is
// the whole point of keeping a ledger.
//
// Measuring against the PUBLISHED OBSERVATION's timestamp does not limit anything: an observation
// made at 12:00:02 can be written at 12:01:00, and a revision proved at 12:01:02 then looks a full
// minute newer than what is published and is written two seconds after it.
func TestPublishRemote_TheFloorIsMeasuredFromTheLastWrite(t *testing.T) {
	r := &GitTargetReconciler{}
	target := remoteTestTarget()
	start := time.Now()

	// Proved at +2s, written at +60s: a reconcile that happened to be late.
	publishPersisted(r, target, observation("aaaa", start.Add(2*time.Second), git.ObservedByPush, git.RepoIdentity{}),
		true, git.RepoIdentity{}, start.Add(time.Minute))
	require.Equal(t, "aaaa", target.Status.Remote.Revision)

	// Proved at +62s: newer than the published observation by a minute, two seconds after the write.
	publishPersisted(r, target, observation("bbbb", start.Add(62*time.Second), git.ObservedByPush, git.RepoIdentity{}),
		true, git.RepoIdentity{}, start.Add(62*time.Second))

	assert.Equal(t, "aaaa", target.Status.Remote.Revision,
		"two status writes two seconds apart is exactly what the floor exists to prevent")

	// A full interval after the WRITE, it publishes.
	publishPersisted(r, target, observation("bbbb", start.Add(62*time.Second), git.ObservedByPush, git.RepoIdentity{}),
		true, git.RepoIdentity{}, start.Add(2*time.Minute))
	assert.Equal(t, "bbbb", target.Status.Remote.Revision)
}

// TestPublishRemote_TheFirstObservationIsImmediate. The floor is on the RATE of writes, and the
// first one has no rate: holding it back would leave a target that has just published showing no
// branch at all, which reads as a broken data plane rather than as a sampled field.
func TestPublishRemote_TheFirstObservationIsImmediate(t *testing.T) {
	r := &GitTargetReconciler{}
	target := remoteTestTarget()
	at := time.Now()

	publishPersisted(r,
		target,
		observation("aaaa", at, git.ObservedByPush, git.RepoIdentity{}),
		true,
		git.RepoIdentity{},
		at,
	)

	require.NotNil(t, target.Status.Remote)
	assert.Equal(t, "aaaa", target.Status.Remote.Revision)
}

// TestPublishRemote_AConvergedTargetWritesNothing is the trap sameLayout was written to avoid: a
// target whose branch has not moved must not write status once per tick merely to advance a clock,
// however long it has been since the last one.
func TestPublishRemote_AConvergedTargetWritesNothing(t *testing.T) {
	r := &GitTargetReconciler{}
	target := remoteTestTarget()
	at := time.Now()
	publishPersisted(r,
		target,
		observation("aaaa", at, git.ObservedByFetch, git.RepoIdentity{}),
		true,
		git.RepoIdentity{},
		at,
	)
	published := target.Status.Remote

	publishPersisted(r, target, observation("aaaa", at, git.ObservedByFetch, git.RepoIdentity{}),
		true, git.RepoIdentity{}, at.Add(time.Hour))

	assert.Same(t, published, target.Status.Remote)
}

// TestPublishRemote_AReProvedRevisionRefreshesTheClock. The same revision proved again later IS
// news — that is what makes lastVerifiedAt a freshness field — and like every other kind of news
// it waits for the floor.
func TestPublishRemote_AReProvedRevisionRefreshesTheClock(t *testing.T) {
	r := &GitTargetReconciler{}
	target := remoteTestTarget()
	at := time.Now()
	publishPersisted(r,
		target,
		observation("aaaa", at, git.ObservedByFetch, git.RepoIdentity{}),
		true,
		git.RepoIdentity{},
		at,
	)

	publishPersisted(r, target, observation("aaaa", at.Add(30*time.Second), git.ObservedByFetch, git.RepoIdentity{}),
		true, git.RepoIdentity{}, at.Add(30*time.Second))
	assert.Equal(t, at.Unix(), target.Status.Remote.LastVerifiedAt.Unix(), "inside the floor")

	publishPersisted(r, target, observation("aaaa", at.Add(90*time.Second), git.ObservedByFetch, git.RepoIdentity{}),
		true, git.RepoIdentity{}, at.Add(90*time.Second))
	assert.Equal(t, at.Add(90*time.Second).Unix(), target.Status.Remote.LastVerifiedAt.Unix(),
		"an operator reading 'verified 40 minutes ago' on a target being refreshed would be "+
			"reading a broken refresher")
}

// TestRemoteStatusIsNews_TheSameAnswerProvedASecondWayIsNot. A revision that changes hands from a
// Push to a Fetch is the same answer proved again; the watch plane has always ignored it for that
// reason, and this layer now agrees.
func TestRemoteStatusIsNews_TheSameAnswerProvedASecondWayIsNot(t *testing.T) {
	at := metav1.NewTime(time.Now())
	published := &configbutleraiv1alpha3.GitTargetRemoteStatus{
		Revision: "aaaa", LastVerifiedAt: &at, VerifiedBy: "Push",
	}

	assert.False(t, remoteStatusIsNews(published, &configbutleraiv1alpha3.GitTargetRemoteStatus{
		Revision: "aaaa", LastVerifiedAt: &at, VerifiedBy: "Fetch",
	}))
}

// TestRemoteStatusIsNews_AMissingTimestampIsAlwaysNews. Neither side can be compared without one,
// and the safe direction is to write: a stanza with no clock is the one an operator cannot read an
// age off at all.
func TestRemoteStatusIsNews_AMissingTimestampIsAlwaysNews(t *testing.T) {
	at := metav1.NewTime(time.Now())
	withClock := &configbutleraiv1alpha3.GitTargetRemoteStatus{
		Revision: "aaaa", LastVerifiedAt: &at, VerifiedBy: "Fetch",
	}
	without := &configbutleraiv1alpha3.GitTargetRemoteStatus{Revision: "aaaa", VerifiedBy: "Fetch"}

	assert.True(t, remoteStatusIsNews(without, withClock))
	assert.True(t, remoteStatusIsNews(withClock, without))
}

// TestPublishRemote_ARevisionFromAnotherRepositoryIsRemoved. The published revision names a commit
// in a repository this GitTarget no longer points at, and there is nothing to replace it with
// until a look at the new one succeeds. If that repository is unreachable there never will be, so
// the stanza goes rather than going stale.
func TestPublishRemote_ARevisionFromAnotherRepositoryIsRemoved(t *testing.T) {
	r := &GitTargetReconciler{}
	target := remoteTestTarget()
	at := time.Now()
	publishPersisted(r, target, observation("aaaa", at, git.ObservedByPush, firstRepo), true, firstRepo, at)
	require.NotNil(t, target.Status.Remote)

	publishPersisted(r, target, observation("aaaa", at, git.ObservedByPush, firstRepo), true, secondRepo, at)

	assert.Nil(t, target.Status.Remote,
		"a revision from a repository this target no longer uses must not be left standing")
}

// TestPublishRemote_ASiblingsObservationCannotHideAStaleRepository is the case a comparison
// against the shared observation alone gets wrong.
//
// One worker serves every GitTarget on a branch, so a sibling can already have proved the
// REPLACEMENT repository. The shared observation then agrees with the GitProvider while this
// target is still publishing a revision from the repository it has left, and the floor would hold
// the correction back on top of that. Invalidation follows what THIS target published.
func TestPublishRemote_ASiblingsObservationCannotHideAStaleRepository(t *testing.T) {
	r := &GitTargetReconciler{}
	target := remoteTestTarget()
	at := time.Now()
	publishPersisted(r, target, observation("aaaa", at, git.ObservedByPush, firstRepo), true, firstRepo, at)
	require.Equal(t, "aaaa", target.Status.Remote.Revision)

	// A sibling on the same branch has just proved the new repository; one second later, this
	// target reconciles.
	publishPersisted(r, target, observation("bbbb", at.Add(time.Second), git.ObservedByFetch, secondRepo),
		true, secondRepo, at.Add(time.Second))

	require.NotNil(t, target.Status.Remote)
	assert.Equal(t, "bbbb", target.Status.Remote.Revision,
		"the repository changed under this target, so the correction does not wait for the floor")
}

// TestPublishRemote_AnUnknownIdentityProvesNothing. The comparison needs both halves. A
// GitProvider that could not be read names no repository, and a worker with no identity of its own
// — the CLI, and tests that never reach a remote — records none. Treating either as a mismatch
// would delete a perfectly good revision every time a provider read blipped.
func TestPublishRemote_AnUnknownIdentityProvesNothing(t *testing.T) {
	for _, tc := range []struct {
		name     string
		observed git.RepoIdentity
		current  git.RepoIdentity
	}{
		{"the provider could not be read", firstRepo, git.RepoIdentity{}},
		{"the worker has no identity", git.RepoIdentity{}, firstRepo},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &GitTargetReconciler{}
			target := remoteTestTarget()
			at := time.Now()

			publishPersisted(r, target, observation("aaaa", at, git.ObservedByPush, tc.observed), true, tc.current, at)

			require.NotNil(t, target.Status.Remote)
			assert.Equal(t, "aaaa", target.Status.Remote.Revision)
		})
	}
}

// TestPublishRemote_TheNewRepositoryRepopulatesTheStanza is the other side of the removal: the
// first look at the repository the target points at NOW publishes again, with no latch to clear
// and no floor to wait for, because the ledger was dropped with the revision it described.
func TestPublishRemote_TheNewRepositoryRepopulatesTheStanza(t *testing.T) {
	r := &GitTargetReconciler{}
	target := remoteTestTarget()
	at := time.Now()
	publishPersisted(r, target, observation("aaaa", at, git.ObservedByPush, firstRepo), true, secondRepo, at)
	require.Nil(t, target.Status.Remote)

	publishPersisted(r, target, observation("bbbb", at.Add(time.Second), git.ObservedByFetch, secondRepo),
		true, secondRepo, at.Add(time.Second))

	require.NotNil(t, target.Status.Remote)
	assert.Equal(t, "bbbb", target.Status.Remote.Revision)
}

// TestPublishRemote_ARestartPublishesAgainstTheObservationAlone. The ledger is in memory, so after
// a restart the only evidence about what is published is the observation itself, which names the
// repository it was proved against.
func TestPublishRemote_ARestartPublishesAgainstTheObservationAlone(t *testing.T) {
	target := remoteTestTarget()
	at := metav1.NewTime(time.Now())
	target.Status.Remote = &configbutleraiv1alpha3.GitTargetRemoteStatus{
		Revision: "aaaa", LastVerifiedAt: &at, VerifiedBy: "Push",
	}
	fresh := &GitTargetReconciler{}

	publishPersisted(
		fresh,
		target,
		observation("aaaa", at.Time, git.ObservedByPush, firstRepo),
		true,
		secondRepo,
		at.Time,
	)

	assert.Nil(t, target.Status.Remote, "the observation is about a repository this target has left")
}

// TestObserveRemote_ReadsTheBranchItsGitTargetWrites. The observation is shared by every GitTarget
// on the branch, so it is looked up by (provider namespace, provider, branch) and not by target.
func TestObserveRemote_ReadsTheBranchItsGitTargetWrites(t *testing.T) {
	target := remoteTestTarget()
	target.Spec.GitProviderRef.Name = "repo1"
	target.Spec.Branch = "main"

	_, seen := (&GitTargetReconciler{}).observeRemote(target, "shop")
	assert.False(t, seen, "a reconcile with no worker manager has nothing to read")

	workers := git.NewWorkerManager(nil, logr.Discard(), git.BranchWorkerLimits{}, types.SensitiveResourcePolicy{})
	_, seen = (&GitTargetReconciler{WorkerManager: workers}).observeRemote(target, "shop")
	assert.False(t, seen, "and neither has one whose branch nothing has looked at")
}

// TestRequestRemoteRefresh_DoesNothingWithoutADataPlane covers the two ways the trigger is simply
// not armed: the refresher turned off, and a reconcile running without a worker manager. Neither
// may reach for a worker, because the reconcile runs on every tick of every target.
func TestRequestRemoteRefresh_DoesNothingWithoutADataPlane(t *testing.T) {
	target := remoteTestTarget()

	assert.NotPanics(t, func() {
		(&GitTargetReconciler{GitRefreshInterval: 0}).requestRemoteRefresh(target, "default")
	}, "a zero interval turns the refresher off")

	assert.NotPanics(t, func() {
		(&GitTargetReconciler{GitRefreshInterval: time.Minute}).requestRemoteRefresh(target, "default")
	}, "and a reconcile with no worker manager has nothing to ask")

	workers := git.NewWorkerManager(nil, logr.Discard(), git.BranchWorkerLimits{}, types.SensitiveResourcePolicy{})
	assert.NotPanics(t, func() {
		(&GitTargetReconciler{
			GitRefreshInterval: time.Minute, WorkerManager: workers,
		}).requestRemoteRefresh(target, "default")
	}, "and a target whose branch has no worker yet has nothing to refresh either")
}

// TestPublishRemote_TheRemovalReachesTheAPIAsADelete. Clearing a Go pointer is only half of
// removing a field: the status write is a JSON merge patch computed against the object as read, so
// the fix works only if a field that was there and is now nil is emitted as an explicit null.
func TestPublishRemote_TheRemovalReachesTheAPIAsADelete(t *testing.T) {
	r := &GitTargetReconciler{}
	target := remoteTestTarget()
	target.Status.Conditions = []metav1.Condition{{
		Type: "Ready", Status: metav1.ConditionTrue, Reason: "Ready",
		LastTransitionTime: metav1.NewTime(time.Now()), ObservedGeneration: 4,
	}}
	at := time.Now()
	publishPersisted(r, target, observation("aaaa", at, git.ObservedByPush, firstRepo), true, firstRepo, at)
	before := target.DeepCopy()

	publishPersisted(r, target, observation("aaaa", at, git.ObservedByPush, firstRepo), true, secondRepo, at)

	data, err := client.MergeFrom(before).Data(target)
	require.NoError(t, err)
	assert.JSONEq(t, `{"status":{"remote":null}}`, string(data))
}

// TestPublishRemote_ADeletedTargetReleasesItsLedgerEntry. The ledger is the reconciler's own
// memory, keyed by namespace/name, so an entry left behind would hold a rate limit against a name
// that may be recreated tomorrow with nothing published at all.
func TestPublishRemote_ADeletedTargetReleasesItsLedgerEntry(t *testing.T) {
	r := &GitTargetReconciler{}
	target := remoteTestTarget()
	at := time.Now()
	publishPersisted(r, target, observation("aaaa", at, git.ObservedByPush, firstRepo), true, firstRepo, at)
	ref := types.NewResourceReference(target.Name, target.Namespace)
	_, had := r.remotePublications.last(ref, target.UID)
	require.True(t, had)

	r.remotePublications.forget(ref)

	_, had = r.remotePublications.last(ref, target.UID)
	assert.False(t, had)

	// A successor under the same name publishes immediately rather than waiting out the floor.
	successor := remoteTestTarget()
	publishPersisted(r, successor, observation("bbbb", at.Add(time.Second), git.ObservedByFetch, firstRepo),
		true, firstRepo, at.Add(time.Second))
	require.NotNil(t, successor.Status.Remote)
	assert.Equal(t, "bbbb", successor.Status.Remote.Revision)
}

// TestRequeueForRemoteAnswer_ShortensOnlyWhenSomethingWasAsked. The refresh is enqueued during the
// reconcile, so its answer lands after the status write: a target that asked has to come back for
// it, or an idle one spends a connection every interval and publishes what it learned a whole tick
// late. A target that asked nothing keeps its own cadence.
func TestRequeueForRemoteAnswer_ShortensOnlyWhenSomethingWasAsked(t *testing.T) {
	assert.Equal(t, RemotePublicationInterval, requeueForRemoteAnswer(RequeueSteadyInterval, true))
	assert.Equal(t, RequeueSteadyInterval, requeueForRemoteAnswer(RequeueSteadyInterval, false))
	assert.Equal(t, time.Second, requeueForRemoteAnswer(time.Second, true),
		"a target already on a faster loop is not slowed down to the publication interval")
}

// remoteLedgerFixture drives publishRemote through the REAL status session, so what advances the
// ledger is a patch the API server accepted rather than a callback a test chose to run.
type remoteLedgerFixture struct {
	r        *GitTargetReconciler
	client   client.Client
	conflict bool
	now      time.Time
}

func newRemoteLedgerFixture(t *testing.T, target *configbutleraiv1alpha3.GitTarget) *remoteLedgerFixture {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, configbutleraiv1alpha3.AddToScheme(scheme))

	f := &remoteLedgerFixture{now: time.Now()}
	f.client = interceptor.NewClient(
		fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(target).WithStatusSubresource(target).Build(),
		interceptor.Funcs{
			SubResourcePatch: func(
				ctx context.Context, c client.Client, subResource string,
				obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption,
			) error {
				if f.conflict {
					// What losing the optimistic lock looks like: the object moved under this
					// reconcile, so the patch is refused.
					return apierrors.NewConflict(
						schema.GroupResource{Group: "configbutler.ai", Resource: "gittargets"},
						obj.GetName(), errors.New("the object has been modified"))
				}
				return c.SubResource(subResource).Patch(ctx, obj, patch, opts...)
			},
		})
	f.r = &GitTargetReconciler{Client: f.client, clock: func() time.Time { return f.now }}
	return f
}

// publish runs one reconcile's worth of publication against the real status session and reports
// whether the write landed.
func (f *remoteLedgerFixture) publish(
	t *testing.T,
	target *configbutleraiv1alpha3.GitTarget,
	observed git.RemoteObservation,
	repo git.RepoIdentity,
) bool {
	t.Helper()
	st := beginStatus(f.client, nil, target)
	f.r.publishRemote(st, target, observed, true, repo, f.now)
	require.NoError(t, st.commit(context.Background()), "a conflict is recorded, not returned")
	return !st.writeLost()
}

// TestPublishRemote_ARejectedPatchDoesNotAdvanceTheLedger. The conflict is the case the callback
// exists for, and it is invisible to a caller checking the error: commit() records the loss and
// returns nil. What must not happen is the ledger recording a publication the API server refused,
// because the cooldown would then hold off the retry for a revision nobody can read.
func TestPublishRemote_ARejectedPatchDoesNotAdvanceTheLedger(t *testing.T) {
	target := remoteTestTarget()
	target.UID = "uid-target"
	f := newRemoteLedgerFixture(t, target)
	ref := types.NewResourceReference(target.Name, target.Namespace)

	f.conflict = true
	require.False(t, f.publish(t, target, observation("aaaa", f.now, git.ObservedByPush, firstRepo), firstRepo),
		"precondition: the patch was refused")
	_, had := f.r.remotePublications.last(ref, target.UID)
	assert.False(t, had, "a publication the API server refused is not a publication")

	// The retry, two seconds later, is not held off by a cooldown it never earned.
	f.conflict = false
	f.now = f.now.Add(2 * time.Second)
	fresh := &configbutleraiv1alpha3.GitTarget{}
	require.NoError(t, f.client.Get(context.Background(),
		client.ObjectKeyFromObject(target), fresh))
	require.Nil(t, fresh.Status.Remote, "nothing was published, so there is nothing to read back")

	require.True(t, f.publish(t, fresh, observation("bbbb", f.now, git.ObservedByPush, firstRepo), firstRepo))
	published, had := f.r.remotePublications.last(ref, target.UID)
	require.True(t, had)
	assert.Equal(t, f.now, published.at, "and the cooldown starts when the write landed")
	assert.Equal(t, "bbbb", fresh.Status.Remote.Revision)
}

// TestPublishRemote_ARejectedWithdrawalKeepsTheLedgerEntry is the same rule for the correction.
// Forgetting the entry before the removal persists would leave the old repository's revision in
// etcd with nothing left that knows it is wrong: the next reconcile compares against a ledger that
// says this target published nothing.
func TestPublishRemote_ARejectedWithdrawalKeepsTheLedgerEntry(t *testing.T) {
	target := remoteTestTarget()
	target.UID = "uid-target"
	f := newRemoteLedgerFixture(t, target)
	ref := types.NewResourceReference(target.Name, target.Namespace)

	require.True(t, f.publish(t, target, observation("aaaa", f.now, git.ObservedByPush, firstRepo), firstRepo))
	require.NotNil(t, target.Status.Remote)

	// The GitProvider is recreated against another repository, and the withdrawal loses the race.
	f.conflict = true
	f.now = f.now.Add(time.Second)
	require.False(t, f.publish(t, target, observation("aaaa", f.now, git.ObservedByPush, firstRepo), secondRepo))

	entry, had := f.r.remotePublications.last(ref, target.UID)
	require.True(t, had, "the entry is what makes the next reconcile withdraw again")
	assert.Equal(t, firstRepo, entry.repo)

	// And the next one does, against the object as it actually stands.
	f.conflict = false
	f.now = f.now.Add(time.Second)
	stale := &configbutleraiv1alpha3.GitTarget{}
	require.NoError(t, f.client.Get(context.Background(), client.ObjectKeyFromObject(target), stale))
	require.NotNil(t, stale.Status.Remote, "the old repository's revision is still published")

	require.True(t, f.publish(t, stale, observation("aaaa", f.now, git.ObservedByPush, firstRepo), secondRepo))
	assert.Nil(t, stale.Status.Remote)
	_, had = f.r.remotePublications.last(ref, target.UID)
	assert.False(t, had, "and the ledger forgets it only once the removal is persisted")
}

// TestPublishRemote_TheCooldownStartsWhenTheWriteLands. `now` is taken before the gates, the
// worker wiring and the patch, so on a slow reconcile a cooldown dated from it is already part
// spent when the write lands — and a reconcile queued behind it publishes again straight away.
func TestPublishRemote_TheCooldownStartsWhenTheWriteLands(t *testing.T) {
	r := &GitTargetReconciler{}
	target := remoteTestTarget()
	evaluated := time.Now()
	landed := evaluated.Add(90 * time.Second) // a slow pass: gates, wiring, then the patch

	r.clock = func() time.Time { return landed }
	st := &reconcileStatus{}
	r.publishRemote(st, target, observation("aaaa", evaluated, git.ObservedByPush, firstRepo),
		true, firstRepo, evaluated)
	st.runPersisted()
	require.NotNil(t, target.Status.Remote)

	// A reconcile that was queued behind it, one second after the write landed.
	r.clock = func() time.Time { return landed.Add(time.Second) }
	st = &reconcileStatus{}
	r.publishRemote(st, target, observation("bbbb", landed.Add(time.Second), git.ObservedByPush, firstRepo),
		true, firstRepo, landed.Add(time.Second))
	st.runPersisted()

	assert.Equal(t, "aaaa", target.Status.Remote.Revision,
		"the floor runs from the write, not from the moment the slow pass started")
}

// TestPublishRemote_AWriteThatDidNotLandDoesNotAdvanceTheLedger.
//
// The status patch carries an optimistic lock, and a conflict is RECORDED rather than returned, so
// a caller checking the error alone learns nothing. If the ledger advanced anyway, the object
// would still hold the winner's older answer while the ledger claimed the newer one was published
// — and the retry would be held off behind a cooldown for a revision nobody can read. The withdrawal
// has the same shape: forgetting the entry before the removal persists leaves the old repository's
// revision standing with nothing to take it back.
func TestPublishRemote_AWriteThatDidNotLandDoesNotAdvanceTheLedger(t *testing.T) {
	r := &GitTargetReconciler{}
	target := remoteTestTarget()
	at := time.Now()

	// The write is computed and lost: nothing settles.
	lost := &reconcileStatus{}
	r.publishRemote(lost, target, observation("aaaa", at, git.ObservedByPush, firstRepo), true, firstRepo, at)
	require.NotNil(t, target.Status.Remote, "precondition: the status change was computed")

	_, had := r.remotePublications.last(types.NewResourceReference(target.Name, target.Namespace), target.UID)
	assert.False(t, had, "a publication nobody can read is not a publication")

	// The retry, two seconds later, must not be held off by a cooldown it never earned.
	retried := remoteTestTarget()
	publishPersisted(r, retried, observation("bbbb", at.Add(2*time.Second), git.ObservedByPush, firstRepo),
		true, firstRepo, at.Add(2*time.Second))
	require.NotNil(t, retried.Status.Remote)
	assert.Equal(t, "bbbb", retried.Status.Remote.Revision)
}

// TestPublishRemote_ARecreatedTargetDoesNotInheritTheCooldown.
//
// The ledger is keyed by namespace/name, and cleanup needs a reconcile that observes NotFound. A
// delete followed quickly by a recreate can be reconciled from the successor directly, so the
// entry survives — and the successor, with nothing published at all, would have its FIRST
// observation suppressed by its predecessor's cooldown. The entry carries the UID it was earned
// under, which is the one thing that tells the two objects apart.
func TestPublishRemote_ARecreatedTargetDoesNotInheritTheCooldown(t *testing.T) {
	r := &GitTargetReconciler{}
	at := time.Now()

	predecessor := remoteTestTarget()
	predecessor.UID = "uid-first"
	publishPersisted(r, predecessor, observation("aaaa", at, git.ObservedByPush, firstRepo), true, firstRepo, at)
	require.NotNil(t, predecessor.Status.Remote)

	// Deleted and recreated under the same name, a second later. No NotFound reconcile ran.
	successor := remoteTestTarget()
	successor.UID = "uid-second"
	publishPersisted(r, successor, observation("bbbb", at.Add(time.Second), git.ObservedByFetch, firstRepo),
		true, firstRepo, at.Add(time.Second))

	require.NotNil(t, successor.Status.Remote,
		"a target with nothing published has no rate to be limited to")
	assert.Equal(t, "bbbb", successor.Status.Remote.Revision)
}
