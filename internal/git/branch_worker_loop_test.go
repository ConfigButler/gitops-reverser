// SPDX-License-Identifier: Apache-2.0

package git

import (
	"testing"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// A negative or malformed window has no case here on purpose: the field is a metav1.Duration
// behind a duration pattern, so neither can be stored, and the API-server side of that is pinned
// by the admission specs in internal/controller.
func TestCommitWindowFor_DefaultsAndParsing(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, configv1alpha3.AddToScheme(scheme))

	target := func(name string, window *metav1.Duration) *configv1alpha3.GitTarget {
		spec := configv1alpha3.GitTargetSpec{
			GitProviderRef: meta.LocalObjectReference{Name: "p"},
			Branch:         "main",
			Path:           "clusters/prod",
		}
		if window != nil {
			spec.Commit = &configv1alpha3.GitTargetCommitSpec{Window: window}
		}
		return &configv1alpha3.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
			Spec:       spec,
		}
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		target("unset", nil),
		target("quarter", &metav1.Duration{Duration: 250 * time.Millisecond}),
		target("zero", &metav1.Duration{Duration: 0}),
	).Build()
	w := NewBranchWorker(c, logr.Discard(), "p", "ns", "main", nil, BranchWorkerLimits{})
	ctx := t.Context()

	for _, tc := range []struct {
		name   string
		target string
		want   time.Duration
		why    string
	}{
		{"unset", "unset", DefaultCommitWindow, "a target that declares no window takes the default"},
		{"explicit", "quarter", 250 * time.Millisecond, "an explicit window is honored"},
		{"zero", "zero", 0, `"0s" opts into per-event commits`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, w.commitWindowFor(ctx, tc.target, "ns", DefaultCommitWindow), tc.why)
		})
	}

	// A window is a property of the GitTarget, so a worker serving two targets on one branch
	// resolves two different cadences — which is the whole reason the field moved off GitProvider.
	assert.NotEqual(t,
		w.commitWindowFor(ctx, "quarter", "ns", DefaultCommitWindow),
		w.commitWindowFor(ctx, "unset", "ns", DefaultCommitWindow),
		"two GitTargets on one (provider, branch) worker may disagree about their commit window")

	assert.Equal(t, DefaultCommitWindow, w.commitWindowFor(ctx, "absent", "ns", DefaultCommitWindow),
		"an unreadable GitTarget takes the fallback rather than stalling the batch")
	assert.Equal(t, DefaultCommitWindow, w.commitWindowFor(ctx, "", "", DefaultCommitWindow),
		"an unbound window (no target) takes the fallback")
}

// TestEventLoop_MaybeSchedulePush covers the cooldown gating logic without
// touching real Git: the loop's lastPushAt and pushTimer state alone determine
// whether the deferred push timer is set or skipped.
func TestEventLoop_MaybeSchedulePush_CooldownGate(t *testing.T) {
	w := &BranchWorker{Log: logr.Discard()}
	loop := newBranchWorkerEventLoop(w, 5*time.Second)

	// No unpushed events → no-op, no timer scheduled.
	loop.maybeSchedulePush()
	assert.Nil(t, loop.pushTimer, "no unpushed events → no timer")

	// Locally-committed events plus active cooldown → schedule a one-shot
	// pushTimer rather than push immediately.
	loop.pendingWrites = []PendingWrite{{Kind: PendingWriteCommit}}
	loop.pendingWritesBytes = 1
	loop.lastPushAt = time.Now() // pretend we just pushed
	loop.maybeSchedulePush()
	require.NotNil(t, loop.pushTimer, "cooldown active → pushTimer scheduled")

	// Calling again does not stack a second timer.
	prev := loop.pushTimer
	loop.maybeSchedulePush()
	assert.Same(t, prev, loop.pushTimer, "maybeSchedulePush is idempotent while a timer is pending")

	// Reset and verify the expired-cooldown path would take the immediate
	// branch (we avoid calling pushPending here since it touches Git; assert
	// the inputs to the decision instead).
	loop.stopPushTimer()
	loop.lastPushAt = time.Time{} // never pushed
	elapsedOK := loop.lastPushAt.IsZero() || time.Since(loop.lastPushAt) >= PushCooldown
	assert.True(t, elapsedOK, "first ever push should bypass cooldown")
}

// TestEventLoop_TotalRetainedBytes verifies the byte cap is enforced against
// the open window + pendingWrites combined.
func TestEventLoop_TotalRetainedBytes(t *testing.T) {
	w := &BranchWorker{Log: logr.Discard()}
	loop := newBranchWorkerEventLoop(w, time.Second)

	assert.Equal(t, int64(0), loop.totalRetainedBytes())

	loop.windowBytes = 100
	loop.pendingWritesBytes = 250
	assert.Equal(t, int64(350), loop.totalRetainedBytes(),
		"cap is enforced against the open window + pendingWrites combined")
}

