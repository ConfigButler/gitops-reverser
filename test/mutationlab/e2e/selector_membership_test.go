//go:build mutationlab_e2e

// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/ConfigButler/gitops-reverser/internal/mutationlab"
)

// The actors of the selector-membership scenario. Alice builds the collection up; Bob takes objects
// out of it, once by relabeling and once by deleting; a controller clears a finalizer. Three real
// identities, so the audit events name who a production cluster would name.
const (
	membershipAlice      = "alice@example.com"
	membershipBob        = "bob@example.com"
	membershipCarol      = "carol@example.com"
	membershipController = "membership-finalizer-controller"
	membershipSelector   = "team=a"
	membershipSentinel   = "cm-sentinel"
)

// TestSelectorMembership captures what a label-selected collection sees, beside the unfiltered
// stream and the audit and admission records of the same writes. It is the evidence behind the
// objectSelector design (docs/design/watches-labels-simplification.md): the API server decides
// membership, a label change that stops matching is a collection removal even though the object
// still exists, and the only evidence that names who caused such a removal is the write at the
// removal's exact resourceVersion.
func TestSelectorMembership(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.newScenario(ctx, t, "selector-membership")

	alice := h.membershipActor(ctx, t, s, membershipAlice)
	bob := h.membershipActor(ctx, t, s, membershipBob)
	carol := h.membershipActor(ctx, t, s, membershipCarol)
	controller := h.asServiceAccount(ctx, t, s.ns, membershipController)

	start, err := h.kube.CoreV1().ConfigMaps(s.ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list the scenario namespace for a starting resourceVersion: %v", err)
	}
	// The probe replays from the starting resourceVersion, so it records every event of the
	// sequence below in order however late it connects.
	filteredDone := make(chan membershipProbe, 1)
	go func() {
		records, probeErr := h.tryProbeWatch(watchProbeRequest{
			Scenario:        s.id,
			Mode:            "collection",
			Resource:        configmapsResource,
			Namespace:       s.ns,
			LabelSelector:   membershipSelector,
			ResourceVersion: start.ResourceVersion,
			UntilName:       membershipSentinel,
		})
		filteredDone <- membershipProbe{records: records, err: probeErr}
	}()

	membershipSequence(ctx, t, s, membershipActors{alice: alice, bob: bob, carol: carol, controller: controller})

	probe := <-filteredDone
	if probe.err != nil {
		t.Fatalf("collection probe: %v", probe.err)
	}
	filtered := probe.records
	all := h.drain(t, s.id, drainSpec{
		minCount: len(filtered), settle: 3 * time.Second, timeout: 90 * time.Second,
		// A delete answered with a Status carries no labels, so its audit event is attributed to the
		// namespace rather than the scenario.
		alsoNamespace: s.ns,
		until: func(rs []mutationlab.Record) bool {
			return firstAuditNamed(rs, membershipSentinel) != nil && watchNamed(rs, membershipSentinel, "ADDED") != nil
		},
	})
	// Only this run's namespace: an earlier run's namespace is torn down asynchronously, and the
	// deletions that cleanup produces carry the same scenario label.
	rest := inNamespace(withoutRecords(all, filtered), s.ns)

	member, err := h.kube.CoreV1().ConfigMaps(s.ns).Get(ctx, "cm-member", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("cm-member must still exist after leaving the collection twice: %v", err)
	}

	committed := assertMembershipLaws(t, filtered, rest, string(member.UID))
	h.syncCorpus(t, "configmap/selector-membership", committed)
}

type membershipProbe struct {
	records []mutationlab.Record
	err     error
}

// membershipActor returns an impersonated user allowed to write ConfigMaps in the scenario.
func (h *harness) membershipActor(ctx context.Context, t *testing.T, s scenario, user string) kubernetes.Interface {
	t.Helper()
	h.allowConfigMapWrites(ctx, t, s.ns, "scenario-"+user[:3],
		rbacv1.Subject{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: user})
	return h.asActor(t, user, "system:authenticated")
}

// membershipActors are the identities the sequence writes as.
type membershipActors struct {
	alice, bob, carol, controller kubernetes.Interface
}

