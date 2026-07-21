// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"errors"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// A retained PendingWrite is committed locally but not yet pushed, and a push conflict REPLAYS it
// against the rebased worktree — re-planning, so it can decide deletions the first apply never
// made. These tests pin which policy that replay runs under.

const (
	replayTargetName      = "acme"
	replayTargetNamespace = "tenant-acme"
)

func replayScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, configv1alpha3.AddToScheme(scheme))
	return scheme
}

// gitTargetWithMode is the CURRENT stored object — what an operator's `kubectl patch` produced
// after the pending write below was planned.
func gitTargetWithMode(mode configv1alpha3.PruneMode) *configv1alpha3.GitTarget {
	return &configv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: replayTargetName, Namespace: replayTargetNamespace},
		Spec: configv1alpha3.GitTargetSpec{
			Path:  "tenants/acme",
			Prune: &configv1alpha3.PrunePolicy{Mode: mode},
		},
	}
}

// retainedWriteUnder is a resync write already committed locally under plannedMode, waiting for a
// push that has not succeeded yet.
func retainedWriteUnder(plannedMode configv1alpha3.PruneMode) PendingWrite {
	key := pendingTargetKey{Name: replayTargetName, Namespace: replayTargetNamespace}
	return PendingWrite{
		Kind:               PendingWriteResync,
		GitTargetName:      replayTargetName,
		GitTargetNamespace: replayTargetNamespace,
		Targets: map[pendingTargetKey]ResolvedTargetMetadata{
			key: {
				Name:      replayTargetName,
				Namespace: replayTargetNamespace,
				Path:      "tenants/acme",
				PruneMode: plannedMode,
			},
		},
	}
}

func replayWorker(t *testing.T, objects []client.Object, fns *interceptor.Funcs) *BranchWorker {
	t.Helper()
	builder := fake.NewClientBuilder().WithScheme(replayScheme(t)).WithObjects(objects...)
	if fns != nil {
		builder = builder.WithInterceptorFuncs(*fns)
	}
	worker := NewBranchWorker(builder.Build(), logr.Discard(), "provider", "default", "main", nil, 0)
	worker.ctx = context.Background()
	return worker
}

func modeOfWrite(t *testing.T, pw PendingWrite) configv1alpha3.PruneMode {
	t.Helper()
	return pw.Target().PruneMode
}

// TestTightenPendingPruneModes_TighteningReachesAWriteThatHasNotPushed is the emergency stop.
//
// A resync planned under `always` is committed locally and waiting on a push that keeps
// conflicting. The operator patches the GitTarget to `onEvent` to stop the deletions. Without this,
// the replay re-plans under the captured `always` and can sweep documents against a remote that has
// changed since — deletions the operator has already revoked.
func TestTightenPendingPruneModes_TighteningReachesAWriteThatHasNotPushed(t *testing.T) {
	worker := replayWorker(t, []client.Object{gitTargetWithMode(configv1alpha3.PruneOnEvent)}, nil)
	writes := []PendingWrite{retainedWriteUnder(configv1alpha3.PruneAlways)}

	require.NoError(t, worker.tightenPendingPruneModes(worker.ctx, writes))

	assert.Equal(t, configv1alpha3.PruneOnEvent, modeOfWrite(t, writes[0]),
		"a policy tightened after the write was planned must apply to the replay")
}

// TestTightenPendingPruneModes_LooseningNeverEscalatesAPlannedWrite is the opposite direction, and
// it must NOT be symmetric. The retained write chose to keep its orphans against a desired snapshot
// that is now stale; declaring `always` afterwards applies to the next resync — which gathers a
// fresh snapshot — not to this one.
func TestTightenPendingPruneModes_LooseningNeverEscalatesAPlannedWrite(t *testing.T) {
	worker := replayWorker(t, []client.Object{gitTargetWithMode(configv1alpha3.PruneAlways)}, nil)
	writes := []PendingWrite{retainedWriteUnder(configv1alpha3.PruneOnEvent)}

	require.NoError(t, worker.tightenPendingPruneModes(worker.ctx, writes))

	assert.Equal(t, configv1alpha3.PruneOnEvent, modeOfWrite(t, writes[0]),
		"a widened policy must not turn a stale retention decision into deletions")
}

// TestTightenPendingPruneModes_DeletedGitTargetReplaysUnderNever covers the definite no-answer: the
// GitTarget is gone, so no policy authorizes anything. Retrying could not produce a better answer,
// so the replay proceeds under the most restrictive mode rather than the captured one.
func TestTightenPendingPruneModes_DeletedGitTargetReplaysUnderNever(t *testing.T) {
	worker := replayWorker(t, nil, nil)
	writes := []PendingWrite{retainedWriteUnder(configv1alpha3.PruneAlways)}

	require.NoError(t, worker.tightenPendingPruneModes(worker.ctx, writes))

	assert.Equal(t, configv1alpha3.PruneNever, modeOfWrite(t, writes[0]),
		"a deleted GitTarget must not keep authorizing deletions through a retained write")
}

