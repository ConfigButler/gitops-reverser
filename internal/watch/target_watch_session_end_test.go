// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// A session ENDING says only that it ended. Grading a clean end as Blocked is what reported the
// API server's own randomized watch timeout — the protocol working — as a stalled stream, costing
// a Warning event and a Ready flip roughly every forty minutes per type on a healthy cluster.
func TestTargetStreamStateForSessionEnd(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		err        error
		wantMark   bool
		wantState  StreamState
		wantReason string
	}{
		"a clean session end is not a state change at all": {
			err:      errTargetWatchClosed,
			wantMark: false,
		},
		"a wrapped clean end is still clean": {
			err:      fmt.Errorf("pump: %w", errTargetWatchClosed),
			wantMark: false,
		},
		// Routine watch-history pressure, graded exactly as the resume path grades it. Which
		// session observes the 410 must not change how it reads.
		"an expired cursor is replaying, not blocked": {
			err:        errTargetWatchExpired,
			wantMark:   true,
			wantState:  StreamStateReplaying,
			wantReason: StreamReasonExpiredResourceVersion,
		},
		"anything else is a blocked stream": {
			err:        errors.New("connection refused"),
			wantMark:   true,
			wantState:  StreamStateBlocked,
			wantReason: StreamReasonWatchError,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			state, reason, mark := targetStreamStateForSessionEnd(tc.err)
			require.Equal(t, tc.wantMark, mark)
			if !tc.wantMark {
				return
			}
			assert.Equal(t, tc.wantState, state)
			assert.Equal(t, tc.wantReason, reason)
		})
	}
}

// The design claim this guards: a clean close publishes nothing, and the NEXT open is what reports
// a real fault. Both halves in one test, because neither is safe alone — suppressing the clean end
// would hide a genuine outage if the open did not speak up, and that is exactly the trade being
// made here.
func TestRunTargetWatch_CleanCloseIsSilentAndTheNextOpenReports(t *testing.T) {
	gitDest := types.NewResourceReference("target", "default")
	key := targetWatchKey{GVR: configmapsGVR, Namespace: "apps"}

	var mu sync.Mutex
	opens := 0
	openErr := errors.New("dial tcp: connection refused")
	firstWatch := watch.NewFake()

	manager := &Manager{
		Log: logr.Discard(),
		targetWatchOpen: func(
			_ context.Context,
			_ schema.GroupVersionResource,
			_ string,
			_ metav1.ListOptions,
		) (watch.Interface, error) {
			mu.Lock()
			defer mu.Unlock()
			opens++
			if opens == 1 {
				return firstWatch, nil
			}
			return nil, openErr
		},
		targetWatchList: func(
			_ context.Context,
			_ schema.GroupVersionResource,
			_ string,
			_ metav1.ListOptions,
		) (*unstructured.UnstructuredList, error) {
			return nil, errors.New("no list fallback expected")
		},
	}
	manager.rememberGitTargetUID(gitDest.WithUID("uid-1"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		manager.runTargetWatch(ctx, logr.Discard(), gitDest, testStream(key, nil))
	}()

	// The first session opens and marks the cell Replaying while it folds initial events.
	require.Eventually(t, func() bool {
		return streamStateFor(manager, gitDest, key) == StreamStateReplaying
	}, time.Second, 10*time.Millisecond, "the first session marks its cell replaying")

	// Now end that session the way the API server ends one: close the result channel. This is a
	// clean end, and it must publish NOTHING — the cell keeps the state the session left it in.
	firstWatch.Stop()
	assert.Never(t, func() bool {
		return streamStateFor(manager, gitDest, key) == StreamStateBlocked
	}, targetWatchBackoff/2, 10*time.Millisecond,
		"a clean session end must not report the stream blocked")

	// The reconnect's open fails, and THAT is what reports the fault — one backoff later, from the
	// call that actually failed.
	require.Eventually(t, func() bool {
		return streamStateFor(manager, gitDest, key) == StreamStateBlocked
	}, 2*targetWatchBackoff, 10*time.Millisecond,
		"the next open's failure is what marks the stream blocked")

	status := streamStatusFor(manager, gitDest, key)
	assert.Equal(t, StreamReasonWatchError, status.reason)
	assert.Contains(t, status.message, "connection refused")

	cancel()
	<-done
}

func streamStatusFor(m *Manager, gitDest types.ResourceReference, key targetWatchKey) targetStreamStatus {
	return m.watchPlane().streams[gitDest.Key()][key.Cell()]
}

func streamStateFor(m *Manager, gitDest types.ResourceReference, key targetWatchKey) StreamState {
	return streamStatusFor(m, gitDest, key).state
}