// membershipSequence performs the writes, in order. Each step names the membership transition it
// exists to capture.
func membershipSequence(ctx context.Context, t *testing.T, s scenario, actors membershipActors) {
	t.Helper()
	alice, bob, carol, controller := actors.alice, actors.bob, actors.carol, actors.controller
	cms := func(c kubernetes.Interface) corev1client.ConfigMapInterface { return c.CoreV1().ConfigMaps(s.ns) }
	labeled := func(name, team string, finalizers ...string) *corev1.ConfigMap {
		meta := s.meta(name)
		if team != "" {
			meta.Labels["team"] = team
		}
		meta.Finalizers = finalizers
		return &corev1.ConfigMap{ObjectMeta: meta, Data: map[string]string{"key": "v1"}}
	}
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	update := func(c kubernetes.Interface, name string, mutate func(*corev1.ConfigMap)) {
		t.Helper()
		cm, err := cms(c).Get(ctx, name, metav1.GetOptions{})
		must("get "+name, err)
		mutate(cm)
		_, err = cms(c).Update(ctx, cm, metav1.UpdateOptions{})
		must("update "+name, err)
	}
	patch := func(c kubernetes.Interface, name, body string) {
		t.Helper()
		_, err := cms(c).Patch(ctx, name, types.MergePatchType, []byte(body), metav1.PatchOptions{})
		must("patch "+name, err)
	}

	// Enters: created matching.
	_, err := cms(alice).Create(ctx, labeled("cm-member", "a"), metav1.CreateOptions{})
	must("create cm-member", err)
	// Never a member: created, and later updated, outside the selection.
	_, err = cms(alice).Create(ctx, labeled("cm-outsider", "b"), metav1.CreateOptions{})
	must("create cm-outsider", err)
	// Stays: an update that keeps the label.
	update(alice, "cm-member", func(cm *corev1.ConfigMap) { cm.Data["key"] = "v2" })
	// Outside: an update to an object the collection never held.
	update(alice, "cm-outsider", func(cm *corev1.ConfigMap) { cm.Data["key"] = "v2" })
	// Leaves by PUT: Bob relabels team=a to team=b.
	update(bob, "cm-member", func(cm *corev1.ConfigMap) { cm.Labels["team"] = "b" })
	// Re-enters by PATCH: Alice restores team=a. Same object, same uid.
	patch(alice, "cm-member", `{"metadata":{"labels":{"team":"a"}}}`)
	// Leaves by PATCH: Bob removes the label.
	patch(bob, "cm-member", `{"metadata":{"labels":{"team":null}}}`)
	// Leaves by deletion: created matching, deleted by Bob.
	_, err = cms(alice).Create(ctx, labeled("cm-gone", "a"), metav1.CreateOptions{})
	must("create cm-gone", err)
	must("delete cm-gone", cms(bob).Delete(ctx, "cm-gone", metav1.DeleteOptions{}))
	// Leaves through a finalizer: Alice asks for the deletion, the controller's patch removes it.
	_, err = cms(alice).Create(ctx, labeled("cm-held", "a", finalizerName), metav1.CreateOptions{})
	must("create cm-held", err)
	must("delete cm-held", cms(alice).Delete(ctx, "cm-held", metav1.DeleteOptions{}))
	patch(controller, "cm-held", `{"metadata":{"finalizers":null}}`)
	// Leaves by relabeling, THEN is deleted by someone else: Bob takes it out of the selection and
	// Carol deletes the same uid afterwards. The selected stream sees only the exit; Carol's delete
	// is a uid-only fact about the same object that did not cause that exit.
	_, err = cms(alice).Create(ctx, labeled("cm-exit-deleted", "a"), metav1.CreateOptions{})
	must("create cm-exit-deleted", err)
	patch(bob, "cm-exit-deleted", `{"metadata":{"labels":{"team":null}}}`)
	must("delete cm-exit-deleted", cms(carol).Delete(ctx, "cm-exit-deleted", metav1.DeleteOptions{}))
	// Ends the probe.
	_, err = cms(alice).Create(ctx, labeled(membershipSentinel, "a"), metav1.CreateOptions{})
	must("create the sentinel", err)
}

