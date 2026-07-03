//go:build mutationlab_e2e

// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/ConfigButler/gitops-reverser/internal/mutationlab"
)

// ConfigMap scenarios — the core capture moments expressible against a built-in
// ConfigMap: create (Row 1), update (Row 2), server-side apply (Row 3), no-op
// apply (Row 4), finalizer delete (Row 8), deletecollection (Row 9), dry-run
// create (Row 11), and record-and-reject (Row 12).

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

// applyFieldManager owns the lab's server-side-apply writes. Using one stable
// manager across both applies in Rows 3/4 is what makes the no-op apply a true
// no-op (a different manager would add a managedFields entry and churn the rv).
const applyFieldManager = "mutationlab-apply"

// applyConfigMap server-side-applies a ConfigMap with the given data value under
// applyFieldManager. The body carries apiVersion/kind/name (required for apply)
// and the scenario label so the records attribute to the scenario.
func (h *harness) applyConfigMap(ctx context.Context, t *testing.T, s scenario, name, value string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      name,
			"namespace": s.ns,
			"labels":    map[string]string{scenarioLabel: s.id},
		},
		"data": map[string]string{"key": value},
	})
	if err != nil {
		t.Fatalf("marshal apply body: %v", err)
	}
	force := true
	if _, err := h.kube.CoreV1().ConfigMaps(s.ns).Patch(
		ctx, name, types.ApplyPatchType, body,
		metav1.PatchOptions{FieldManager: applyFieldManager, Force: &force}); err != nil {
		t.Fatalf("server-side apply %s=%s: %v", name, value, err)
	}
}

// TestServerSideApply captures Row 3, the dominant GitOps write path: Flux/Argo
// write by apply, so this pins what a server-side apply that *changes* an object
// looks like across the three mechanisms. The setup is itself an apply (same field
// manager), so the captured moment is a clean second apply: a watch MODIFIED, an
// audit record for the apply request, and an admission UPDATE carrying the apply
// options — with the apply field manager visible in managedFields.
func TestServerSideApply(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.newScenario(ctx, t, "server-side-apply")

	// First apply creates the object under applyFieldManager.
	h.applyConfigMap(ctx, t, s, "cm-a", "value")
	h.quiesceAndClear(t, s.id, 3)

	// A second apply that changes the value: MODIFIED + apply audit + admission UPDATE.
	h.applyConfigMap(ctx, t, s, "cm-a", "updated")
	records := h.drain(t, s.id, drainSpec{minCount: 3, settle: 2 * time.Second, timeout: 60 * time.Second})
	if got := countSource(records, mutationlab.SourceWatch); got == 0 {
		t.Error("server-side apply that changed the object produced no watch event; want a MODIFIED")
	}
	h.syncCorpus(t, "configmap/server-side-apply", records)
}

