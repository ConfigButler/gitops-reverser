// SPDX-License-Identifier: Apache-2.0

package git

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// only is the one open window, or nil, for a test of a worker that holds one window at a time. It
// panics when several are open: such a test asks which one it means.
func (s *commitWindows) only() *openWindow {
	switch len(s.windows) {
	case 0:
		return nil
	case 1:
		return s.windows[0]
	default:
		panic(fmt.Sprintf("%d commit windows are open; the test reads one", len(s.windows)))
	}
}

// setOnly makes w the one open window, or leaves none open when w is nil, for a test that fakes one.
// What the windows are charged is left alone.
func (s *commitWindows) setOnly(w *openWindow) {
	s.windows = nil
	if w != nil {
		s.windows = []*openWindow{w}
	}
}

func TestCommitWindows_WindowsAreNumberedInTheOrderTheyOpen(t *testing.T) {
	var set commitWindows
	opened := time.Now()

	first := set.open(configMapTargetEvent("a", "alice", "team-a"), nil, opened)
	assert.Equal(t, opened, first.openedAt)
	assert.Equal(t, []*openWindow{first}, set.all())
	second := set.open(configMapTargetEvent("b", "bob", "team-a"), nil, opened)
	assert.Greater(t, second.seq, first.seq, "a window opened later is numbered after it")
	assert.Equal(t, []*openWindow{first, second}, set.all(), "oldest first")

	set.remove(first)
	assert.Equal(t, []*openWindow{second}, set.all())
	third := set.open(configMapTargetEvent("c", "carol", "team-a"), nil, opened)
	assert.Equal(t, []*openWindow{second, third}, set.all(), "a close keeps the others in their order")
	assert.Equal(t, 2, set.count())
}

func TestCommitWindows_ASetSaysWhichWindowAWriteOrARequestBelongsTo(t *testing.T) {
	var set commitWindows
	window := set.open(configMapTargetEvent("a", "alice", "team-a"), nil, time.Now())

	alice := configMapTargetEvent("b", "alice", "team-a")
	bob := configMapTargetEvent("b", "bob", "team-a")
	assert.Same(t, window, set.forWrite(alice))
	assert.Empty(t, set.others(set.forWrite(alice)))
	assert.Nil(t, set.forWrite(bob))
	assert.Equal(t, []*openWindow{window}, set.others(set.forWrite(bob)))

	aliceSave := &pendingCommitRequest{author: "alice", gitTargetName: "team-a", gitTargetNamespace: "default"}
	bobSave := &pendingCommitRequest{author: "bob", gitTargetName: "team-a", gitTargetNamespace: "default"}
	assert.Same(t, window, set.forRequest(aliceSave))
	assert.Nil(t, set.foreignTo(aliceSave))
	assert.Nil(t, set.forRequest(bobSave))
	assert.Same(t, window, set.foreignTo(bobSave))

	bobs := set.open(bob, nil, time.Now())
	assert.Same(t, bobs, set.forRequest(bobSave))
	assert.Nil(t, set.foreignTo(bobSave), "a window of the request's own author is open: nobody stands in its way")
	carolSave := &pendingCommitRequest{author: "carol", gitTargetName: "team-a", gitTargetNamespace: "default"}
	assert.Same(t, window, set.foreignTo(carolSave), "the oldest window it cannot attach to")

	id := commitRequestID{Namespace: "default", Name: "save", UID: "uid-save"}
	assert.Nil(t, set.carrying(id))
	bobs.pendingCR = &id
	assert.Same(t, bobs, set.carrying(id))
}

func TestCommitWindows_ClosingAWindowReleasesExactlyItsCharge(t *testing.T) {
	var set commitWindows
	window := set.open(configMapTargetEvent("a", "alice", "team-a"), nil, time.Now())
	other := set.open(configMapTargetEvent("b", "bob", "team-a"), nil, time.Now())
	set.charge(window, 100)
	set.charge(window, -30)
	set.charge(other, 5)
	require.Equal(t, int64(70), window.bytes)
	require.Equal(t, int64(75), set.bytes())

	set.remove(window)
	assert.True(t, set.isOpen(other))
	assert.False(t, set.isOpen(window))
	assert.Equal(t, int64(5), set.bytes())

	set.remove(window)
	assert.Equal(t, int64(5), set.bytes(), "a window already closed has nothing left to release")
	set.remove(other)
	assert.False(t, set.anyOpen())
	assert.Zero(t, set.bytes())
}
