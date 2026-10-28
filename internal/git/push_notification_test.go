// SPDX-License-Identifier: Apache-2.0

package git

import (
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// queuedItems takes every item the worker queued for its loop.
func queuedItems(w *BranchWorker) []WorkItem {
	var items []WorkItem
	for {
		select {
		case item := <-w.eventQueue:
			items = append(items, item)
		default:
			return items
		}
	}
}

// TestPushNotice_ARunOfReportsQueuesOneLook: any number of deliveries reported while a notice waits
// cost the worker one look, and the next report after that look is dispatched queues another. What
// the folded notice names is what the open window decides on: the reports' commit when they agree,
// nothing (a push not yet seen) when they do not.
func TestPushNotice_ARunOfReportsQueuesOneLook(t *testing.T) {
	for _, tc := range []struct {
		name      string
		reports   []string
		wantAfter string
		wantClose bool
	}{
		{name: "late reports of our own push", reports: []string{"ours", "ours", "ours"}, wantAfter: "ours"},
		{name: "our push and somebody else's", reports: []string{"ours", "theirs"}, wantClose: true},
		{name: "somebody else's push, then ours", reports: []string{"theirs", "ours"}, wantClose: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loop, _ := applierClaimLoop(t, applierTestTarget("alpha", true), &configv1alpha3.GitProvider{
				ObjectMeta: metav1.ObjectMeta{Name: "test-provider", Namespace: "team-a"},
				Spec:       configv1alpha3.GitProviderSpec{URL: "https://github.com/acme/platform"},
			})
			loop.w.recordRemoteObservation("ours", ObservedByPush)
			loop.windows.setOnly(&openWindow{})

			for _, after := range tc.reports {
				loop.w.EnqueuePushNotification(after)
			}
			items := queuedItems(loop.w)
			require.Len(t, items, 1, "one look stands for every report")
			loop.dispatch(items[0])

			assert.Equal(t, tc.wantAfter, items[0].Refresh.After)
			assert.Equal(t, tc.wantClose, loop.pushHinted)

			loop.w.EnqueuePushNotification("later")
			assert.Len(t, queuedItems(loop.w), 1, "a report after the look was dispatched queues its own")
		})
	}
}

// TestPushNotice_ADroppedLookFreesTheSlot: a notice the queue refuses is not waiting, so it must not
// fold the next report into nothing.
func TestPushNotice_ADroppedLookFreesTheSlot(t *testing.T) {
	w := &BranchWorker{Log: logr.Discard(), Branch: "main", eventQueue: make(chan WorkItem)}

	w.EnqueuePushNotification("aaa")
	_, waiting := w.pushNotices.take()
	assert.False(t, waiting, "the refused notice left nothing waiting")

	assert.True(t, w.pushNotices.offer("bbb"), "the next report queues a look of its own")
}

// TestPushNotice_LastPushNoticeIsWhenItWasReported records every report, folded or not, since that
// is what GitBranch.status.lastPushNoticeAt says: deliveries reach this branch.
func TestPushNotice_LastPushNoticeIsWhenItWasReported(t *testing.T) {
	w := &BranchWorker{Log: logr.Discard(), Branch: "main", eventQueue: make(chan WorkItem, 1)}
	assert.True(t, w.LastPushNotice().IsZero(), "no report yet")

	before := time.Now()
	w.EnqueuePushNotification("aaa")
	first := w.LastPushNotice()
	assert.False(t, first.Before(before))

	w.EnqueuePushNotification("bbb")
	assert.False(t, w.LastPushNotice().Before(first), "a folded report is still a report")
}

// TestDispatch_APushNoticeNotFoldedKeepsItsCommit: only a notice EnqueuePushNotification queued
// takes the fold, so a refresh queued with its own commit keeps it.
func TestDispatch_APushNoticeNotFoldedKeepsItsCommit(t *testing.T) {
	loop, _ := applierClaimLoop(t, applierTestTarget("alpha", true))
	req := &RefreshRequest{PushNotice: true, After: "ccc"}
	loop.dispatch(WorkItem{Refresh: req})
	assert.Equal(t, "ccc", req.After)
}