// assertMembershipLaws checks what the capture says and returns the records worth committing.
// Every law here is a claim the objectSelector design or its attribution policy rests on.
func assertMembershipLaws(
	t *testing.T, filtered, rest []mutationlab.Record, memberUID string,
) []mutationlab.Record {
	t.Helper()
	assertMembershipStreams(t, filtered, rest)
	assertExitsAndReentry(t, filtered, rest, memberUID)
	assertDeletionEvidence(t, filtered, rest)
	assertExitThenDeletion(t, filtered, rest)

	var committed []mutationlab.Record
	for _, name := range []string{"cm-member", "cm-outsider", "cm-gone", "cm-held", "cm-exit-deleted"} {
		committed = append(committed, markedFiltered(recordsNamed(filtered, name, mutationlab.SourceWatch))...)
		committed = append(committed, recordsNamed(rest, name, "")...)
	}
	return committed
}

// Law 1 — the API server decides membership. The selected stream sees entry and exit as ADDED and
// DELETED; the unfiltered stream sees the same writes as MODIFIED, and an object that never matched
// is invisible to the selected stream whatever happens to it.
func assertMembershipStreams(t *testing.T, filtered, rest []mutationlab.Record) {
	t.Helper()
	for name, want := range map[string][]string{
		"cm-member":   {"ADDED", "MODIFIED", "DELETED", "ADDED", "DELETED"},
		"cm-outsider": nil,
		"cm-gone":     {"ADDED", "DELETED"},
		"cm-held":     {"ADDED", "MODIFIED", "DELETED"},
		// The deletion after the exit is invisible to the selection: the object had already left.
		"cm-exit-deleted": {"ADDED", "DELETED"},
	} {
		if got := watchTypes(filtered, name); !slices.Equal(got, want) {
			t.Errorf("selected stream for %s = %v, want %v", name, got, want)
		}
	}
	for name, want := range map[string][]string{
		"cm-member":       {"ADDED", "MODIFIED", "MODIFIED", "MODIFIED", "MODIFIED"},
		"cm-outsider":     {"ADDED", "MODIFIED"},
		"cm-exit-deleted": {"ADDED", "MODIFIED", "DELETED"},
	} {
		if got := watchTypes(rest, name); !slices.Equal(got, want) {
			t.Errorf("unfiltered stream for %s = %v, want %v", name, got, want)
		}
	}
}

// Law 2 — leaving the collection is not deletion: every selected event about cm-member names one
// uid, the live object's, after both exits.
//
// Law 3 — an exit's DELETED carries the object as it was BEFORE the write (still matching) at the
// resourceVersion the write produced, which is the resourceVersion the write's own audit response
// carries. That exact (uid, resourceVersion) is the evidence of who took it out.
//
// Law 4 — re-entry is ADDED at the re-entering write's resourceVersion.
func assertExitsAndReentry(t *testing.T, filtered, rest []mutationlab.Record, memberUID string) {
	t.Helper()
	for _, r := range recordsNamed(filtered, "cm-member", mutationlab.SourceWatch) {
		if r.Key.UID != memberUID {
			t.Errorf("selected %s for cm-member carries uid %q, the live object %q",
				r.Summary.WatchType, r.Key.UID, memberUID)
		}
	}
	exits := watchesNamed(filtered, "cm-member", "DELETED")
	if len(exits) != 2 {
		t.Fatalf("want two exits for cm-member, got %d", len(exits))
	}
	for i, verb := range []string{"update", "patch"} {
		if team := watchLabel(t, &exits[i], "team"); team != "a" {
			t.Errorf("exit by %s carries team=%q; want the previous, still-matching team=a", verb, team)
		}
		write := auditBy(t, rest, "cm-member", verb, membershipBob)
		if write == nil {
			t.Fatalf("no audit %s by %s for cm-member", verb, membershipBob)
		}
		if rv := auditResponseResourceVersion(t, write); rv != exits[i].Key.ResourceVersion {
			t.Errorf("exit by %s: DELETED rv %q, the write's audit response rv %q",
				verb, exits[i].Key.ResourceVersion, rv)
		}
	}
	entries := watchesNamed(filtered, "cm-member", "ADDED")
	reentry := auditBy(t, rest, "cm-member", "patch", membershipAlice)
	if len(entries) != 2 || reentry == nil {
		t.Fatalf("want an entry, a re-entry, and the re-entering patch; got %d entries, patch %v",
			len(entries), reentry != nil)
	}
	if rv := auditResponseResourceVersion(t, reentry); rv != entries[1].Key.ResourceVersion {
		t.Errorf("re-entry ADDED rv %q, the patch's audit response rv %q", entries[1].Key.ResourceVersion, rv)
	}
}

