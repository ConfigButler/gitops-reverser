//go:build mutationlab_e2e

// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/mutationlab"
)

// Aggregated API write (Row 15) — the body-quality cliff, and the product
// question the lab exists to settle: when the official kube-apiserver audit body for an
// aggregated-API write is shallow/empty (the apiserver proxies the request and
// has no schema to render request/response objects), does the *watch* still carry
// the full object? It does — and that finding is what retired the aggregated-API
// body-enrichment proxy: a watch-based capture carries the object content natively,
// so there is no separate enriched audit body to join.
//
// The vehicle is the wardle sample aggregated API (flunders), which the e2e
// cluster runs as a directly-served aggregated API. So one flunder create yields
// the two views the corpus puts side by side: the official audit (/audit-webhook,
// empty body) and the live watch (full object). Both are load-bearing for Row 15 —
// the empty-audit-vs-full-watch contrast is the point — so the driver waits for
// and requires each.

var flunderGVR = schema.GroupVersionResource{
	Group: "wardle.example.com", Version: "v1alpha1", Resource: "flunders",
}

// TestAggregatedAPIWrite captures Row 15. It creates a flunder and proves the
// watch carries the full object (spec included), then commits the official audit
// (empty body) and the watch side by side so the body-quality difference is
// visible in the corpus. Both are required — the corpus row's whole point is the
// empty-audit-vs-full-watch contrast — so a missing event fails the scenario
// rather than silently dropping a corpus file.
func TestAggregatedAPIWrite(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.newScenario(ctx, t, "aggregated-api-write")

	flunder := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "wardle.example.com/v1alpha1",
		"kind":       "Flunder",
		"metadata": map[string]any{
			"name":      "fl-1",
			"namespace": s.ns,
			"labels":    map[string]any{scenarioLabel: s.id},
		},
		"spec": map[string]any{"referenceType": "Flunder", "reference": "some-flunder"},
	}}
	if _, err := h.dyn.Resource(flunderGVR).Namespace(s.ns).Create(ctx, flunder, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create flunder: %v", err)
	}

	// Wait for the flunder's watch ADDED and its official audit — both are committed
	// side by side. Union the namespace key because a shallow-bodied aggregated-API
	// audit event carries no object label to attribute by — but then select strictly
	// by flunder identity, because that union also surfaces the namespace's unlabeled
	// auto-objects (kube-root-ca.crt, the default ServiceAccount), which must NOT be
	// mistaken for the flunder's records.
	records := h.drain(t, s.id, drainSpec{
		minCount: 2, settle: 5 * time.Second, timeout: 90 * time.Second, alsoNamespace: s.ns,
		until: func(rs []mutationlab.Record) bool {
			return flunderRecord(rs, mutationlab.SourceWatch, "ADDED") != nil &&
				flunderRecord(rs, mutationlab.SourceAudit, "") != nil
		},
	})

	added := flunderRecord(records, mutationlab.SourceWatch, "ADDED")
	official := flunderRecord(records, mutationlab.SourceAudit, "")
	admission := flunderRecord(records, mutationlab.SourceAdmission, "")
	if added == nil {
		t.Fatal("no watch ADDED for the flunder; the aggregated-API watch did not carry it")
	}
	if official == nil {
		t.Fatal("no official audit event for the flunder create")
	}

	// THE RESULT: the watch event carries the full object (spec included). This is
	// the point of Row 15 — whatever the official audit body quality, the live
	// watch carries the object content.
	if !added.Summary.HasObject || flunderReference(added) != "some-flunder" {
		t.Errorf("watch ADDED did not carry the full flunder object (spec.reference=%q, hasObject=%v)",
			flunderReference(added), added.Summary.HasObject)
	}
	t.Logf("Row 15 (flunder only): official audit hasRequestObject=%v hasResponseObject=%v; "+
		"watch carries full object=%v; flunder admission records=%v",
		official.Summary.HasRequestObject, official.Summary.HasResponseObject,
		added.Summary.HasObject, admission != nil)

	h.syncCorpus(t, "flunder/aggregated-api-write",
		[]mutationlab.Record{*official, *added})
}

// flunderRecord returns the first record from the given source that is about the
// flunder (by objectRef resource or object name), optionally restricted to a
// watch type. This isolates the flunder from the namespace's auto-created objects
// that the namespace-union read also surfaces.
func flunderRecord(records []mutationlab.Record, src mutationlab.Source, watchType string) *mutationlab.Record {
	for i := range records {
		r := &records[i]
		if r.Source != src {
			continue
		}
		if watchType != "" && r.Summary.WatchType != watchType {
			continue
		}
		if r.Key.Resource == "flunders" || r.Key.Name == "fl-1" {
			return r
		}
	}
	return nil
}

