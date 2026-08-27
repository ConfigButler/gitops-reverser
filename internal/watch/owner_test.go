// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"

	v1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// ownerTestManager is a Manager the owner loop can drive with no cluster behind it: discovery is
// stubbed so a pass never reaches the network, and EventRouter is nil so ensureGitTargetWatches
// succeeds without opening anything. These tests are about WHEN and HOW OFTEN a pass runs, not
// about what it installs.
func ownerTestManager() *Manager {
	return &Manager{Log: logr.Discard(), discoveryClient: commonTestDiscoveryClient()}
}

// settle moves a dirty target's timestamps into the past so its silence window has elapsed,
// instead of making the test wait two real seconds for it.
func (m *Manager) settle(ref types.ResourceReference) {
	t := m.triggers()
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := t.dirty[ref.Key()]
	if entry == nil {
		return
	}
	past := time.Now().Add(-2 * settleWindow)
	entry.firstDirty, entry.lastTrigger = past, past
}

// runTurns drives the owner until it has nothing ready left, and returns how many passes ran.
// ownerTurn returns a zero wait exactly when it ran one.
func (m *Manager) runTurns(t *testing.T, limit int) int {
	t.Helper()
	passes := 0
	for range limit {
		if wait := m.ownerTurn(context.Background(), logr.Discard()); wait != 0 {
			return passes
		}
		passes++
	}
	t.Fatalf("the owner ran %d passes without going idle", limit)
	return passes
}

// A discovery client must never run without a deadline, and a WATCH must never run with one. The
// local cluster used to be exempted from the first half on the reasoning that it is never slow,
// which left the legacy non-context ServerGroupsAndResources() able to hang forever.
func TestDiscoveryRESTConfig_BoundsDiscoveryWithoutBoundingWatches(t *testing.T) {
	cfg := &rest.Config{Host: "https://kubernetes.default.svc"}

	disco := discoveryRESTConfig(cfg)

	assert.Equal(t, sourceClusterDialTimeout, disco.Timeout,
		"the discovery config carries a request timeout, local cluster included")
	assert.Zero(t, cfg.Timeout,
		"the config watches are built from is untouched; a deadline there would kill every watch")
	assert.NotSame(t, cfg, disco, "the timeout is set on a COPY, which is what makes it safe")
}

// One `kubectl apply` of a GitTarget and four WatchRules is ONE piece of configuration. The API
// server delivers it as five watch events within a few hundred milliseconds; the settle window
// turns those into one pass.
func TestOwner_OneApplyOfATargetAndItsRulesIsOnePass(t *testing.T) {
	m := ownerTestManager()
	ref := types.NewResourceReference("target", "team-a").WithUID("uid-1")

	m.DeclareForGitTarget(ref, configPlaneClusterID, "", v1alpha3.PruneOnEvent)
	for range 4 {
		m.TriggerRuleChange(types.NewResourceReference("target", "team-a"))
	}

	require.Len(t, m.triggers().dirty, 1, "five triggers coalesce onto one dirty target")
	assert.Equal(t, uint64(4), m.triggers().coalesced,
		"four of the five landed on a target that was already dirty")

	assert.Zero(t, m.runTurns(t, 8), "nothing runs while the target is still being edited")

	m.settle(ref)
	assert.Equal(t, 1, m.runTurns(t, 8), "the settled burst produces exactly one pass")
	assert.Empty(t, m.triggers().dirty, "and the target is clean afterwards")
}

// A rule-side trigger carries no UID — the watch table is rule-derived and has none — so the owner
// resolves it against the declare record rather than planning against a half-identified target.
func TestOwner_ARuleTriggerForAnUndeclaredTargetPlansNothing(t *testing.T) {
	m := ownerTestManager()

	m.TriggerRuleChange(types.NewResourceReference("never-declared", "team-a"))
	m.settle(types.NewResourceReference("never-declared", "team-a"))

	assert.Equal(t, 1, m.runTurns(t, 4), "the owner takes the trigger off the queue")
	assert.Empty(t, m.triggers().dirty, "and drops it rather than inventing an identity for it")
	assert.Empty(t, m.watchPlane().passes, "no pass outcome is recorded for a target that never declared")
}

