// SPDX-License-Identifier: Apache-2.0

package git

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// carryEdit is author's live update of y, holding both of its keys.
func carryEdit(author, a, b string) Event {
	event := configMapTargetEvent("y", author, echoTarget)
	event.Operation = string(itypes.OperationUpdate)
	event.Object.Object["data"] = map[string]interface{}{"a": a, "b": b}
	return event
}

// carryApply is the applier writing y, after every edit so far.
func carryApply(a, b string) Event {
	event := carryEdit("system:serviceaccount:flux-system:kustomize-controller", a, b)
	event.LastFieldManagers = []string{FluxFieldManager}
	event.LastFieldManagedAt = time.Now().Add(time.Hour)
	return event
}

// carryFlushOverAnEdit makes an apply due while x2 sits in the open window, and lets the flush
// publish it only after alice's edit a=a2 (E1) is decided: the attempt goes for a commit that lacks
// the edit, which Git holds as a1.
func carryFlushOverAnEdit(t *testing.T, slug string) *echoFixture {
	t.Helper()
	f := newEchoFixture(t, slug, configv1alpha3.PruneOnEvent, map[string]string{"x": "x1"})
	f.writeWindow(carryEdit("alice", "a1", "b1")) // y as Git holds it before anyone edits
	requester := f.withClaim()
	holdPushes(f.loop)

	f.writeWindow(echoEvent("x", "x2"))
	restore := failPushes(t, f.worker)
	f.echoApplier().repair = true
	f.loop.endWake(0)

	f.writeWindow(carryEdit("alice", "a2", "b1")) // E1, decided after the mark
	restore()
	f.loop.retry.due = time.Now().Add(-time.Second)
	f.loop.runRetry()
	f.loop.endWake(0)
	require.Len(t, requester.taken(), 1, "the apply is sent without E1")
	return f
}

// publish closes the open window and pushes it.
func (f *echoFixture) publish() {
	f.t.Helper()
	f.loop.lastPushAt = time.Time{}
	require.True(f.t, f.loop.finalizeOpenWindow())
	f.loop.pushPending()
}

// carryWithheld publishes E1, then has the apply land over it, late: the apply is withheld, a Repair
// is pending, and the cluster runs a1 while Git holds a2.
func carryWithheld(t *testing.T, slug string) *echoFixture {
	t.Helper()
	f := carryFlushOverAnEdit(t, slug)
	f.publish()
	require.Contains(t, f.onMain(echoFile("y")), "a: a2", "precondition: E1 is published")

	f.writeWindow(carryApply("a1", "b1"))
	require.True(t, f.echoApplier().repair, "the apply over E1 is withheld, and a Repair is pending")
	require.Len(t, f.loop.withheld[carryKey()].Fields, 1, "the field the apply reverted is recorded")
	return f
}

// carryKey is y's objectKey.
func carryKey() string { return objectKey(carryEdit("alice", "", "").Identifier) }