// flunderReference extracts spec.reference from a watch record's object, the
// field that proves the watch carried the full object rather than a shell.
func flunderReference(r *mutationlab.Record) string {
	var env struct {
		Object struct {
			Spec struct {
				Reference string `json:"reference"`
			} `json:"spec"`
		} `json:"object"`
	}
	if err := json.Unmarshal(r.Raw, &env); err != nil {
		return ""
	}
	return env.Object.Spec.Reference
}

// TestAggregatedAPIDelete is the delete half of Row 15, and it exists because the create half
// raised a question it could not answer: an aggregated-API write is audited with an EMPTY body, so
// what does an aggregated-API DELETE look like — is it audited at all, does its objectRef carry a
// uid, and does the watch DELETED carry the object?
//
// The answer decides whether a removal of an aggregated type can be attributed to its deleter at
// all. The join needs either a uid on the fact (which the exact and latest tiers key on) or a
// collection fact covering it. If a proxied delete is audited with no uid, per-object attribution
// for aggregated types is structurally impossible and only the collection tiers can ever work.
func TestAggregatedAPIDelete(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.newScenario(ctx, t, "aggregated-api-delete")

	flunder := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "wardle.example.com/v1alpha1",
		"kind":       "Flunder",
		"metadata": map[string]any{
			"name":      "fl-del",
			"namespace": s.ns,
			"labels":    map[string]any{scenarioLabel: s.id},
		},
		"spec": map[string]any{"referenceType": "Flunder", "reference": "doomed"},
	}}
	if _, err := h.dyn.Resource(flunderGVR).Namespace(s.ns).Create(ctx, flunder, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create flunder: %v", err)
	}
	// The create's own records are setup, not the finding. Union the namespace because an
	// aggregated audit event carries no object label to attribute by.
	h.drain(t, s.id, drainSpec{
		minCount: 1, settle: 3 * time.Second, timeout: 90 * time.Second, alsoNamespace: s.ns,
		until: func(rs []mutationlab.Record) bool {
			return flunderRecordNamed(rs, mutationlab.SourceWatch, "ADDED", "fl-del") != nil
		},
	})
	h.clearRecords(t)

	if err := h.dyn.Resource(flunderGVR).Namespace(s.ns).
		Delete(ctx, "fl-del", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete flunder: %v", err)
	}

	records := h.drain(t, s.id, drainSpec{
		minCount: 1, settle: 5 * time.Second, timeout: 90 * time.Second, alsoNamespace: s.ns,
		until: func(rs []mutationlab.Record) bool {
			return flunderRecordNamed(rs, mutationlab.SourceWatch, "DELETED", "fl-del") != nil
		},
	})

	deleted := flunderRecordNamed(records, mutationlab.SourceWatch, "DELETED", "fl-del")
	audit := flunderRecordNamed(records, mutationlab.SourceAudit, "", "fl-del")
	if deleted == nil {
		t.Fatal("no watch DELETED for the flunder; the aggregated-API watch did not carry the removal")
	}

	// THE FINDING, whichever way it goes. A delete that is audited WITH a uid can be attributed
	// per object; audited WITHOUT one can only ever be reached by the collection tiers; not
	// audited at all can never be attributed, exactly like a type the policy excludes.
	switch {
	case audit == nil:
		t.Logf("FINDING: an aggregated-API delete produced NO audit record at all. Per-object "+
			"attribution of %s removals is impossible; every one ships committer-authored.",
			flunderGVR.Resource)
	case audit.Key.UID == "":
		t.Logf("FINDING: the aggregated-API delete IS audited (verb=%q) but its objectRef carries "+
			"no uid, so the exact and latest tiers can never match it. Only a collection fact can "+
			"attribute an aggregated removal.", audit.Summary.Operation)
	default:
		t.Logf("FINDING: the aggregated-API delete is audited with uid %q, so per-object "+
			"attribution works for aggregated types after all.", audit.Key.UID)
	}
	t.Logf("watch DELETED carries object=%v; audit present=%v", deleted.Summary.HasObject, audit != nil)

	commit := []mutationlab.Record{*deleted}
	if audit != nil {
		commit = append([]mutationlab.Record{*audit}, commit...)
	}
	h.syncCorpus(t, "flunder/aggregated-api-delete", commit)
}

