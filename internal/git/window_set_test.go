// SPDX-License-Identifier: Apache-2.0

package git

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCommitWindows_WindowsAreNumberedInTheOrderTheyOpen(t *testing.T) {
	var set commitWindows
	opened := time.Now()

	first := set.open(configMapTargetEvent("a", "alice", "team-a"), nil, opened)
	assert.Equal(t, opened, first.openedAt)
	assert.Equal(t, []*openWindow{first}, set.all())
	set.remove(first)
	assert.Empty(t, set.all())

	second := set.open(configMapTargetEvent("b", "bob", "team-a"), nil, opened)
	assert.Greater(t, second.seq, first.seq, "a window opened later is numbered after it")
}

func TestCommitWindows_ASetSaysWhichWindowAWriteOrARequestBelongsTo(t *testing.T) {
	var set commitWindows
	window := set.open(configMapTargetEvent("a", "alice", "team-a"), nil, time.Now())

	alice := configMapTargetEvent("b", "alice", "team-a")
	bob := configMapTargetEvent("b", "bob", "team-a")
	assert.Same(t, window, set.forWrite(alice))
	assert.Empty(t, set.blocking(alice))
	assert.Nil(t, set.forWrite(bob))
	assert.Equal(t, []*openWindow{window}, set.blocking(bob), "bob's write must close alice's window first")

	aliceSave := &pendingCommitRequest{author: "alice", gitTargetName: "team-a", gitTargetNamespace: "default"}
	bobSave := &pendingCommitRequest{author: "bob", gitTargetName: "team-a", gitTargetNamespace: "default"}
	assert.Same(t, window, set.forRequest(aliceSave))
	assert.Nil(t, set.foreignTo(aliceSave))
	assert.Nil(t, set.forRequest(bobSave))
	assert.Same(t, window, set.foreignTo(bobSave))

	id := commitRequestID{Namespace: "default", Name: "save", UID: "uid-save"}
	assert.Nil(t, set.carrying(id))
	window.pendingCR = &id
	assert.Same(t, window, set.carrying(id))
}

func TestCommitWindows_ClosingAWindowReleasesExactlyItsCharge(t *testing.T) {
	var set commitWindows
	window := set.open(configMapTargetEvent("a", "alice", "team-a"), nil, time.Now())
	set.charge(window, 100)
	set.charge(window, -30)
	require.Equal(t, int64(70), window.bytes)
	require.Equal(t, int64(70), set.windowBytes)

	set.remove(window)
	assert.False(t, set.anyOpen())
	assert.Zero(t, set.windowBytes)

	set.remove(window)
	assert.Zero(t, set.windowBytes, "a window already closed has nothing left to release")
}