// TestApplyWindow_AWithheldApplyIsNotCommittedUnderTheNextEdit pins the withheld-field record
// (withheld_fields.go).
//
// S1 withholds an apply that lands over an edit decided after the flush mark, and a Repair puts the
// edit back once it is published. But the apply did land on the cluster, and a window holds whole
// objects: the next edit of that object, by the same author, carries the apply's value for every
// field it did not touch. Committed as it came, it wrote that value under the author's name, the
// edit the apply overwrote was lost, and the Repair put the loss back on the cluster.
func TestApplyWindow_AWithheldApplyIsNotCommittedUnderTheNextEdit(t *testing.T) {
	t.Run("in the window that holds the edit", func(t *testing.T) {
		f := carryFlushOverAnEdit(t, "carry-same-window")
		require.NotNil(t, f.loop.windows.only(), "E1 is collecting in alice's window")

		f.writeWindow(carryApply("a1", "b1"))
		require.True(t, f.echoApplier().repair, "the apply over E1 is withheld, and a Repair is pending")
		f.writeWindow(carryEdit("alice", "a1", "b2")) // E2: alice changes b on the object the apply wrote
		f.publish()

		require.Equal(t, "alice", gitIn(t, f.repoDir, "log", "-1", "--format=%an", "main"))
		assert.Contains(t, f.onMain(echoFile("y")), "a: a2",
			"E1 is alice's, and the apply that overwrote it was withheld; Git keeps it")
		assert.Contains(t, f.onMain(echoFile("y")), "b: b2")
	})

	t.Run("after the edit is published", func(t *testing.T) {
		f := carryWithheld(t, "carry-next-window")
		f.writeWindow(carryEdit("alice", "a1", "b2"))
		f.publish()

		require.Equal(t, "alice", gitIn(t, f.repoDir, "log", "-1", "--format=%an", "main"))
		assert.Contains(t, f.onMain(echoFile("y")), "a: a2",
			"alice's commit of b keeps her own E1, not the withheld apply's a1")
		assert.Contains(t, f.onMain(echoFile("y")), "b: b2")
		assert.Len(t, f.loop.withheld[carryKey()].Fields, 1, "the cluster still runs a1: the record stays")
	})

	t.Run("an edit of another author does not carry it either", func(t *testing.T) {
		f := carryWithheld(t, "carry-other-author")
		f.writeWindow(carryEdit("bob", "a1", "b2"))
		f.publish()

		require.Equal(t, "bob", gitIn(t, f.repoDir, "log", "-1", "--format=%an", "main"))
		assert.Contains(t, f.onMain(echoFile("y")), "a: a2", "bob changed b only")
		assert.Contains(t, f.onMain(echoFile("y")), "b: b2")
	})
}

// TestApplyWindow_TheWithheldRecordEnds pins when a field leaves the record: when an event shows it
// holding another value than the apply's, and when the object is deleted.
func TestApplyWindow_TheWithheldRecordEnds(t *testing.T) {
	t.Run("an author who changes the field wins", func(t *testing.T) {
		f := carryWithheld(t, "carry-author-wins")
		f.writeWindow(carryEdit("alice", "a3", "b1")) // E2: alice changes a herself
		f.publish()

		assert.Contains(t, f.onMain(echoFile("y")), "a: a3", "her value is not the apply's: it is hers")
		assert.Empty(t, f.loop.withheld, "the field left the record")
	})

	t.Run("the Repair landing clears it", func(t *testing.T) {
		f := carryWithheld(t, "carry-repair-lands")
		f.writeWindow(carryApply("a2", "b1")) // the Repair puts E1 back on the cluster
		assert.Empty(t, f.loop.withheld, "the cluster runs the accepted value again")

		f.writeWindow(carryEdit("alice", "a1", "b1")) // alice sets a1 herself, now
		f.publish()
		assert.Contains(t, f.onMain(echoFile("y")), "a: a1", "an edit after the Repair is not set back")
	})

	t.Run("a delete clears it", func(t *testing.T) {
		f := carryWithheld(t, "carry-delete")
		deleted := carryEdit("alice", "a1", "b1")
		deleted.Operation = string(itypes.OperationDelete)
		f.writeWindow(deleted)
		assert.Empty(t, f.loop.withheld)

		created := carryEdit("alice", "a1", "b2")
		created.Operation = string(itypes.OperationCreate)
		f.writeWindow(created)
		f.publish()
		assert.Contains(t, f.onMain(echoFile("y")), "a: a1", "the object made again is mirrored as it is")
	})
}

// carryDeleteThenRecreate publishes alice's delete of y inside the open apply window, then has the
// applier make y again, late: the apply is withheld, a Repair is pending, and the cluster runs y while
// Git holds none.
func carryDeleteThenRecreate(t *testing.T, slug string) *echoFixture {
	t.Helper()
	f := carryFlushOverAnEdit(t, slug)
	f.publish()
	deleted := carryEdit("alice", "a2", "b1")
	deleted.Operation = string(itypes.OperationDelete)
	f.writeWindow(deleted)
	f.publish()
	require.Empty(t, f.onMain(echoFile("y")), "precondition: alice's delete is published")

	recreated := carryApply("a1", "b1")
	recreated.Operation = string(itypes.OperationCreate)
	f.writeWindow(recreated)
	require.True(t, f.echoApplier().repair, "the apply that made y again is withheld, and a Repair is pending")
	return f
}

