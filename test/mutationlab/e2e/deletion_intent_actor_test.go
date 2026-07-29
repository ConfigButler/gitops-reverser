//go:build mutationlab_e2e

// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/ConfigButler/gitops-reverser/internal/mutationlab"
)

// The two actors this scenario drives one object with. They are the shape of the reported
// mis-attribution: a HUMAN asks for the deletion, and a CONTROLLER's cleanup write is what
// actually makes the object disappear. Both are real identities to the API server — the human
// through impersonation, the controller through a ServiceAccount token — so the audit events
// carry the actors a production cluster would carry, not a single admin twice.
const (
	deletionIntentHuman        = "alice@example.com"
	deletionIntentControllerSA = "finalizer-controller"
)

// deletionIntentHold is how long the controller waits, after the deletion is requested, before
// clearing the finalizer. It is the knob this scenario exists to turn.
//
// The API server batches audit deliveries (--audit-webhook-batch-max-wait, 1s in this cluster), so
// the hold decides whether the human's `delete` and the controller's `patch` arrive as ONE batch or
// as two. That is not a detail of the capture; it is the race itself, because the two facts they
// produce collide on one index key (see the resourceVersion law below), so which one a watch event
// joins depends on whether the second has landed yet.
func deletionIntentHold(t *testing.T, fallback time.Duration) time.Duration {
	t.Helper()
	raw := os.Getenv("LAB_FINALIZER_HOLD")
	if raw == "" {
		return fallback
	}
	hold, err := time.ParseDuration(raw)
	if err != nil {
		t.Fatalf("LAB_FINALIZER_HOLD=%q is not a duration: %v", raw, err)
	}
	return hold
}

// TestDeletionIntentActorRace captures what Kubernetes emits when a human deletes a finalized
// object and a controller clears the finalizer — the shape behind "the deletion is attributed to
// the controller that cleaned up, not to the person who asked for it".
//
// Row 8 (finalizer-delete) already established the two-phase removal with one actor. This scenario
// adds the two things that turn that shape into a mis-attribution: a SECOND actor on the second
// phase, and a controllable hold between them. Three objects are driven with three holds, because
// the hold is what decides whether both audit events reach the operator in one batch.
//
// The law worth reading twice is the resourceVersion one. Both audit events carry a response body,
// and both response bodies carry the SAME resourceVersion — the one the deletion stamped. So the
// two facts they produce are keyed identically, and the later one (the controller's) simply
// replaces the earlier one (the human's) in the index. It is not that the human's evidence ranks
// lower; it is that it is no longer there.
func TestDeletionIntentActorRace(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.newScenario(ctx, t, "deletion-intent-actor")

	human := h.asActor(t, deletionIntentHuman, "system:authenticated")
	h.allowConfigMapWrites(ctx, t, s.ns, "scenario-human",
		rbacv1.Subject{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: deletionIntentHuman})
	controller := h.asServiceAccount(ctx, t, s.ns, deletionIntentControllerSA)

	// Three holds: none (the controller is already cleaning up when the delete lands), a hold
	// longer than one audit batch window, and a hold long enough that no batching could merge them.
	cases := []struct {
		name string
		hold time.Duration
	}{
		{name: "cm-hold-none", hold: deletionIntentHold(t, 0)},
		{name: "cm-hold-short", hold: deletionIntentHold(t, 3*time.Second)},
		{name: "cm-hold-long", hold: deletionIntentHold(t, 8*time.Second)},
	}

	var committed []mutationlab.Record
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			records := h.driveDeletionIntentRace(ctx, t, s, tc.name, tc.hold, human, controller)
			moments := assertDeletionIntentLaws(t, records, tc.name)
			if tc.name == "cm-hold-short" {
				committed = moments
			}
		})
	}

	// One object's moments are committed. The other two exercise the same laws under a different
	// hold, which is a timing property rather than a shape one: committing all three would put the
	// same six files in the corpus three times and add nothing a reader could not infer.
	if len(committed) == 0 {
		t.Fatal("no moments captured for the committed hold")
	}
	h.syncCorpus(t, "configmap/deletion-intent-actor", committed)
}