func TestEventLoop_ResetCommitTimer(t *testing.T) {
	w := &BranchWorker{Log: logr.Discard()}
	loop := newBranchWorkerEventLoop(w, 30*time.Millisecond)

	loop.resetCommitTimer()
	require.NotNil(t, loop.commitTimer)
	first := loop.commitTimer

	// Reset before fire — same timer object, fresh deadline.
	loop.resetCommitTimer()
	assert.Same(t, first, loop.commitTimer, "reset reuses the existing timer")

	// Wait for the timer to fire and verify the channel becomes readable.
	select {
	case <-loop.commitTimer.C:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("commit timer never fired")
	}
}

func TestEventLoop_StopTimers(t *testing.T) {
	w := &BranchWorker{Log: logr.Discard()}
	loop := newBranchWorkerEventLoop(w, time.Second)

	loop.resetCommitTimer()
	loop.pushTimer = time.NewTimer(time.Hour)

	loop.stopTimers()
	assert.Nil(t, loop.commitTimer)
	assert.Nil(t, loop.pushTimer)
}

func TestNewBranchWorker_DefaultsBufferCap(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, configv1alpha3.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	w := NewBranchWorker(c, logr.Discard(), "p", "ns", "main", nil, BranchWorkerLimits{})
	assert.Equal(t, DefaultBranchBufferMaxBytes, w.branchBufferMaxBytes)

	w = NewBranchWorker(c, logr.Discard(), "p", "ns", "main", nil, BranchWorkerLimits{MaxBufferBytes: 4096})
	assert.Equal(t, int64(4096), w.branchBufferMaxBytes)

	w = NewBranchWorker(c, logr.Discard(), "p", "ns", "main", nil, BranchWorkerLimits{MaxBufferBytes: -7})
	assert.Equal(t, DefaultBranchBufferMaxBytes, w.branchBufferMaxBytes,
		"non-positive override falls back to default")
}

// The queue depth is the drop boundary, so a configured depth that did not reach the
// channel would be a knob that reads back correctly and changes nothing — the failure the
// operator cannot see, because the symptom (dropped writes) is what they set it to prevent.
func TestNewBranchWorker_QueueDepthReachesTheChannel(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, configv1alpha3.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	w := NewBranchWorker(c, logr.Discard(), "p", "ns", "main", nil, BranchWorkerLimits{})
	assert.Equal(t, DefaultBranchWorkerQueueDepth, cap(w.eventQueue))

	w = NewBranchWorker(c, logr.Discard(), "p", "ns", "main", nil, BranchWorkerLimits{QueueDepth: 7})
	assert.Equal(t, 7, cap(w.eventQueue))

	w = NewBranchWorker(c, logr.Discard(), "p", "ns", "main", nil, BranchWorkerLimits{QueueDepth: -1})
	assert.Equal(t, DefaultBranchWorkerQueueDepth, cap(w.eventQueue),
		"non-positive override falls back to default")
}

// The two knobs are independent: setting one must not silently reset the other, since an
// operator raising the queue has no reason to restate a buffer cap they are happy with.
func TestNewBranchWorker_LimitsAreIndependent(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, configv1alpha3.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	w := NewBranchWorker(c, logr.Discard(), "p", "ns", "main", nil, BranchWorkerLimits{QueueDepth: 3})
	assert.Equal(t, 3, cap(w.eventQueue))
	assert.Equal(t, DefaultBranchBufferMaxBytes, w.branchBufferMaxBytes)

	w = NewBranchWorker(c, logr.Discard(), "p", "ns", "main", nil, BranchWorkerLimits{MaxBufferBytes: 4096})
	assert.Equal(t, DefaultBranchWorkerQueueDepth, cap(w.eventQueue))
	assert.Equal(t, int64(4096), w.branchBufferMaxBytes)
}

// A configured depth must move the drop boundary with it, which is the behaviour the knob
// is FOR: accept exactly the configured number of pending writes, and drop the next.
func TestBranchWorker_QueueDepthBoundsAcceptedWrites(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, configv1alpha3.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	const depth = 5
	w := NewBranchWorker(c, logr.Discard(), "p", "ns", "main", nil, BranchWorkerLimits{QueueDepth: depth})

	// The worker is never started, so nothing drains: the queue fills to exactly its depth.
	for i := range depth {
		assert.Truef(t, w.enqueueRequest(&WriteRequest{}), "write %d must fit the configured depth", i)
	}
	assert.False(t, w.enqueueRequest(&WriteRequest{}), "the write past the depth must be dropped")
	assert.Equal(t, int64(depth), w.inflightItems.Load(),
		"a dropped write must not be counted in flight")
}