// TestNoOpApply captures Row 4, the load-bearing watch-mode caveat: re-applying
// identical content (same field manager, same values) leaves the resourceVersion
// unchanged, so it produces **no** watch event — yet the apply request still
// reaches audit (and admission). The watch silence is the finding: a naive watcher
// must not read "no event" as "nothing happened"; the apply did occur, it was just
// a no-op at storage. Only a periodic relist/reconcile would observe it.
func TestNoOpApply(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.newScenario(ctx, t, "no-op-apply")

	h.applyConfigMap(ctx, t, s, "cm-a", "value")
	h.quiesceAndClear(t, s.id, 3)

	// Re-apply the identical content. minCount 1 = at least the audit request; the
	// settle window then admits any admission record (recorded synchronously during
	// the request) without depending on whether a no-op apply reaches admission,
	// while still rejecting a stray watch event.
	h.applyConfigMap(ctx, t, s, "cm-a", "value")
	records := h.drain(t, s.id, drainSpec{minCount: 1, settle: 4 * time.Second, timeout: 60 * time.Second})
	if got := countSource(records, mutationlab.SourceWatch); got != 0 {
		t.Errorf("no-op apply produced %d watch events; want 0 (resourceVersion unchanged)", got)
	}
	if got := countSource(records, mutationlab.SourceAudit); got == 0 {
		t.Error("no-op apply produced no audit record; want one (the request is still logged)")
	}
	h.syncCorpus(t, "configmap/no-op-apply", records)
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
	} else if !apierrors.IsNotFound(err) {
		t.Fatalf("dry-run create lookup failed with %v; want NotFound", err)
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

// TestOptimisticConcurrencyConflict captures Row 13: an Update that loses the
// optimistic-concurrency race. The same "a write that never persisted produces no
// watch event" property as Rows 11/12, reached here through a `409 Conflict`
// rather than a dry-run or a rejecting webhook.
//
// The setup (create + a successful update) is quiesced and cleared, so the corpus
// is exactly the conflict moment. The successful update exists only to advance the
// object's resourceVersion past the copy the test still holds; that stale copy is
// then re-submitted:
//
//   - The apiserver rejects the stale update with `409 Conflict`. The
//     resourceVersion precondition is checked in the storage layer, BEFORE the
//     validating admission webhook runs — so (empirically, on k8s v1.35.2) the
//     conflict produces NO admission record at all. The response body is a
//     `Status`, but the requestObject still carries the submitted object, so the
//     event is audited as an `update` with responseStatus.code 409, attributed to
//     the user. Nothing persists, so there is NO watch event either.
//
// The finding for the watch-only proposal is stronger than Rows 11/12: for a
// storage-level conflict, audit is the SOLE witness — not even admission sees it,
// and watch never does. A watch-only state pipeline therefore cannot observe the
// phantom write at all; only the optional audit path records that it was attempted.
// The corpus commits the single 409 audit `update`, and asserts the absence of any
// watch and any admission record over the drain.
func TestOptimisticConcurrencyConflict(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.newScenario(ctx, t, "conflict-update")

	cm := &corev1.ConfigMap{ObjectMeta: s.meta("cm-conflict"), Data: map[string]string{"key": "value"}}
	created, err := h.kube.CoreV1().ConfigMaps(s.ns).Create(ctx, cm, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// stale keeps the create's resourceVersion. The successful update below bumps
	// the object's resourceVersion in the cluster, so `stale` (captured first) is
	// left behind and will lose the race.
	stale := created.DeepCopy()

	created.Data["key"] = "winner"
	if _, err := h.kube.CoreV1().ConfigMaps(s.ns).Update(ctx, created, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("setup update (should succeed): %v", err)
	}
	// Drain past create (3) + successful update (3) = 6 setup records, then clear,
	// so the corpus captures only the conflict that follows.
	h.quiesceAndClear(t, s.id, 6)

	// Re-submit the stale copy — its resourceVersion is now behind, so the apiserver
	// returns 409 Conflict.
	stale.Data["key"] = "loser"
	_, err = h.kube.CoreV1().ConfigMaps(s.ns).Update(ctx, stale, metav1.UpdateOptions{})
	if err == nil {
		t.Fatal("update over a stale resourceVersion was accepted; want a 409 Conflict")
	}
	if !apierrors.IsConflict(err) {
		t.Fatalf("stale update failed with %v; want a 409 Conflict", err)
	}

	// The sole witness is the audit `update` carrying responseStatus.code 409,
	// attributed to the user via the requestObject label. The audit event is async;
	// gate on it, and settle so a late admission record (if the apiserver ordering
	// ever changed) could not be missed and silently pass the admission==0 assertion.
	records := h.drain(t, s.id, drainSpec{
		minCount: 1, settle: 3 * time.Second, timeout: 90 * time.Second,
		until: func(rs []mutationlab.Record) bool { return countAuditCode(rs, 409) >= 1 },
	})

	// The failed write never persisted, so watch never saw it.
	if got := countSource(records, mutationlab.SourceWatch); got != 0 {
		t.Errorf("conflict produced %d watch events; want 0 (the failed update never persists)", got)
	}
	// Exactly one audit update, carrying the 409.
	if got := countSource(records, mutationlab.SourceAudit); got != 1 {
		t.Errorf("got %d audit records; want exactly 1 (the failed update is still audited)", got)
	}
	if got := countAuditCode(records, 409); got != 1 {
		t.Errorf("got %d audit records with code 409; want exactly 1 (the conflict)", got)
	}
	// The storage-layer conflict fires before validating admission, so the webhook
	// never sees the attempt: audit is the sole witness.
	if got := countSource(records, mutationlab.SourceAdmission); got != 0 {
		t.Errorf("got %d admission records; want 0 (the conflict is rejected before admission runs)", got)
	}

	conflict := mustRecord(t, firstAuditNamed(records, "cm-conflict"), "audit update carrying the 409")
	h.syncCorpus(t, "configmap/conflict-update", []mutationlab.Record{*conflict})
}

// countAuditCode counts audit records whose responseStatus.code equals want.
func countAuditCode(records []mutationlab.Record, want int32) int {
	n := 0
	for i := range records {
		if r := &records[i]; r.Source == mutationlab.SourceAudit && r.Summary.ResponseCode == want {
			n++
		}
	}
	return n
}

// TestOwnerRefCascade captures Row 10: deleting a parent object cascades through
// the garbage collector to its owner-referenced children. The user issues ONE
// delete (against the parent); the children disappear by a SECOND mechanism — the
// GC controller — under a SYSTEM identity, not the human's.
//
// A child ConfigMap carries an ownerReference to a parent ConfigMap (no controller
// is required — the GC acts on any ownerReference). Deleting the parent with
// Background propagation removes the parent immediately and the GC then deletes the
// child asynchronously.
//
// The two findings for the watch-only proposal:
//
//   - State: watch emits a DELETED for BOTH the parent and the cascaded child,
//     even though only one delete request was issued. This is the same fan-out
//     property as deletecollection (Row 9) but reached through GC — exactly what
//     Git deletes need, with no collection verb to decompose.
//   - Attribution: the parent delete is audited under the human user, but the
//     child delete is audited under the GC system actor
//     (system:serviceaccount:kube-system:generic-garbage-collector) — so a
//     cascaded delete's provenance is the system, not the user who deleted the
//     parent. The corpus commits that username verbatim.
func TestOwnerRefCascade(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.newScenario(ctx, t, "owner-ref-cascade")

	parent := &corev1.ConfigMap{ObjectMeta: s.meta("cm-parent"), Data: map[string]string{"key": "value"}}
	createdParent, err := h.kube.CoreV1().ConfigMaps(s.ns).Create(ctx, parent, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}

	childMeta := s.meta("cm-child")
	childMeta.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "v1",
		Kind:       "ConfigMap",
		Name:       createdParent.Name,
		UID:        createdParent.UID,
	}}
	child := &corev1.ConfigMap{ObjectMeta: childMeta, Data: map[string]string{"key": "value"}}
	if _, err := h.kube.CoreV1().ConfigMaps(s.ns).Create(ctx, child, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create child: %v", err)
	}
	// 2 creates × (admission + audit + watch) = 6 setup records.
	h.quiesceAndClear(t, s.id, 6)

	// Delete only the parent, with Background propagation: the parent is removed
	// immediately and the GC then deletes the owner-referenced child.
	bg := metav1.DeletePropagationBackground
	if err := h.kube.CoreV1().ConfigMaps(s.ns).Delete(
		ctx, "cm-parent", metav1.DeleteOptions{PropagationPolicy: &bg}); err != nil {
		t.Fatalf("delete parent: %v", err)
	}

	// Drain until BOTH the parent and the cascaded child DELETED have arrived, both
	// deletes are audited, and the stream is quiet — the child delete is
	// asynchronous (GC), so a plain count+settle gate could return after the parent
	// delete alone. A delete carries no object body, so both audit deletes attribute to
	// the namespace key, not the scenario id (scenarioFromAuditEvent) — union both.
	records := h.drain(t, s.id, drainSpec{
		minCount: 4, settle: 3 * time.Second, timeout: 90 * time.Second, alsoNamespace: s.ns,
		until: func(rs []mutationlab.Record) bool {
			return firstWatchDeletedNamed(rs, "cm-parent") != nil &&
				firstWatchDeletedNamed(rs, "cm-child") != nil &&
				firstAuditNamed(rs, "cm-parent") != nil &&
				firstAuditNamed(rs, "cm-child") != nil
		},
	})

	// One user delete, two watch DELETED events (parent + cascaded child).
	if got := countWatchType(records, "DELETED"); got != 2 {
		t.Errorf("got %d watch DELETED events; want 2 (parent + cascaded child)", got)
	}
	parentDel := mustRecord(t, firstWatchDeletedNamed(records, "cm-parent"),
		"watch DELETED for the parent (the user delete should be observed)")
	childDel := mustRecord(t, firstWatchDeletedNamed(records, "cm-child"),
		"watch DELETED for the child (the GC cascade should be observed by watch)")

	// Attribution: the parent delete is the human's; the child delete is the GC
	// system actor. Both deletes are audited (configmaps are kept by the policy).
	parentAudit := mustRecord(t, firstAuditNamed(records, "cm-parent"),
		"audit record for the parent delete")
	childAudit := mustRecord(t, firstAuditNamed(records, "cm-child"),
		"audit record for the child delete (the GC delete should still be audited)")
	if childAudit.Summary.User != gcUser {
		t.Errorf("child delete audited under %q; want the GC system actor %q",
			childAudit.Summary.User, gcUser)
	}
	if parentAudit.Summary.User == gcUser {
		t.Errorf("parent delete audited under the GC actor %q; want the human user", gcUser)
	}

	// Commit the four load-bearing moments explicitly: the two watch DELETED events
	// (state fan-out from one delete) and the two audit deletes (the human vs the GC
	// actor). Admission DELETEs may also fire per object, but the validating
	// webhook's involvement in a GC delete is timing-dependent, so it is asserted as
	// a law over the drain rather than committed as a flaky moment.
	h.syncCorpus(t, "configmap/owner-ref-cascade", []mutationlab.Record{
		*parentAudit, *childAudit, *parentDel, *childDel,
	})
}