// driveDeletionIntentRace runs one object through the two-actor removal and returns the records it
// produced. The phases are kept ordered by waiting for the deletion-pending MODIFIED before the
// controller writes, so the capture measures the hold rather than a scheduling accident.
func (h *harness) driveDeletionIntentRace(
	ctx context.Context,
	t *testing.T,
	s scenario,
	name string,
	hold time.Duration,
	human, controller kubernetes.Interface,
) []mutationlab.Record {
	t.Helper()

	meta := s.meta(name)
	meta.Finalizers = []string{finalizerName}
	cm := &corev1.ConfigMap{ObjectMeta: meta, Data: map[string]string{"key": "value"}}
	if _, err := human.CoreV1().ConfigMaps(s.ns).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create %s as the human: %v", name, err)
	}
	h.quiesceAndClear(t, s.id, 3)

	// Phase 1, the HUMAN: the finalizer blocks real removal, so this only stamps deletionTimestamp.
	if err := human.CoreV1().ConfigMaps(s.ns).Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete %s as the human: %v", name, err)
	}
	h.drain(t, s.id, drainSpec{
		minCount: 1, settle: 0, timeout: 60 * time.Second,
		until: func(rs []mutationlab.Record) bool { return firstDeletionPendingWatch(rs) != nil },
	})

	// The hold. This is the whole knob: it decides whether the controller's write shares an audit
	// batch with the human's delete.
	time.Sleep(hold)

	// Phase 2, the CONTROLLER: clearing the last finalizer is a `patch`, and it is what makes the
	// object disappear.
	patch := []byte(`{"metadata":{"finalizers":null}}`)
	if _, err := controller.CoreV1().ConfigMaps(s.ns).Patch(
		ctx, name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		t.Fatalf("clear finalizer on %s as the controller: %v", name, err)
	}

	return h.drain(t, s.id, drainSpec{
		minCount: 6, settle: 3 * time.Second, timeout: 90 * time.Second,
		until: func(rs []mutationlab.Record) bool { return firstWatch(rs, "DELETED") != nil },
	})
}