// carryLabel is bob's kubectl label on y as the applier made it again: the whole object it carries
// is the apply's.
func carryLabel() Event {
	event := carryApply("a1", "b1")
	event.UserInfo.Username = "bob"
	event.LastFieldManagers = []string{"kubectl-label"}
	event.LastFieldManagedAt = time.Now().Add(2 * time.Hour)
	event.Object.SetLabels(map[string]string{"team": "a"})
	return event
}

// publishAny closes the open windows, if any, and pushes what is pending.
func (f *echoFixture) publishAny() {
	f.t.Helper()
	f.loop.lastPushAt = time.Time{}
	f.loop.finalizeOpenWindow()
	f.loop.pushPending()
}

// TestApplyWindow_ADeleteInTheWindowStaysDeleted pins a delete as the last accepted version: an
// apply that makes the object again, late, over the delete is withheld, and until the Repair's own
// delete lands no later event of the object, by anyone, brings it back into Git.
func TestApplyWindow_ADeleteInTheWindowStaysDeleted(t *testing.T) {
	t.Run("a later edit does not bring it back", func(t *testing.T) {
		f := carryDeleteThenRecreate(t, "carry-delete-label")
		f.writeWindow(carryLabel())
		f.publishAny()
		assert.Empty(t, f.onMain(echoFile("y")),
			"bob labelled the object the withheld apply made; committing it would undo alice's delete")
		require.Len(t, f.loop.withheld[carryKey()].Fields, 1, "the record holds the object out")

		repaired := carryApply("a1", "b1")
		repaired.Operation = string(itypes.OperationDelete)
		f.writeWindow(repaired) // the Repair deletes y on the cluster
		f.publishAny()
		assert.Empty(t, f.loop.withheld, "the Repair's delete clears the record")
		assert.Empty(t, f.onMain(echoFile("y")))

		created := carryEdit("bob", "a9", "b9")
		created.Operation = string(itypes.OperationCreate)
		f.writeWindow(created)
		f.publishAny()
		assert.Contains(t, f.onMain(echoFile("y")), "a: a9", "an object made after the Repair is mirrored")
	})

	t.Run("a resync does not bring it back", func(t *testing.T) {
		f := carryDeleteThenRecreate(t, "carry-delete-resync")
		snapshot := carryLabel()
		req := &ResyncRequest{
			Desired: []manifestanalyzer.DesiredResource{
				{Resource: snapshot.Identifier, Object: snapshot.Object},
				echoDesired("x", "x2"),
			},
			GitTargetName:      echoTarget,
			GitTargetNamespace: "default",
			Initial:            true,
			Result:             make(chan ResyncResult, 1),
		}
		f.loop.handleQueueItem(resyncItem(req))
		require.NoError(t, (<-req.Result).Err)
		f.publishAny()

		assert.Empty(t, f.onMain(echoFile("y")), "the snapshot holds y only because the apply made it again")
		assert.Len(t, req.Desired, 2, "the snapshot itself is left as it was")
	})
}

// TestApplyWindow_AResyncAfterAWithholdKeepsTheEdit pins the record on a resync: its snapshot holds
// what the cluster runs, the withheld apply's a1 included, and is set back as a live event is.
func TestApplyWindow_AResyncAfterAWithholdKeepsTheEdit(t *testing.T) {
	f := carryWithheld(t, "carry-resync")
	snapshot := carryEdit("alice", "a1", "b2")
	req := &ResyncRequest{
		Desired: []manifestanalyzer.DesiredResource{
			{Resource: snapshot.Identifier, Object: snapshot.Object},
			echoDesired("x", "x2"),
		},
		GitTargetName:      echoTarget,
		GitTargetNamespace: "default",
		// A snapshot F3 lets through while the Repair is pending.
		Initial: true,
		Result:  make(chan ResyncResult, 1),
	}
	f.loop.handleQueueItem(resyncItem(req))
	require.NoError(t, (<-req.Result).Err)
	f.loop.lastPushAt = time.Time{}
	f.loop.pushPending()

	assert.Contains(t, f.onMain(echoFile("y")), "a: a2", "the resync keeps E1")
	assert.Contains(t, f.onMain(echoFile("y")), "b: b2", "and mirrors the rest of the snapshot")
	assert.Equal(t, "a1", snapshot.Object.Object["data"].(map[string]interface{})["a"],
		"the snapshot itself is left as it was")
}