// Law 5 — an immediate deletion is answered with a Status: it names the deleted uid in its details
// and carries NO resourceVersion. A deletion fact can therefore only be joined by uid, never at the
// DELETED's exact resourceVersion.
//
// Law 6 — a finalizer-held deletion's delete and the controller's finalizer patch carry the SAME
// resourceVersion, the one the deletion stamped and the selected stream's deletion-pending MODIFIED
// carries, and the final DELETED comes strictly after it. So nothing at the DELETED's exact
// resourceVersion names anyone, and the deletion fact that names the initiator precedes it.
func assertDeletionEvidence(t *testing.T, filtered, rest []mutationlab.Record) {
	t.Helper()
	gone := watchesNamed(filtered, "cm-gone", "DELETED")
	bobDelete := auditBy(t, rest, "cm-gone", "delete", membershipBob)
	if len(gone) != 1 || bobDelete == nil {
		t.Fatalf("want cm-gone's DELETED and Bob's delete; got %d and %v", len(gone), bobDelete != nil)
	}
	if rv := auditResponseResourceVersion(t, bobDelete); rv != "" {
		t.Errorf("an immediate delete's audit response carries rv %q; want none (a Status)", rv)
	}
	if uid := auditStatusUID(t, bobDelete); uid != gone[0].Key.UID {
		t.Errorf("the delete's Status names uid %q, the DELETED %q", uid, gone[0].Key.UID)
	}

	held := watchesNamed(filtered, "cm-held", "DELETED")
	pending := watchesNamed(filtered, "cm-held", "MODIFIED")
	controllerSA := fmt.Sprintf("system:serviceaccount:%s:%s", labActorNamespace, membershipController)
	aliceDelete := auditBy(t, rest, "cm-held", "delete", membershipAlice)
	finalizerPatch := auditBy(t, rest, "cm-held", "patch", controllerSA)
	if len(held) != 1 || len(pending) != 1 || aliceDelete == nil || finalizerPatch == nil {
		t.Fatalf("want cm-held's MODIFIED and DELETED, Alice's delete and the finalizer patch")
	}
	stamped := auditResponseResourceVersion(t, aliceDelete)
	if patched := auditResponseResourceVersion(t, finalizerPatch); patched != stamped {
		t.Errorf("the delete carries rv %q and the finalizer patch %q; want the same stamped rv", stamped, patched)
	}
	if pending[0].Key.ResourceVersion != stamped {
		t.Errorf("the deletion-pending MODIFIED carries rv %q; want the stamped %q",
			pending[0].Key.ResourceVersion, stamped)
	}
	if !rvBefore(t, stamped, held[0].Key.ResourceVersion) {
		t.Errorf("the final DELETED rv %q does not come after the stamped rv %q",
			held[0].Key.ResourceVersion, stamped)
	}
}

// Law 7 — a label exit followed by a deletion of the same uid by someone else. The selected stream
// holds ONE removal, the exit, at the relabeling patch's resourceVersion. The later delete is
// answered with a Status naming that same uid and no resourceVersion. A uid-only deletion fact
// therefore exists for an object whose selected removal it did not cause, which is why a filtered
// removal is attributed on the exact (uid, resourceVersion) write alone: accepting the uid-only fact
// names the deleter for the exit whenever the exit's own fact is late or missing.
func assertExitThenDeletion(t *testing.T, filtered, rest []mutationlab.Record) {
	t.Helper()
	exits := watchesNamed(filtered, "cm-exit-deleted", "DELETED")
	bobPatch := auditBy(t, rest, "cm-exit-deleted", "patch", membershipBob)
	carolDelete := auditBy(t, rest, "cm-exit-deleted", "delete", membershipCarol)
	if len(exits) != 1 || bobPatch == nil || carolDelete == nil {
		t.Fatalf("want cm-exit-deleted's one selected DELETED, Bob's patch and Carol's delete")
	}
	if rv := auditResponseResourceVersion(t, bobPatch); rv != exits[0].Key.ResourceVersion {
		t.Errorf("the exit's DELETED rv %q, Bob's patch rv %q", exits[0].Key.ResourceVersion, rv)
	}
	if rv := auditResponseResourceVersion(t, carolDelete); rv != "" {
		t.Errorf("Carol's delete carries rv %q; want a Status without one", rv)
	}
	if uid := auditStatusUID(t, carolDelete); uid != exits[0].Key.UID {
		t.Errorf("Carol's delete names uid %q; want the exited object's %q", uid, exits[0].Key.UID)
	}
}