// TestAggregatedAPIDeletecollection is the case the whole collection-fact design turns on for
// aggregated types, and the one a hand-written fixture cannot settle.
//
// The kube-apiserver PROXIES the request to the extension server and never decodes the response it
// streamed back, so the expectation is an audited deletecollection with NO response body: no uid
// set, and therefore a join that can only proceed by SCOPE — type, namespace, selector, window.
// That is exactly the case the deleted response-body expander produced nothing for.
//
// If the body IS present, the join upgrades itself to uid membership and the scope tier is not
// needed here. Either result is worth having in the corpus; the point is to stop inferring it.
func TestAggregatedAPIDeletecollection(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.newScenario(ctx, t, "aggregated-api-deletecollection")

	names := []string{"fl-dc-a", "fl-dc-b"}
	for _, name := range names {
		flunder := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "wardle.example.com/v1alpha1",
			"kind":       "Flunder",
			"metadata": map[string]any{
				"name":      name,
				"namespace": s.ns,
				"labels":    map[string]any{scenarioLabel: s.id},
			},
			"spec": map[string]any{"referenceType": "Flunder", "reference": "doomed"},
		}}
		if _, err := h.dyn.Resource(flunderGVR).Namespace(s.ns).
			Create(ctx, flunder, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	h.drain(t, s.id, drainSpec{
		minCount: len(names), settle: 3 * time.Second, timeout: 90 * time.Second, alsoNamespace: s.ns,
		until: func(rs []mutationlab.Record) bool {
			return flunderRecordNamed(rs, mutationlab.SourceWatch, "ADDED", names[len(names)-1]) != nil
		},
	})
	h.clearRecords(t)

	selector := metav1.ListOptions{LabelSelector: scenarioLabel + "=" + s.id}
	if err := h.dyn.Resource(flunderGVR).Namespace(s.ns).
		DeleteCollection(ctx, metav1.DeleteOptions{}, selector); err != nil {
		t.Fatalf("deletecollection flunders: %v", err)
	}

	records := h.drain(t, s.id, drainSpec{
		minCount: len(names), settle: 5 * time.Second, timeout: 90 * time.Second, alsoNamespace: s.ns,
		until: func(rs []mutationlab.Record) bool {
			return flunderRecordNamed(rs, mutationlab.SourceWatch, "DELETED", names[len(names)-1]) != nil
		},
	})

	// The asymmetry the collection fact exists for: N watch removals against ONE audit event.
	watches := 0
	for i := range records {
		r := &records[i]
		if r.Source == mutationlab.SourceWatch && r.Summary.WatchType == "DELETED" && r.Key.Resource == "flunders" {
			watches++
		}
	}
	if watches != len(names) {
		t.Errorf("aggregated deletecollection produced %d watch DELETEDs; want %d (per-object fan-out)",
			watches, len(names))
	}

	audit := flunderRecordNamed(records, mutationlab.SourceAudit, "", "")
	switch {
	case audit == nil:
		t.Log("FINDING: an aggregated deletecollection produced NO audit record. Nothing can " +
			"attribute these removals; every one ships committer-authored.")
	case !audit.Summary.HasResponseObject:
		t.Log("FINDING: the aggregated deletecollection IS audited with NO response body, so the " +
			"fact carries no uid set and the join must fall back to SCOPE matching. This is the " +
			"case the deleted response-body expander produced nothing at all for.")
	default:
		t.Log("FINDING: the aggregated deletecollection returned a response body, so the fact " +
			"carries the uid set and the join resolves by uid membership.")
	}
	t.Logf("%d watch DELETEDs against %d audit record(s)", watches, countSource(records, mutationlab.SourceAudit))

	h.syncCorpus(t, "flunder/aggregated-api-deletecollection", records)
}

// flunderRecordNamed is flunderRecord with an optional name filter, so a scenario that creates more
// than one flunder can pick out the one it means. An empty name matches any flunder record.
func flunderRecordNamed(
	records []mutationlab.Record,
	src mutationlab.Source,
	watchType, name string,
) *mutationlab.Record {
	for i := range records {
		r := &records[i]
		if r.Source != src {
			continue
		}
		if watchType != "" && r.Summary.WatchType != watchType {
			continue
		}
		if r.Key.Resource != "flunders" {
			continue
		}
		if name != "" && r.Key.Name != name {
			continue
		}
		return r
	}
	return nil
}