// An edit to one GitTarget must produce no pass for any other. This is the property the deleted
// refreshRunningTargetWatches did not have: it walked every resident table on every rule change.
func TestOwner_EditingOneTargetPlansNoOther(t *testing.T) {
	m := ownerTestManager()
	edited := types.NewResourceReference("edited", "team-a").WithUID("uid-a")
	quiet := types.NewResourceReference("quiet", "team-b").WithUID("uid-b")

	m.DeclareForGitTarget(edited, configPlaneClusterID, "", v1alpha3.PruneOnEvent)
	m.DeclareForGitTarget(quiet, configPlaneClusterID, "", v1alpha3.PruneOnEvent)
	m.settle(edited)
	m.settle(quiet)
	require.Equal(t, 2, m.runTurns(t, 8), "both targets declare once")

	m.TriggerRuleChange(types.NewResourceReference("edited", "team-a"))

	require.Len(t, m.triggers().dirty, 1)
	_, quietDirty := m.triggers().dirty[quiet.Key()]
	assert.False(t, quietDirty, "the untouched GitTarget is not replanned because a sibling's rule moved")
}

// The dirty sequence is what makes a change arriving mid-pass impossible to lose. Clearing the
// dirty mark unconditionally at the end of a pass would drop it.
func TestOwner_ATriggerDuringAPassSchedulesAnother(t *testing.T) {
	m := ownerTestManager()
	ref := types.NewResourceReference("target", "team-a").WithUID("uid-1")
	m.DeclareForGitTarget(ref, configPlaneClusterID, "", v1alpha3.PruneOnEvent)
	m.settle(ref)

	// What the loop captures when the pass starts.
	inFlight := *m.triggers().dirty[ref.Key()]
	// ...and a rule edit that lands while it is running.
	m.TriggerRuleChange(types.NewResourceReference("target", "team-a"))

	m.clearDirty(&inFlight, nil)

	entry := m.triggers().dirty[ref.Key()]
	require.NotNil(t, entry, "the mid-pass change leaves the target dirty")
	assert.False(t, entry.readyAt().After(time.Now()),
		"and it runs immediately: the change it carries has already waited a window out")
}

// A pass that fails leaves the target dirty and backs off on a stated ladder, rather than being
// retried by whatever reconcile happens to come next.
func TestOwner_AFailedPassStaysDirtyAndBacksOff(t *testing.T) {
	m := ownerTestManager()
	ref := types.NewResourceReference("target", "team-a").WithUID("uid-1")
	m.DeclareForGitTarget(ref, configPlaneClusterID, "", v1alpha3.PruneOnEvent)
	m.settle(ref)
	entry := *m.triggers().dirty[ref.Key()]

	m.recordPassOutcome(ref, time.Now(), errors.New("discovery timed out"))
	m.clearDirty(&entry, errors.New("discovery timed out"))

	current := m.triggers().dirty[ref.Key()]
	require.NotNil(t, current, "a failed pass never clears the target")
	assert.Equal(t, 1, current.failures)
	assert.True(t, current.readyAt().After(time.Now().Add(time.Second)),
		"the retry waits out the first rung of the ladder")

	status := m.DeclareStatusForGitTarget(ref)
	assert.True(t, status.Pending, "a target whose passes keep failing must not read as idle")
	assert.Equal(t, 1, status.Failures)
	assert.Equal(t, "discovery timed out", status.LastError,
		"the failure is published where an operator looks, not only in a log")
}

func TestRetryDelay_WalksTheLadderAndCapsAtTheLastRung(t *testing.T) {
	assert.Equal(t, 2*time.Second, retryDelay(1))
	assert.Equal(t, 5*time.Second, retryDelay(2))
	assert.Equal(t, 10*time.Second, retryDelay(3))
	assert.Equal(t, 30*time.Second, retryDelay(4))
	assert.Equal(t, time.Minute, retryDelay(5))
	assert.Equal(t, time.Minute, retryDelay(50), "the ladder caps rather than growing forever")
}

