// SPDX-License-Identifier: Apache-2.0

package git

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// TestApplyWindow_AWithheldApplyIsNotCommittedUnderTheNextEdit is a known bug (docs/TODO.md, "A
// withheld apply rides the next edit"), written now and skipped until it is fixed; set
// GITOPS_REVERSER_KNOWN_BUGS=1 to run it.
//
// S1 withholds an apply that lands over an edit decided after the flush mark, and a Repair puts the
// edit back once it is published. But the apply did land on the cluster, and a window holds whole
// objects: the next edit of that object, by the same author, carries the apply's value for every
// field it did not touch, and its commit writes that value under the author's name. The edit the
// apply overwrote is lost, and the Repair puts the loss back on the cluster.
func TestApplyWindow_AWithheldApplyIsNotCommittedUnderTheNextEdit(t *testing.T) {
	if os.Getenv("GITOPS_REVERSER_KNOWN_BUGS") == "" {
		t.Skip("known bug, docs/TODO.md: a withheld apply rides the next edit")
	}

	// edit is alice's live update of y, holding both of its keys.
	edit := func(a, b string) Event {
		event := configMapTargetEvent("y", "alice", echoTarget)
		event.Operation = string(itypes.OperationUpdate)
		event.Object.Object["data"] = map[string]interface{}{"a": a, "b": b}
		return event
	}
	// lateApply is the applier writing y back to what the flushed commit holds, after the edit.
	lateApply := func() Event {
		event := edit("a1", "b1")
		event.UserInfo.Username = "system:serviceaccount:flux-system:kustomize-controller"
		event.LastFieldManagers = []string{FluxFieldManager}
		event.LastFieldManagedAt = time.Now().Add(time.Hour)
		return event
	}
	// flushOverAnEdit makes an apply due while x2 sits in the open window, and lets the flush publish
	// it only after alice's edit a=a2 is decided: the attempt goes for a commit that lacks the edit.
	flushOverAnEdit := func(t *testing.T, slug string) (*echoFixture, *recordingRequester) {
		f := newEchoFixture(t, slug, configv1alpha3.PruneOnEvent, map[string]string{"x": "x1"})
		f.writeWindow(edit("a1", "b1")) // y as Git holds it before anyone edits
		requester := f.withClaim()
		holdPushes(f.loop)

		f.writeWindow(echoEvent("x", "x2"))
		restore := failPushes(t, f.worker)
		f.echoApplier().repair = true
		f.loop.endWake(0)

		f.writeWindow(edit("a2", "b1")) // E1, decided after the mark
		restore()
		f.loop.retry.due = time.Now().Add(-time.Second)
		f.loop.runRetry()
		f.loop.endWake(0)
		require.Len(t, requester.taken(), 1, "the apply is sent without E1")
		return f, requester
	}

	t.Run("in the window that holds the edit", func(t *testing.T) {
		f, _ := flushOverAnEdit(t, "carry-same-window")
		require.NotNil(t, f.loop.openWindow, "E1 is collecting in alice's window")

		f.writeWindow(lateApply())
		require.True(t, f.echoApplier().repair, "the apply over E1 is withheld, and a Repair is pending")
		f.writeWindow(edit("a1", "b2")) // E2: alice changes b on the object the apply wrote
		f.loop.lastPushAt = time.Time{}
		require.True(t, f.loop.finalizeOpenWindow())
		f.loop.pushPending()

		require.Equal(t, "alice", gitIn(t, f.repoDir, "log", "-1", "--format=%an", "main"))
		assert.Contains(t, f.onMain(echoFile("y")), "a: a2",
			"E1 is alice's, and the apply that overwrote it was withheld; Git loses it")
	})

	t.Run("after the edit is published", func(t *testing.T) {
		f, _ := flushOverAnEdit(t, "carry-next-window")
		f.loop.lastPushAt = time.Time{}
		require.True(t, f.loop.finalizeOpenWindow())
		f.loop.pushPending()
		require.Contains(t, f.onMain(echoFile("y")), "a: a2", "precondition: E1 is published")

		f.writeWindow(lateApply())
		require.True(t, f.echoApplier().repair, "the apply over E1 is withheld, and a Repair is pending")
		f.writeWindow(edit("a1", "b2"))
		f.loop.lastPushAt = time.Time{}
		require.True(t, f.loop.finalizeOpenWindow())
		f.loop.pushPending()

		require.Equal(t, "alice", gitIn(t, f.repoDir, "log", "-1", "--format=%an", "main"))
		assert.Contains(t, f.onMain(echoFile("y")), "a: a2",
			"alice's commit of b reverts her own E1 to the withheld apply's a1")
	})
}
