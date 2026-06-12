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

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TestHandleResyncRequest_ClosedWindowIsPushedEvenWhenNoOpResync pins the
// stranded-write fix (docs/design/stream/commitrequest-design.md §6.4.2, §9.5): a
// resync that closes a live commit window but commits nothing of its own must
// still schedule the window's commit for push. Before the fix maybeSchedulePush
// ran only when the resync itself committed, so a window the resync closed was
// committed locally and then stranded — never reaching the remote.
func TestHandleResyncRequest_ClosedWindowIsPushedEvenWhenNoOpResync(t *testing.T) {
	worker, serverRepo, _ := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")

	loop := newBranchWorkerEventLoop(worker, time.Hour)
	defer loop.stopTimers()

	initialRef, err := serverRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	initialHash := initialRef.Hash()

	// A live ConfigMap edit opens a window (commitWindow is an hour, so it stays open).
	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events:     []Event{configMapTargetEvent("held", "alice", "team-a")},
		CommitMode: CommitModePerEvent,
	}})
	require.NotNil(t, loop.openWindow, "the edit must open a window")

	// A type-scoped resync for a DIFFERENT type with an empty desired set: it closes
	// the open window (resync-before-apply) but its own mark-and-sweep — scoped to a
	// type with no documents — changes nothing, so the resync itself does not commit.
	scope := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	resultCh := make(chan ResyncResult, 1)
	loop.handleResyncRequest(&ResyncRequest{
		GitTargetName:      "team-a",
		GitTargetNamespace: "default",
		ScopeGVR:           &scope,
		Desired:            nil,
		Result:             resultCh,
	})
	res := <-resultCh
	require.NoError(t, res.Err)
	require.Zero(t, res.Stats.Created+res.Stats.Updated+res.Stats.Deleted,
		"the scoped resync must itself commit nothing")

	// The window's commit must have been pushed, not stranded as a retained pending write.
	assert.Empty(t, loop.pendingWrites, "the closed window's commit must not be retained unpushed")
	afterRef, err := serverRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	assert.NotEqual(t, initialHash, afterRef.Hash(),
		"the held edit's commit must reach the remote even though the resync was a no-op")
}