// A rolling window that is reset forever never fires, so the hard cap has to win once the target
// has been dirty long enough.
func TestDirtyTarget_ReadyAtIsBoundedByTheMaxWait(t *testing.T) {
	now := time.Now()
	churning := &dirtyTarget{firstDirty: now.Add(-maxSettleWait), lastTrigger: now}

	assert.False(t, churning.readyAt().After(now),
		"a target under continuous change runs at the cap instead of starving")

	quiet := &dirtyTarget{firstDirty: now, lastTrigger: now}
	assert.Equal(t, now.Add(settleWindow), quiet.readyAt(),
		"an ordinary edit waits out the silence window")

	backedOff := &dirtyTarget{firstDirty: now.Add(-time.Hour), lastTrigger: now, retryAt: now.Add(time.Minute)}
	assert.Equal(t, now.Add(time.Minute), backedOff.readyAt(),
		"a backoff outranks both, or a failing target would spin")
}

// Delete-and-recreate under the SAME namespace and name is the sequence where a stale trigger
// resurrects state for an object that no longer exists. Testing delete alone does not cover it.
func TestOwner_DeleteAndRecreateUnderTheSameNameDoesNotResurrect(t *testing.T) {
	m := ownerTestManager()
	name, namespace := "target", "team-a"
	first := types.NewResourceReference(name, namespace).WithUID("uid-1")
	second := types.NewResourceReference(name, namespace).WithUID("uid-2")

	m.DeclareForGitTarget(first, "prod-eu-1", "", v1alpha3.PruneOnEvent)
	m.settle(first)
	require.Equal(t, 1, m.runTurns(t, 4))
	require.Equal(t, "uid-1", m.watchPlane().uids[first.Key()])

	// Delete, then recreate before the owner has drained the deletion.
	m.ForgetGitTargetDeclaration(first)
	assert.Empty(t, m.triggers().dirty,
		"the delete drops the pending trigger in the same step, or the next pass rebuilds what it tore down")
	m.DeclareForGitTarget(second, "prod-eu-1", "", v1alpha3.PruneOnEvent)
	m.settle(second)
	require.Equal(t, 1, m.runTurns(t, 4))

	// Only now does the owner see the queued teardown, which belongs to the predecessor.
	m.applyPendingTeardowns(logr.Discard())

	assert.Equal(t, "uid-2", m.watchPlane().uids[second.Key()],
		"a stale teardown must not take the successor's state with it")
	assert.True(t, m.DeclareStatusForGitTarget(second).Declared,
		"the recreated GitTarget is still declared")
}

// The one path that still fans out to everything is the periodic floor, and it says so.
func TestMarkInvalidatedTargets_DirtiesOnlyTheTargetsAPlanChangeTouched(t *testing.T) {
	m := ownerTestManager()
	moved := types.NewResourceReference("moved", "team-a").WithUID("uid-a")
	unaffected := types.NewResourceReference("unaffected", "team-b").WithUID("uid-b")
	m.DeclareForGitTarget(moved, configPlaneClusterID, "", v1alpha3.PruneOnEvent)
	m.DeclareForGitTarget(unaffected, configPlaneClusterID, "", v1alpha3.PruneOnEvent)
	m.settle(moved)
	m.settle(unaffected)
	require.Equal(t, 2, m.runTurns(t, 8))
	require.Empty(t, m.triggers().dirty)

	before := map[string]uint64{moved.Key(): 1, unaffected.Key(): 7}
	after := map[string]uint64{moved.Key(): 2, unaffected.Key(): 7}

	m.markInvalidatedTargets(before, after, false)

	require.Len(t, m.triggers().dirty, 1,
		"a CRD that changes no plan for a target is no reason to replan it")
	_, dirty := m.triggers().dirty[moved.Key()]
	assert.True(t, dirty)

	// The periodic floor is the exception, and the only one.
	m.markInvalidatedTargets(after, after, true)
	assert.Len(t, m.triggers().dirty, 2, "the periodic sweep marks everything, as the floor")
}

