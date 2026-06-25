//go:build mutationlab_e2e

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

package e2e

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/ConfigButler/gitops-reverser/internal/mutationlab"
)

// ConfigMap scenarios — the core capture moments expressible against a built-in
// ConfigMap: create (Row 1), update (Row 2), finalizer delete (Row 8),
// deletecollection (Row 9), dry-run create (Row 11), and record-and-reject
// (Row 12).

// quiesceAndClear drains the setup phase to a quiet state, then clears records, so
// the scenario that follows captures only the verb under test (not its create
// prerequisite).
func (h *harness) quiesceAndClear(t *testing.T, id string, setupMin int) {
	t.Helper()
	h.drain(t, id, drainSpec{minCount: setupMin, settle: 2 * time.Second, timeout: 60 * time.Second})
	h.clearRecords(t)
}

func countSource(records []mutationlab.Record, src mutationlab.Source) int {
	n := 0
	for _, r := range records {
		if r.Source == src {
			n++
		}
	}
	return n
}

// TestCreateSucceeds is the baseline anchor (Row 1) and the proof of the capture
// loop: capture -> normalize -> write -> diff on a single ConfigMap create. It
// expects three moments — a watch ADDED, an audit create, and an admission
// create — and commits them as corpus/configmap/create-succeeds/.
func TestCreateSucceeds(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.newScenario(ctx, t, "create-succeeds")

	cm := &corev1.ConfigMap{ObjectMeta: s.meta("cm-a"), Data: map[string]string{"key": "value"}}
	if _, err := h.kube.CoreV1().ConfigMaps(s.ns).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create configmap: %v", err)
	}

	records := h.drain(t, s.id, drainSpec{minCount: 3, settle: 2 * time.Second, timeout: 60 * time.Second})
	h.syncCorpus(t, "configmap/create-succeeds", records)
}

// TestUpdate captures Row 2: an Update (PUT) after a create. The create is set up
// and cleared, so the corpus is just the update moment — admission UPDATE, audit
// update, watch MODIFIED — with the verb that differs by request shape.
func TestUpdate(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.newScenario(ctx, t, "update")

	cm := &corev1.ConfigMap{ObjectMeta: s.meta("cm-a"), Data: map[string]string{"key": "value"}}
	created, err := h.kube.CoreV1().ConfigMaps(s.ns).Create(ctx, cm, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	h.quiesceAndClear(t, s.id, 3)

	created.Data["key"] = "updated"
	if _, err := h.kube.CoreV1().ConfigMaps(s.ns).Update(ctx, created, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update: %v", err)
	}
	records := h.drain(t, s.id, drainSpec{minCount: 3, settle: 2 * time.Second, timeout: 60 * time.Second})
	h.syncCorpus(t, "configmap/update", records)
}

// TestDryRunCreate captures Row 11: a dry-run create reaches admission and audit
// but produces no watch object and no etcd object — seen, never persisted.
func TestDryRunCreate(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.newScenario(ctx, t, "dry-run-create")

	cm := &corev1.ConfigMap{ObjectMeta: s.meta("cm-dry"), Data: map[string]string{"key": "value"}}
	opts := metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}
	if _, err := h.kube.CoreV1().ConfigMaps(s.ns).Create(ctx, cm, opts); err != nil {
		t.Fatalf("dry-run create: %v", err)
	}

	records := h.drain(t, s.id, drainSpec{minCount: 2, settle: 3 * time.Second, timeout: 60 * time.Second})
	if got := countSource(records, mutationlab.SourceWatch); got != 0 {
		t.Errorf("dry-run produced %d watch events; want 0 (nothing persisted)", got)
	}
	if _, err := h.kube.CoreV1().ConfigMaps(s.ns).Get(ctx, "cm-dry", metav1.GetOptions{}); err == nil {
		t.Error("dry-run create persisted an object; want none")
	}
	h.syncCorpus(t, "configmap/dry-run-create", records)
}