// TestTightenPendingPruneModes_UnreadablePolicyRetriesInsteadOfGuessing covers the case with no
// answer at all. Guessing the captured mode could apply a revoked deletion; guessing the strictest
// mode could silently drop a legitimate one, which under `onEvent` no later resync re-derives.
// Returning the error does neither: the push cycle leaves the writes retained and tries again.
func TestTightenPendingPruneModes_UnreadablePolicyRetriesInsteadOfGuessing(t *testing.T) {
	unavailable := apierrors.NewServiceUnavailable("cache not synced")
	worker := replayWorker(t, []client.Object{gitTargetWithMode(configv1alpha3.PruneOnEvent)}, &interceptor.Funcs{
		Get: func(
			_ context.Context, _ client.WithWatch, _ client.ObjectKey, obj client.Object, _ ...client.GetOption,
		) error {
			if _, isTarget := obj.(*configv1alpha3.GitTarget); isTarget {
				return unavailable
			}
			return nil
		},
	})
	writes := []PendingWrite{retainedWriteUnder(configv1alpha3.PruneAlways)}

	err := worker.tightenPendingPruneModes(worker.ctx, writes)

	require.Error(t, err, "an unreadable policy must fail the replay so the push retries it")
	assert.True(t, errors.Is(err, unavailable) || apierrors.IsServiceUnavailable(err),
		"the underlying read failure must survive wrapping, so the retry is diagnosable")
	assert.Equal(t, configv1alpha3.PruneAlways, modeOfWrite(t, writes[0]),
		"nothing is decided when the policy could not be read")
}

// TestTightenPendingPruneModes_CoversBothDeletionPaths is why the tightening mutates the shared
// Targets map rather than a local copy: the resync sweep reads its mode through PendingWrite.Target
// and the steady-state DELETE writer reads it through pruneModeForBase. One pass has to serve both,
// or `never` would stop half of what an operator just asked it to stop.
func TestTightenPendingPruneModes_CoversBothDeletionPaths(t *testing.T) {
	worker := replayWorker(t, []client.Object{gitTargetWithMode(configv1alpha3.PruneNever)}, nil)
	writes := []PendingWrite{retainedWriteUnder(configv1alpha3.PruneAlways)}

	require.NoError(t, worker.tightenPendingPruneModes(worker.ctx, writes))

	assert.Equal(t, configv1alpha3.PruneNever, modeOfWrite(t, writes[0]),
		"the resync sweep reads the mode here")
	assert.Equal(t, configv1alpha3.PruneNever, pruneModeForBase(writes[0].Targets, "tenants/acme"),
		"the steady-state DELETE writer reads it here, off the same map")
}

// TestTightenPendingPruneModes_ReadsEachGitTargetOnce keeps the replay's cost proportional to the
// targets involved rather than to the retained writes: a conflicting push can be replaying many
// windows for one busy target.
func TestTightenPendingPruneModes_ReadsEachGitTargetOnce(t *testing.T) {
	var gets int
	worker := replayWorker(t, []client.Object{gitTargetWithMode(configv1alpha3.PruneOnEvent)}, &interceptor.Funcs{
		Get: func(
			ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption,
		) error {
			if _, isTarget := obj.(*configv1alpha3.GitTarget); isTarget {
				gets++
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	writes := []PendingWrite{
		retainedWriteUnder(configv1alpha3.PruneAlways),
		retainedWriteUnder(configv1alpha3.PruneAlways),
		retainedWriteUnder(configv1alpha3.PruneAlways),
	}

	require.NoError(t, worker.tightenPendingPruneModes(worker.ctx, writes))

	assert.Equal(t, 1, gets, "one policy read per GitTarget, not per retained write")
	for i := range writes {
		assert.Equal(t, configv1alpha3.PruneOnEvent, modeOfWrite(t, writes[i]))
	}
}

// TestMoreRestrictiveOf_OrdersTheModes pins the ordering the replay depends on, including the two
// values that are not modes: the empty string (unset, which means onEvent) and an unrecognized one
// (a policy this build cannot interpret, which must authorize nothing).
func TestMoreRestrictiveOf_OrdersTheModes(t *testing.T) {
	for _, tc := range []struct {
		name        string
		left, right configv1alpha3.PruneMode
		want        configv1alpha3.PruneMode
	}{
		{"always over onEvent", configv1alpha3.PruneAlways, configv1alpha3.PruneOnEvent, configv1alpha3.PruneOnEvent},
		{"always over never", configv1alpha3.PruneAlways, configv1alpha3.PruneNever, configv1alpha3.PruneNever},
		{"onEvent over never", configv1alpha3.PruneOnEvent, configv1alpha3.PruneNever, configv1alpha3.PruneNever},
		{"never under always", configv1alpha3.PruneNever, configv1alpha3.PruneAlways, configv1alpha3.PruneNever},
		{"identical", configv1alpha3.PruneAlways, configv1alpha3.PruneAlways, configv1alpha3.PruneAlways},
		{"unset resolves to onEvent", "", configv1alpha3.PruneAlways, configv1alpha3.PruneOnEvent},
		{"unset against never", "", configv1alpha3.PruneNever, configv1alpha3.PruneNever},
		{"unrecognized authorizes nothing", configv1alpha3.PruneAlways, "someFutureMode", "someFutureMode"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.left.MoreRestrictiveOf(tc.right))
		})
	}
}

// TestMoreRestrictiveOf_UnrecognizedResultStillAuthorizesNothing guards the one place returning the
// raw unrecognized value could go wrong: it is only safe because both predicates already read it as
// false. If that ever changes, an unknown policy would start deleting.
func TestMoreRestrictiveOf_UnrecognizedResultStillAuthorizesNothing(t *testing.T) {
	result := configv1alpha3.PruneAlways.MoreRestrictiveOf("someFutureMode")

	assert.False(t, result.SweepsOrphans())
	assert.False(t, result.AppliesEventDeletes())
}
