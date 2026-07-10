// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// A watch whose result channel closes because the operator is shutting down has not failed.
// Both the ctx.Done() case and the closed-channel case are ready at once, and Go's select
// picks between ready cases at random — so a clean shutdown reported a spurious
// "target watch result channel closed" about half the time. Nothing retried on it, but it
// surfaced as an error on every teardown, and it is the kind of noise that trains an
// operator to ignore the log.
//
// Repeated because the bug is a coin flip per iteration: one run proves nothing.
func TestStreamLiveTargetWatchEvents_CanceledContextIsNotAnError(t *testing.T) {
	t.Parallel()

	m := &Manager{Log: logr.Discard()}
	gitDest := types.NewResourceReference("target", "default")
	key := targetWatchKey{GVR: configmapsGVR, Namespace: "apps"}

	for i := range 200 {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // shutting down

		events := make(chan watch.Event)
		close(events) // ...which is why the watch closed

		err := m.streamLiveTargetWatchEvents(ctx, logr.Discard(), gitDest, key, OperationSet{}, events)
		require.NoErrorf(t, err, "iteration %d: a closed watch during shutdown is not an error", i)
	}
}

// The same channel closing while the operator is still running IS an error: the watch died
// under it, and the caller must reopen rather than silently stop mirroring the type.
func TestStreamLiveTargetWatchEvents_ClosedChannelWhileRunningIsAnError(t *testing.T) {
	t.Parallel()

	m := &Manager{Log: logr.Discard()}
	gitDest := types.NewResourceReference("target", "default")
	key := targetWatchKey{GVR: configmapsGVR, Namespace: "apps"}

	events := make(chan watch.Event)
	close(events)

	err := m.streamLiveTargetWatchEvents(
		context.Background(), logr.Discard(), gitDest, key, OperationSet{}, events)
	require.ErrorIs(t, err, errTargetWatchClosed)
}