// TestRecordAndReject captures Row 12: the recorder is always called (parallel
// validation) and, with the reject label, record-and-rejects — so admission saw a
// write that never persisted. The failed create is still audited, so the corpus
// commits both an admission and an audit record; no watch object, no etcd object.
func TestRecordAndReject(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.newScenario(ctx, t, "record-and-reject")

	meta := s.meta("cm-reject")
	meta.Labels[rejectLabel] = "true"
	cm := &corev1.ConfigMap{ObjectMeta: meta, Data: map[string]string{"key": "value"}}
	_, err := h.kube.CoreV1().ConfigMaps(s.ns).Create(ctx, cm, metav1.CreateOptions{})
	if err == nil {
		t.Fatal("expected the create to be rejected by the lab admission recorder")
	}

	// minCount 2 = admission + the (failed) audit; this forces the drain to await
	// the async audit rather than returning after admission alone.
	records := h.drain(t, s.id, drainSpec{minCount: 2, settle: 3 * time.Second, timeout: 60 * time.Second})
	if got := countSource(records, mutationlab.SourceWatch); got != 0 {
		t.Errorf("rejected create produced %d watch events; want 0", got)
	}
	if got := countSource(records, mutationlab.SourceAdmission); got != 1 {
		t.Errorf("rejected create produced %d admission records; want exactly 1", got)
	}
	if got := countSource(records, mutationlab.SourceAudit); got != 1 {
		t.Errorf("rejected create produced %d audit records; want exactly 1 (the failed create is still audited)", got)
	}
	h.syncCorpus(t, "configmap/record-and-reject", records)
}

// rejectLabel mirrors recorder.RejectLabel; the driver stamps it to trigger the
// record-and-reject policy.
const rejectLabel = "mutationlab.configbutler.ai/reject"

// finalizerName is an inert custom finalizer no controller handles, so it blocks
// real removal until the test itself clears it — turning one logical delete into
// the two-phase removal Row 8 captures.
const finalizerName = "mutationlab.configbutler.ai/hold"