// The plan fingerprint is what the scoped invalidation compares, so it has to move on exactly the
// things a stream is built from and nothing else.
func TestWatchPlanFingerprint_MovesOnTheStreamSetAndNotOnTheTargetName(t *testing.T) {
	gitDest := types.NewResourceReference("target", "team-a")
	base := planTable(gitDest, "apps")

	assert.Equal(t, watchPlanFingerprint(base), watchPlanFingerprint(planTable(gitDest, "apps")),
		"an identical plan fingerprints identically")
	assert.NotEqual(t, watchPlanFingerprint(base), watchPlanFingerprint(planTable(gitDest, "apps", "ops")),
		"a new scope is a new stream, so the plan moved")

	renamed := planTable(types.NewResourceReference("other", "team-a"), "apps")
	assert.Equal(t, watchPlanFingerprint(base), watchPlanFingerprint(renamed),
		"the fingerprint describes the STREAM SET; it is compared per target, so the name is not in it")
}

// A timeout must never install an empty plan. A pass that could not gather is not a pass that
// found nothing, and an ungatherable cell must never present as an absent one.
func TestOwner_AFailedPassLeavesTheRunningPlanUntouched(t *testing.T) {
	m := ownerTestManager()
	// Discovery is broken, so this cluster's API surface can never be observed.
	m.discoveryClient = func() (apiResourceDiscovery, error) {
		return nil, errors.New("discovery is unavailable")
	}
	workerManager := git.NewWorkerManager(nil, logr.Discard(), 0, types.SensitiveResourcePolicy{})
	m.EventRouter = NewEventRouter(workerManager, m, nil, logr.Discard())
	ref := types.NewResourceReference("target", "team-a").WithUID("uid-1")

	// A stream that is already running, from a pass that DID gather.
	cancelled := installFakeWatch(m, ref)

	// This pass cannot gather: nothing has observed the cluster's API surface.
	err := m.applyTargetPlan(context.Background(), declareIntent{ref: ref, clusterID: configPlaneClusterID})

	require.Error(t, err, "a pass that cannot observe the surface fails rather than planning against nothing")
	assert.False(t, *cancelled, "the running stream is left alone; the failure installs nothing")
	assert.Len(t, m.targetWatchSet(ref).streams, 1, "and the plan it was built from still stands")
}

// A GitTarget reconcile is level-triggered: the controller re-declares the same values on every
// steady requeue and on every event it watches. Those must not each buy a pass, and — because
// "the declaration has landed" is a readiness input — must not keep a healthy target unsettled.
func TestOwner_ARedeclareThatChangesNothingIsANoOp(t *testing.T) {
	m := ownerTestManager()
	ref := types.NewResourceReference("target", "team-a").WithUID("uid-1")

	m.DeclareForGitTarget(ref, configPlaneClusterID, "route", v1alpha3.PruneOnEvent)
	m.settle(ref)
	require.Equal(t, 1, m.runTurns(t, 4))
	require.True(t, m.DeclareStatusForGitTarget(ref).Settled(), "the first declaration lands")

	m.DeclareForGitTarget(ref, configPlaneClusterID, "route", v1alpha3.PruneOnEvent)

	assert.Empty(t, m.triggers().dirty, "an identical re-declare owes the owner nothing")
	assert.True(t, m.DeclareStatusForGitTarget(ref).Settled(),
		"and leaves the target converged rather than perpetually pending")

	// A value that actually moved is a different matter: a widened prune policy is the one edge
	// that must force a fresh replay.
	m.DeclareForGitTarget(ref, configPlaneClusterID, "route", v1alpha3.PruneAlways)
	assert.Len(t, m.triggers().dirty, 1, "a changed declaration is a change")
}

// Being dirty is the steady state of a system that replans on every rule edit and sweeps every
// 30s. Reporting that as unsettled would flap Ready on a healthy mirror.
func TestDeclareStatus_DirtyIsNotPendingOnceAPassHasLanded(t *testing.T) {
	m := ownerTestManager()
	ref := types.NewResourceReference("target", "team-a").WithUID("uid-1")
	m.DeclareForGitTarget(ref, configPlaneClusterID, "", v1alpha3.PruneOnEvent)

	assert.True(t, m.DeclareStatusForGitTarget(ref).Pending,
		"a declaration whose pass has never run has not landed")

	m.settle(ref)
	require.Equal(t, 1, m.runTurns(t, 4))
	m.TriggerRuleChange(types.NewResourceReference("target", "team-a"))

	require.Len(t, m.triggers().dirty, 1, "precondition: the rule edit made it dirty")
	assert.False(t, m.DeclareStatusForGitTarget(ref).Pending,
		"a replan of a target that already mirrors is progress, not an unsettled declaration")
}