// TestWithheldFields_RecordAndRestore pins the record below the worker: the fields recorded are
// where the withheld object differs from the accepted one, a map key by key and a list as one field,
// with absence on either side; a later object is restored where it still holds the apply's value.
func TestWithheldFields_RecordAndRestore(t *testing.T) {
	loop := &branchWorkerEventLoop{w: &BranchWorker{}}
	accepted := carryEdit("alice", "", "")
	accepted.Object.Object["data"] = map[string]interface{}{"a": "a2", "c": "c1"}
	accepted.Object.Object["spec"] = map[string]interface{}{"list": []interface{}{"one", "two"}}
	loop.keepAccepted(accepted)

	withheld := carryApply("", "")
	withheld.Object.Object["data"] = map[string]interface{}{"a": "a1"}
	withheld.Object.Object["spec"] = map[string]interface{}{"list": []interface{}{"one"}}
	withheld.Object.Object["extra"] = "x"
	loop.recordWithheld(withheld)

	key := carryKey()
	assert.Equal(t, []withheldField{
		{Path: []string{"data", "a"}, Accepted: fieldValue{Value: "a2"}, Applier: fieldValue{Value: "a1"}},
		{Path: []string{"data", "c"}, Accepted: fieldValue{Value: "c1"}, Applier: fieldValue{Absent: true}},
		{Path: []string{"extra"}, Accepted: fieldValue{Absent: true}, Applier: fieldValue{Value: "x"}},
		{
			Path:     []string{"spec", "list"},
			Accepted: fieldValue{Value: []interface{}{"one", "two"}},
			Applier:  fieldValue{Value: []interface{}{"one"}},
		},
	}, loop.withheld[key].Fields)

	// A later object: b added, data.a carried, data.c still missing, extra changed by someone, the
	// list carried.
	later := withheld.Object.DeepCopy()
	later.Object["data"].(map[string]interface{})["b"] = "b2"
	later.Object["extra"] = "y"
	restored := loop.restoreWithheld(key, later)

	require.NotSame(t, later, restored, "the object set back is a copy")
	assert.Equal(t, map[string]interface{}{"a": "a2", "b": "b2", "c": "c1"}, restored.Object["data"])
	assert.Equal(t, "y", restored.Object["extra"], "a field someone changed is theirs")
	assert.Equal(t, []interface{}{"one", "two"}, restored.Object["spec"].(map[string]interface{})["list"])
	assert.Len(t, loop.withheld[key].Fields, 3, "extra left the record")

	// A second withhold, over a version without data at all, replaces the fields inside it.
	loop.keepAccepted(Event{Identifier: accepted.Identifier, Object: restored})
	again := carryApply("", "")
	delete(again.Object.Object, "data")
	again.Object.Object["spec"] = map[string]interface{}{"list": []interface{}{"one"}}
	again.Object.Object["extra"] = "y"
	loop.recordWithheld(again)
	paths := make([][]string, 0, len(loop.withheld[key].Fields))
	for _, field := range loop.withheld[key].Fields {
		paths = append(paths, field.Path)
	}
	assert.ElementsMatch(t, [][]string{{"data"}, {"spec", "list"}}, paths)

	// An object that changed nothing the record holds is returned as it is, and ends the record.
	unrelated := &unstructured.Unstructured{Object: map[string]interface{}{
		"data": map[string]interface{}{"a": "a9"},
		"spec": map[string]interface{}{"list": []interface{}{"three"}},
	}}
	assert.Same(t, unrelated, loop.restoreWithheld(key, unrelated))
	assert.Empty(t, loop.withheld)
}