func auditStatusUID(t *testing.T, r *mutationlab.Record) string {
	t.Helper()
	var event struct {
		ResponseObject *struct {
			Kind    string `json:"kind"`
			Details struct {
				UID string `json:"uid"`
			} `json:"details"`
		} `json:"responseObject"`
	}
	if err := json.Unmarshal(r.Raw, &event); err != nil {
		t.Fatalf("decode audit record %s: %v", r.ID, err)
	}
	if event.ResponseObject == nil || event.ResponseObject.Kind != "Status" {
		return ""
	}
	return event.ResponseObject.Details.UID
}

func rvBefore(t *testing.T, a, b string) bool {
	t.Helper()
	x, errA := strconv.ParseUint(a, 10, 64)
	y, errB := strconv.ParseUint(b, 10, 64)
	if errA != nil || errB != nil {
		t.Fatalf("resourceVersions %q and %q are not numbers", a, b)
	}
	return x < y
}

// markedFiltered renames the selected stream's records apart from the unfiltered ones, so both
// land in one corpus directory without the one hiding the other.
func markedFiltered(records []mutationlab.Record) []mutationlab.Record {
	out := make([]mutationlab.Record, 0, len(records))
	for _, r := range records {
		r.Summary.WatchType = "selected-" + r.Summary.WatchType
		out = append(out, r)
	}
	return out
}

func (h *harness) tryProbeWatch(req watchProbeRequest) ([]mutationlab.Record, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	resp, status, err := h.doJSON(http.MethodPost, "/watch-probe", body)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("status %d: %s", status, bytes.TrimSpace(resp))
	}
	var records []mutationlab.Record
	return records, json.Unmarshal(resp, &records)
}

func withoutRecords(all, drop []mutationlab.Record) []mutationlab.Record {
	ids := map[string]struct{}{}
	for _, r := range drop {
		ids[r.ID] = struct{}{}
	}
	var out []mutationlab.Record
	for _, r := range all {
		if _, dropped := ids[r.ID]; !dropped {
			out = append(out, r)
		}
	}
	return out
}

func inNamespace(records []mutationlab.Record, ns string) []mutationlab.Record {
	var out []mutationlab.Record
	for _, r := range records {
		if r.Key.Namespace == ns {
			out = append(out, r)
		}
	}
	return out
}

func recordsNamed(records []mutationlab.Record, name string, src mutationlab.Source) []mutationlab.Record {
	var out []mutationlab.Record
	for _, r := range records {
		if r.Key.Name == name && (src == "" || r.Source == src) {
			out = append(out, r)
		}
	}
	return out
}

func watchesNamed(records []mutationlab.Record, name, watchType string) []mutationlab.Record {
	var out []mutationlab.Record
	for _, r := range recordsNamed(records, name, mutationlab.SourceWatch) {
		if r.Summary.WatchType == watchType {
			out = append(out, r)
		}
	}
	return out
}

func watchNamed(records []mutationlab.Record, name, watchType string) *mutationlab.Record {
	if matches := watchesNamed(records, name, watchType); len(matches) > 0 {
		return &matches[0]
	}
	return nil
}

func watchTypes(records []mutationlab.Record, name string) []string {
	var out []string
	for _, r := range recordsNamed(records, name, mutationlab.SourceWatch) {
		out = append(out, r.Summary.WatchType)
	}
	return out
}

func auditBy(t *testing.T, records []mutationlab.Record, name, verb, actor string) *mutationlab.Record {
	t.Helper()
	for i := range records {
		r := &records[i]
		if r.Source == mutationlab.SourceAudit && r.Key.Name == name && r.Summary.Operation == verb &&
			auditActor(t, r) == actor {
			return r
		}
	}
	return nil
}

func watchLabel(t *testing.T, r *mutationlab.Record, key string) string {
	t.Helper()
	var env struct {
		Object struct {
			Metadata struct {
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
		} `json:"object"`
	}
	if err := json.Unmarshal(r.Raw, &env); err != nil {
		t.Fatalf("decode watch record %s: %v", r.ID, err)
	}
	return env.Object.Metadata.Labels[key]
}