// Both production callers of ForgetGitTargetDeclaration react to a NotFound, so they carry no UID
// at all. A UID-less deletion that matched every incarnation would tear down the successor of a
// GitTarget deleted and recreated under the same namespace and name.
func TestOwner_AUIDLessDeleteTearsDownTheIncarnationItWasQueuedFor(t *testing.T) {
	m := ownerTestManager()
	name, namespace := "target", "team-a"
	first := types.NewResourceReference(name, namespace).WithUID("uid-1")
	second := types.NewResourceReference(name, namespace).WithUID("uid-2")

	m.DeclareForGitTarget(first, "prod-eu-1", "", v1alpha3.PruneOnEvent)
	m.settle(first)
	require.Equal(t, 1, m.runTurns(t, 4))

	// The controller observes the delete and cleans up by name only.
	m.ForgetGitTargetDeclaration(types.NewResourceReference(name, namespace))
	// The recreate lands before the owner has drained the deletion, and its pass runs first —
	// which is what moves the recorded UID to the successor.
	m.DeclareForGitTarget(second, "prod-eu-1", "", v1alpha3.PruneOnEvent)
	m.settle(second)
	require.Equal(t, 1, m.runTurns(t, 4))

	m.applyPendingTeardowns(logr.Discard())

	assert.Equal(t, "uid-2", m.watchPlane().uids[second.Key()],
		"a deletion queued for the predecessor must not take the successor's state with it")
	assert.True(t, m.DeclareStatusForGitTarget(second).Declared)
}

// A delete that names an incarnation the record has already replaced is stale on arrival: its
// object is gone and a successor has declared. It must not take the successor's pending pass.
func TestOwner_AStaleDeleteDoesNotDropTheSuccessorsPendingPass(t *testing.T) {
	m := ownerTestManager()
	name, namespace := "target", "team-a"
	first := types.NewResourceReference(name, namespace).WithUID("uid-1")
	second := types.NewResourceReference(name, namespace).WithUID("uid-2")

	m.DeclareForGitTarget(first, "prod-eu-1", "", v1alpha3.PruneOnEvent)
	m.settle(first)
	require.Equal(t, 1, m.runTurns(t, 4))

	// The successor declares, and is owed a pass...
	m.DeclareForGitTarget(second, "prod-eu-1", "", v1alpha3.PruneOnEvent)
	require.Len(t, m.triggers().dirty, 1, "precondition: the recreation is owed a pass")

	// ...and only then does the predecessor's delete arrive.
	m.ForgetGitTargetDeclaration(first)

	assert.Len(t, m.triggers().dirty, 1,
		"the stale delete leaves the successor's pending pass alone")
	assert.True(t, m.DeclareStatusForGitTarget(second).Declared,
		"and leaves its declaration standing")

	m.applyPendingTeardowns(logr.Discard())
	assert.True(t, m.DeclareStatusForGitTarget(second).Declared,
		"applying it must not tear the successor down either")
}

// Every network call the watch plane makes lives in the shared refresh, and the loop must not wait
// for it. A pass therefore never dials — not even for a cluster nothing has observed yet, where it
// would be most tempting.
func TestOwner_APassNeverDialsForAnUnobservedCluster(t *testing.T) {
	m := ownerTestManager()
	dials := make(chan struct{}, 8)
	m.discoveryClient = func() (apiResourceDiscovery, error) {
		dials <- struct{}{}
		// Slow enough that a synchronous dial on the loop would be unmistakable.
		time.Sleep(200 * time.Millisecond)
		return newCommonTestDiscovery(), nil
	}
	ref := types.NewResourceReference("target", "team-a").WithUID("uid-1")

	started := time.Now()
	err := m.applyTargetPlan(context.Background(), declareIntent{ref: ref, clusterID: configPlaneClusterID})

	require.NoError(t, err)
	assert.Less(t, time.Since(started), 200*time.Millisecond, "the pass returned without dialling")
	assert.Empty(t, dials, "it asked for a shared refresh instead of making the call itself")
	assert.True(t, m.triggers().sharedDue, "and that request is queued for the refresh to serve")

	// The refresh is what dials, and the caller does not wait for it either.
	m.refreshSharedSnapshotsIfDue(context.Background(), logr.Discard())
	assert.Eventually(t, func() bool { return len(dials) > 0 }, 2*time.Second, 10*time.Millisecond,
		"the shared refresh dials on its own goroutine")
}

