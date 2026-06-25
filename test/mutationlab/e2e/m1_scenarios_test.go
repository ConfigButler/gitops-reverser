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

	"github.com/ConfigButler/gitops-reverser/internal/mutationlab"
)

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
// write that never persisted. No watch object, no etcd object.
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

	records := h.drain(t, s.id, drainSpec{minCount: 1, settle: 3 * time.Second, timeout: 60 * time.Second})
	if got := countSource(records, mutationlab.SourceWatch); got != 0 {
		t.Errorf("rejected create produced %d watch events; want 0", got)
	}
	if got := countSource(records, mutationlab.SourceAdmission); got == 0 {
		t.Error("expected an admission record for the rejected write")
	}
	h.syncCorpus(t, "configmap/record-and-reject", records)
}

// rejectLabel mirrors recorder.RejectLabel; the driver stamps it to trigger the
// record-and-reject policy.
const rejectLabel = "mutationlab.configbutler.ai/reject"

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