// assertDeletionIntentLaws checks what the capture says about the two-actor removal and returns the
// moments worth committing. Every check here is a claim the design note rests on.
func assertDeletionIntentLaws(t *testing.T, records []mutationlab.Record, name string) []mutationlab.Record {
	t.Helper()

	auditDelete := firstOp(records, mutationlab.SourceAudit, "delete")
	auditPatch := firstOp(records, mutationlab.SourceAudit, "patch")
	pending := firstDeletionPendingWatch(records)
	deleted := firstWatch(records, "DELETED")
	admDelete := firstOp(records, mutationlab.SourceAdmission, "DELETE")
	admUpdate := firstOp(records, mutationlab.SourceAdmission, "UPDATE")
	for label, r := range map[string]*mutationlab.Record{
		"audit delete":                      auditDelete,
		"audit patch":                       auditPatch,
		"watch MODIFIED (deletion-pending)": pending,
		"watch DELETED (terminal)":          deleted,
		"admission DELETE":                  admDelete,
		"admission UPDATE":                  admUpdate,
	} {
		if r == nil {
			t.Fatalf("%s: missing required moment: %s", name, label)
		}
	}

	// Law 1 — two actors, one object. The delete is the human's; the write that removes the object
	// is the controller's. Impersonation puts the human in impersonatedUser, which is the field the
	// product reads in preference to user.
	if got := auditActor(t, auditDelete); got != deletionIntentHuman {
		t.Errorf("%s: the delete names %q; want the human %q", name, got, deletionIntentHuman)
	}
	wantSA := fmt.Sprintf("system:serviceaccount:%s:%s", labActorNamespace, deletionIntentControllerSA)
	if got := auditActor(t, auditPatch); got != wantSA {
		t.Errorf("%s: the finalizer patch names %q; want the controller %q", name, got, wantSA)
	}

	// Law 2 — the removal verb is a patch. There is exactly one audit `delete`, and it belongs to
	// phase 1; nothing audits the disappearance itself.
	if got := countAuditVerb(records, "delete"); got != 1 {
		t.Errorf("%s: %d audit `delete` events; want exactly 1 (phase 1 only)", name, got)
	}

	// Law 3 — THE FINDING. Both audit events carry a response body, and both bodies carry the SAME
	// resourceVersion: the one the deletion stamped. The facts they produce therefore share one
	// (uid, resourceVersion) key, and the join has no way to prefer the earlier one, because the
	// later one has taken its place.
	deleteRV := auditResponseResourceVersion(t, auditDelete)
	patchRV := auditResponseResourceVersion(t, auditPatch)
	if deleteRV == "" || patchRV == "" {
		t.Fatalf("%s: an audit event carried no response resourceVersion (delete=%q patch=%q)",
			name, deleteRV, patchRV)
	}
	if deleteRV != patchRV {
		t.Errorf("%s: the two audit events carry different resourceVersions (delete=%q patch=%q); "+
			"the key collision this scenario measures does not hold", name, deleteRV, patchRV)
	}

	// Law 4 — and that shared key is exactly what the watch event joins on. The deletion-pending
	// MODIFIED is the observation the deletion-as-intent rule renders as a DELETE, and it carries
	// the same resourceVersion both facts were filed under.
	if pending.Key.ResourceVersion != deleteRV {
		t.Errorf("%s: the deletion-pending watch event carries rv %q, the audit facts %q; "+
			"the transition event does not join the colliding key", name, pending.Key.ResourceVersion, deleteRV)
	}
	if pending.Key.ResourceVersion >= deleted.Key.ResourceVersion {
		t.Errorf("%s: deletion-pending rv %q should precede terminal DELETED rv %q",
			name, pending.Key.ResourceVersion, deleted.Key.ResourceVersion)
	}

	return []mutationlab.Record{*admDelete, *admUpdate, *auditDelete, *auditPatch, *pending, *deleted}
}

// auditActor returns the actor an audit record names, preferring the impersonated subject over the
// impersonator — the same preference the product's resolveUserInfo applies when it builds a fact.
func auditActor(t *testing.T, r *mutationlab.Record) string {
	t.Helper()
	var event struct {
		User struct {
			Username string `json:"username"`
		} `json:"user"`
		ImpersonatedUser *struct {
			Username string `json:"username"`
		} `json:"impersonatedUser"`
	}
	if err := json.Unmarshal(r.Raw, &event); err != nil {
		t.Fatalf("decode audit record %s: %v", r.ID, err)
	}
	if event.ImpersonatedUser != nil && event.ImpersonatedUser.Username != "" {
		return event.ImpersonatedUser.Username
	}
	return event.User.Username
}

// auditResponseResourceVersion reads the resourceVersion out of an audit event's response body,
// which is where the product reads it from too (resourceVersionFromEvent prefers the response
// object, because objectRef.resourceVersion is the precondition rather than the result).
func auditResponseResourceVersion(t *testing.T, r *mutationlab.Record) string {
	t.Helper()
	var event struct {
		ResponseObject *struct {
			Metadata struct {
				ResourceVersion string `json:"resourceVersion"`
			} `json:"metadata"`
		} `json:"responseObject"`
	}
	if err := json.Unmarshal(r.Raw, &event); err != nil {
		t.Fatalf("decode audit record %s: %v", r.ID, err)
	}
	if event.ResponseObject == nil {
		return ""
	}
	return event.ResponseObject.Metadata.ResourceVersion
}