// gcUser is the system identity Kubernetes' garbage collector acts under when it
// removes owner-referenced children. A cascaded delete is therefore attributable
// to the system, never to the human who deleted the parent.
const gcUser = "system:serviceaccount:kube-system:generic-garbage-collector"

// mustRecord fails the test when r is nil, otherwise returns it — collapsing the
// repeated nil-check + fatal so a multi-moment scenario stays under the
// complexity budget.
func mustRecord(t *testing.T, r *mutationlab.Record, what string) *mutationlab.Record {
	t.Helper()
	if r == nil {
		t.Fatalf("missing required moment: %s", what)
	}
	return r
}

// countWatchType counts watch records of the given event type.
func countWatchType(records []mutationlab.Record, watchType string) int {
	n := 0
	for i := range records {
		if r := &records[i]; r.Source == mutationlab.SourceWatch && r.Summary.WatchType == watchType {
			n++
		}
	}
	return n
}

// firstWatchDeletedNamed returns the first watch DELETED record whose object has
// the given name.
func firstWatchDeletedNamed(records []mutationlab.Record, name string) *mutationlab.Record {
	for i := range records {
		r := &records[i]
		if r.Source == mutationlab.SourceWatch && r.Summary.WatchType == "DELETED" && r.Key.Name == name {
			return r
		}
	}
	return nil
}

// firstAuditNamed returns the first audit record whose objectRef name matches.
func firstAuditNamed(records []mutationlab.Record, name string) *mutationlab.Record {
	for i := range records {
		if r := &records[i]; r.Source == mutationlab.SourceAudit && r.Key.Name == name {
			return r
		}
	}
	return nil
}
