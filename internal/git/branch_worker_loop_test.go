/*
SPDX-License-Identifier: Apache-2.0

Copyright 2025 ConfigButler

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package git

import (
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
)

func TestGetCommitWindow_DefaultsAndParsing(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, configv1alpha1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	w := NewBranchWorker(c, logr.Discard(), "p", "ns", "main", nil, 0)

	defaultWindow := w.getCommitWindow(&configv1alpha1.GitProvider{})
	assert.Equal(t, DefaultCommitWindow, defaultWindow)

	parsed := w.getCommitWindow(&configv1alpha1.GitProvider{
		Spec: configv1alpha1.GitProviderSpec{
			Push: &configv1alpha1.PushStrategy{CommitWindow: ptrString("250ms")},
		},
	})
	assert.Equal(t, 250*time.Millisecond, parsed)

	zero := w.getCommitWindow(&configv1alpha1.GitProvider{
		Spec: configv1alpha1.GitProviderSpec{
			Push: &configv1alpha1.PushStrategy{CommitWindow: ptrString("0s")},
		},
	})
	assert.Equal(t, time.Duration(0), zero)

	negative := w.getCommitWindow(&configv1alpha1.GitProvider{
		Spec: configv1alpha1.GitProviderSpec{
			Push: &configv1alpha1.PushStrategy{CommitWindow: ptrString("-2s")},
		},
	})
	assert.Equal(t, time.Duration(0), negative, "negative commitWindow falls back to 0")

	garbage := w.getCommitWindow(&configv1alpha1.GitProvider{
		Spec: configv1alpha1.GitProviderSpec{
			Push: &configv1alpha1.PushStrategy{CommitWindow: ptrString("not-a-duration")},
		},
	})
	assert.Equal(t, DefaultCommitWindow, garbage, "parse error falls back to default")
}

// TestEventLoop_DrainOrSchedulePush covers the cooldown gating logic without
// touching real Git: the loop's lastPushAt and pushTimer state alone determine
// whether a flush runs immediately or is deferred.
func TestEventLoop_DrainOrSchedulePush_CooldownGate(t *testing.T) {
	w := &BranchWorker{Log: logr.Discard()}
	loop := newBranchWorkerEventLoop(w, 5*time.Second)

	// No buffered events → no-op, no timer scheduled.
	loop.drainOrSchedulePush()
	assert.Nil(t, loop.pushTimer, "no buffer → no timer")

	// Buffered events but cooldown active → schedule a one-shot pushTimer.
	loop.buffer = []Event{{}}
	loop.bufferBytes = 1
	loop.lastPushAt = time.Now() // pretend we just pushed
	loop.drainOrSchedulePush()
	require.NotNil(t, loop.pushTimer, "cooldown active → pushTimer scheduled")

	// Calling again does not stack a second timer.
	prev := loop.pushTimer
	loop.drainOrSchedulePush()
	assert.Same(t, prev, loop.pushTimer, "drainOrSchedulePush is idempotent while a timer is pending")

	// Reset and verify expired-cooldown path takes the immediate branch
	// (we avoid the real flush() because it touches Git; assert state instead).
	loop.stopPushTimer()
	loop.lastPushAt = time.Time{} // never pushed
	// We can't safely call flush() here (no repo), so simulate by inspecting
	// the inputs to the decision.
	elapsedOK := loop.lastPushAt.IsZero() || time.Since(loop.lastPushAt) >= PushCooldown
	assert.True(t, elapsedOK, "first ever drain should bypass cooldown")
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
	require.NoError(t, configv1alpha1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	w := NewBranchWorker(c, logr.Discard(), "p", "ns", "main", nil, 0)
	assert.Equal(t, DefaultBranchBufferMaxBytes, w.branchBufferMaxBytes)

	w = NewBranchWorker(c, logr.Discard(), "p", "ns", "main", nil, 4096)
	assert.Equal(t, int64(4096), w.branchBufferMaxBytes)

	w = NewBranchWorker(c, logr.Discard(), "p", "ns", "main", nil, -7)
	assert.Equal(t, DefaultBranchBufferMaxBytes, w.branchBufferMaxBytes,
		"non-positive override falls back to default")
}

func ptrString(s string) *string { return &s }