// TestFinalizerDelete captures Row 8, the sharpest "watch is the lone witness of
// state" case. A finalizer turns one logical delete into a two-phase removal whose
// terminal DELETED has NO audit `delete` behind it:
//
//   - Phase 1 (delete): the finalizer blocks real removal, so the apiserver only
//     stamps deletionTimestamp and the object lingers — a watch MODIFIED, an audit
//     `delete`, and an admission DELETE, but no DELETED yet.
//   - Phase 2 (finalizer removal): clearing the finalizer is a `patch` (audit) /
//     UPDATE (admission), NOT a delete — yet it is what actually makes the object
//     disappear (watch DELETED).
//
// So the disappearance is driven by a `patch`, and an audit-only deletion tracker
// watching for a `delete` verb would attribute it to the wrong request — or miss
// it entirely. The two phases are kept ordered by waiting for the deletion-pending
// MODIFIED before clearing the finalizer.
func TestFinalizerDelete(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.newScenario(ctx, t, "finalizer-delete")

	meta := s.meta("cm-hold")
	meta.Finalizers = []string{finalizerName}
	cm := &corev1.ConfigMap{ObjectMeta: meta, Data: map[string]string{"key": "value"}}
	if _, err := h.kube.CoreV1().ConfigMaps(s.ns).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	h.quiesceAndClear(t, s.id, 3)

	// Phase 1: delete. The finalizer keeps the object alive, so this stamps
	// deletionTimestamp rather than removing it. Wait until the deletion-pending
	// MODIFIED has been observed so phase 2 cannot reorder ahead of it.
	if err := h.kube.CoreV1().ConfigMaps(s.ns).Delete(ctx, "cm-hold", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	h.drain(t, s.id, drainSpec{
		minCount: 1, settle: 0, timeout: 60 * time.Second,
		until: func(rs []mutationlab.Record) bool { return firstDeletionPendingWatch(rs) != nil },
	})

	// Phase 2: remove the finalizer. A merge patch to null clears the slice; with
	// the last finalizer gone the apiserver completes the pending delete.
	patch := []byte(`{"metadata":{"finalizers":null}}`)
	if _, err := h.kube.CoreV1().ConfigMaps(s.ns).Patch(
		ctx, "cm-hold", types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		t.Fatalf("remove finalizer: %v", err)
	}

	// Drain until the terminal DELETED has arrived and the stream is quiet. The
	// six load-bearing moments are: watch MODIFIED + DELETED, audit delete + patch,
	// admission DELETE + UPDATE. An intermediate watch MODIFIED clearing the
	// finalizer can also appear; it is timing-dependent, so it is asserted as a law
	// over the drain rather than committed as a flaky moment.
	records := h.drain(t, s.id, drainSpec{
		minCount: 6, settle: 3 * time.Second, timeout: 90 * time.Second,
		until: func(rs []mutationlab.Record) bool { return firstWatch(rs, "DELETED") != nil },
	})

	// The terminal DELETED has no audit `delete` behind it: there is exactly one
	// audit delete (phase 1), and the removal verb is a patch.
	if got := countAuditVerb(records, "delete"); got != 1 {
		t.Errorf("finalizer delete produced %d audit `delete` events; want exactly 1 (only phase 1)", got)
	}
	if got := countAuditVerb(records, "patch"); got == 0 {
		t.Error("finalizer removal produced no audit `patch` event; want one (the real removal verb)")
	}

	pending := firstDeletionPendingWatch(records)
	deleted := firstWatch(records, "DELETED")
	admDelete := firstOp(records, mutationlab.SourceAdmission, "DELETE")
	admUpdate := firstOp(records, mutationlab.SourceAdmission, "UPDATE")
	auditDelete := firstOp(records, mutationlab.SourceAudit, "delete")
	auditPatch := firstOp(records, mutationlab.SourceAudit, "patch")
	for name, r := range map[string]*mutationlab.Record{
		"watch MODIFIED (deletion-pending)": pending,
		"watch DELETED (terminal)":          deleted,
		"admission DELETE":                  admDelete,
		"admission UPDATE":                  admUpdate,
		"audit delete":                      auditDelete,
		"audit patch":                       auditPatch,
	} {
		if r == nil {
			t.Fatalf("missing required moment: %s", name)
		}
	}
	if pending.Key.ResourceVersion >= deleted.Key.ResourceVersion {
		t.Errorf("deletion-pending rv %q should precede terminal DELETED rv %q",
			pending.Key.ResourceVersion, deleted.Key.ResourceVersion)
	}

	h.syncCorpus(t, "configmap/finalizer-delete", []mutationlab.Record{
		*admDelete, *admUpdate, *auditDelete, *auditPatch, *pending, *deleted,
	})
}

// countAuditVerb counts audit records whose verb (Operation) matches.
func countAuditVerb(records []mutationlab.Record, verb string) int {
	n := 0
	for i := range records {
		if r := &records[i]; r.Source == mutationlab.SourceAudit && r.Summary.Operation == verb {
			n++
		}
	}
	return n
}

// firstOp returns the first record from the given source whose Operation matches
// (admission operations are upper-case CREATE/UPDATE/DELETE; audit verbs are
// lower-case create/patch/delete).
func firstOp(records []mutationlab.Record, src mutationlab.Source, op string) *mutationlab.Record {
	for i := range records {
		if r := &records[i]; r.Source == src && r.Summary.Operation == op {
			return r
		}
	}
	return nil
}

// TestDeletecollection captures Row 9, the watch-mode pressure test: one request
// fans out into N per-object watch DELETED events and N per-object validating
// admission DELETEs (admission fires once per object, not once for the
// collection), while audit sees a single name-less deletecollection. The creates
// are set up and cleared first.
func TestDeletecollection(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.newScenario(ctx, t, "deletecollection")

	names := []string{"cm-a", "cm-b", "cm-c"}
	for _, name := range names {
		cm := &corev1.ConfigMap{ObjectMeta: s.meta(name), Data: map[string]string{"key": "value"}}
		if _, err := h.kube.CoreV1().ConfigMaps(s.ns).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	// 3 creates × (admission + audit + watch) = 9 setup records.
	h.quiesceAndClear(t, s.id, 9)

	selector := metav1.ListOptions{LabelSelector: scenarioLabel + "=" + s.id}
	if err := h.kube.CoreV1().ConfigMaps(s.ns).DeleteCollection(ctx, metav1.DeleteOptions{}, selector); err != nil {
		t.Fatalf("deletecollection: %v", err)
	}

	// N watch DELETED + one name-less audit deletecollection + N per-object
	// admission DELETE (validating admission fires once per object).
	records := h.drain(t, s.id, drainSpec{minCount: 2*len(names) + 1, settle: 3 * time.Second, timeout: 60 * time.Second})
	if got := countSource(records, mutationlab.SourceWatch); got != len(names) {
		t.Errorf("deletecollection produced %d watch deletes; want %d (per-object fan-out)", got, len(names))
	}
	if got := countSource(records, mutationlab.SourceAdmission); got != len(names) {
		t.Errorf("deletecollection: %d admission DELETEs, want %d (once per object, not per collection)",
			got, len(names))
	}
	h.syncCorpus(t, "configmap/deletecollection", records)
}