// A pass runs under a deadline, and its context is cancelled the moment it returns. A stream
// parented to it therefore dies the instant the plan finishes being applied — the plan reads as
// applied while nothing is watching, readiness never leaves Replaying, and the next pass sees the
// cell as KEPT so it never restarts either. A stream's lifetime is the manager's.
func TestOwner_AStreamOutlivesThePassThatStartedIt(t *testing.T) {
	lifetime, stopManager := context.WithCancel(context.Background())
	defer stopManager()

	m := ownerTestManager()
	m.watchLifetime.Store(&lifetime)
	workerManager := git.NewWorkerManager(nil, logr.Discard(), 0, types.SensitiveResourcePolicy{})
	m.EventRouter = NewEventRouter(workerManager, m, nil, logr.Discard())
	gitDest := types.NewResourceReference("target", "team-a")

	var streamCtx context.Context
	passCtx, endPass := context.WithCancel(context.Background())
	started := m.startTargetWatchStreams(
		passCtx,
		m.targetWatchSet(gitDest),
		keysByCell([]targetWatchKey{{GVR: configmapsGVR, Namespace: "apps"}}),
		map[targetWatchKey]OperationSet{},
		map[targetWatchKey]string{},
		map[types.CellKey]uint64{},
		[]types.CellKey{types.CellKeyFor(configmapsGVR, "apps")},
	)
	require.Len(t, started, 1)
	streamCtx = started[0].ctx

	// The pass returns, which is what its deferred cancel does.
	endPass()

	require.NoError(t, streamCtx.Err(), "the stream must survive the pass that started it")

	// It is the MANAGER's lifetime that ends it — and, before that, the owner's own per-cell
	// cancel through the plan diff.
	stopManager()
	assert.Eventually(t, func() bool { return streamCtx.Err() != nil }, time.Second, 5*time.Millisecond,
		"and stop when the manager does")
}

// A target pass that fails carries its own retry, in its dirty entry. A shared refresh has no
// dirty entry, so a failed one has to ask for itself again — otherwise every target keeps planning
// against snapshots that may be stale until the 30s sweep, which is exactly the wrong latency for
// a cluster that has just become unreachable.
func TestOwner_AFailedSharedRefreshAsksForAnother(t *testing.T) {
	m := ownerTestManager()
	var fail atomic.Bool
	fail.Store(true)
	m.discoveryClient = func() (apiResourceDiscovery, error) {
		if fail.Load() {
			return nil, errors.New("the cluster is unreachable")
		}
		return newCommonTestDiscovery(), nil
	}

	m.signalSharedRefresh()
	m.refreshSharedSnapshotsIfDue(context.Background(), logr.Discard())

	assert.Eventually(t, func() bool {
		t := m.triggers()
		t.mu.Lock()
		defer t.mu.Unlock()
		return t.sharedDue && !t.sharedRefreshing
	}, 2*time.Second, 10*time.Millisecond,
		"a failed refresh requeues itself instead of waiting out the periodic sweep")

	// And it stops asking once it succeeds, so a healthy install is not refreshing in a loop.
	fail.Store(false)
	m.refreshSharedSnapshotsIfDue(context.Background(), logr.Discard())

	assert.Eventually(t, func() bool {
		t := m.triggers()
		t.mu.Lock()
		defer t.mu.Unlock()
		return !t.sharedDue && !t.sharedRefreshing
	}, 2*time.Second, 10*time.Millisecond, "a successful refresh asks for nothing")
}
